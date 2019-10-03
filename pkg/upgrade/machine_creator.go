// Copyright 2019 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type podGetter interface {
	Get(string, metav1.GetOptions) (*v1.Pod, error)
}

type nodeLister interface {
	List(options metav1.ListOptions) (*v1.NodeList, error)
}

// MachineOptions are the values that can change on a machine upgrade
type MachineOptions struct {
	ImageID        string
	ImageField     string
	DesiredVersion semver.Version
}

// MachineCreator is responsible for creating a new machine.
// It can optionally wait for the associated node to become ready.
type MachineCreator struct {
	shouldWaitForProviderID   bool
	shouldWaitForMatchingNode bool
	shouldWaitForNodeReady    bool
	MachineOptions            MachineOptions
	managementClient          ctrlclient.Client

	providerIDTimeout   time.Duration
	matchingNodeTimeout time.Duration
	nodeReadyTimeout    time.Duration
	podGetter           podGetter
	nodeLister          nodeLister
	log                 logr.Logger
}

// NewMachineCreator takes a list of option functions to configure the MachineCreator.
func NewMachineCreator(options ...MachineCreatorOption) *MachineCreator {
	creator := &MachineCreator{
		providerIDTimeout:         15 * time.Minute,
		matchingNodeTimeout:       10 * time.Minute,
		nodeReadyTimeout:          15 * time.Minute,
		shouldWaitForMatchingNode: true,
		shouldWaitForProviderID:   true,
		shouldWaitForNodeReady:    true,
	}
	for _, fn := range options {
		fn(creator)
	}
	return creator
}

// NewMachine is the main interface to MachineCreator.
// It creates a machine object on the management cluster and optionally waits for the backing node to become ready.
func (n *MachineCreator) NewMachine(namespace, name string, source *clusterv1.Machine) (*clusterv1.Machine, *v1.Node, error) {
	newMachine := source.DeepCopy()

	// have to clear this out so we can create a new machine
	newMachine.ResourceVersion = ""

	// have to clear this out so the new machine can get its own provider id set
	newMachine.Spec.ProviderID = nil

	newMachine.Name = name

	if n.MachineOptions.ImageField != "" && n.MachineOptions.ImageID != "" {
		if err := updateMachineSpecImage(&newMachine.Spec, n.MachineOptions.ImageField, n.MachineOptions.ImageID); err != nil {
			return nil, nil, err
		}
	}

	desiredVersion := n.MachineOptions.DesiredVersion.String()
	newMachine.Spec.Version = &desiredVersion

	n.log.Info("Creating new machine", "name", newMachine.Name)

	if err := n.managementClient.Create(context.TODO(), newMachine); err != nil {
		return nil, nil, errors.Wrapf(err, "Error creating machine: %s", newMachine.Name)
	}

	if n.shouldWaitForProviderID {
		providerID, err := n.waitForProviderID(newMachine.Namespace, newMachine.Name, n.providerIDTimeout)
		if err != nil {
			return nil, nil, err
		}
		if n.shouldWaitForMatchingNode {
			node, err := n.waitForMatchingNode(providerID, n.matchingNodeTimeout)
			if err != nil {
				return nil, nil, err
			}
			if n.shouldWaitForNodeReady {
				if err := n.waitForNodeReady(node, n.nodeReadyTimeout); err != nil {
					return nil, nil, err
				}
				// return ready node (could delete but here for explicitness)
				return newMachine, node, nil
			}
			// return an unready node
			return newMachine, node, nil
		}
		// return the created machine with provider ID and no node since we are not waiting for the node
		return newMachine, nil, nil
	}
	// return the created machine without a provider ID and no node since we waited for nothing
	return newMachine, nil, nil
}

func (n *MachineCreator) waitForProviderID(ns, name string, timeout time.Duration) (string, error) {
	n.log.Info("waitForMachineProviderID start", "namespace", ns, "name", name)
	var providerID string
	err := wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {

		machine := &clusterv1.Machine{}
		if err := n.managementClient.Get(context.TODO(), ctrlclient.ObjectKey{Name: name, Namespace: ns}, machine); err != nil {
			return false, errors.WithStack(err)
		}

		if machine.Spec.ProviderID == nil {
			return false, nil
		}

		providerID = *machine.Spec.ProviderID
		if providerID != "" {
			n.log.Info("Got providerID", "provider-id", providerID)
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		return "", errors.Wrap(err, "timed out waiting for machine provider id")
	}
	return providerID, nil
}

func (n *MachineCreator) waitForMatchingNode(rawProviderID string, timeout time.Duration) (*v1.Node, error) {
	n.log.Info("Waiting for node", "provider-id", rawProviderID)
	var matchingNode v1.Node
	providerID, err := noderefutil.NewProviderID(rawProviderID)
	if err != nil {
		return nil, err
	}

	err = wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		nodes, err := n.nodeLister.List(metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, node := range nodes.Items {
			// TODO(chuckha) Update to use noderefutil.Equals when we use a more recent cluster-api
			nodeID, err := noderefutil.NewProviderID(node.Spec.ProviderID)
			if err != nil {
				return false, err
			}
			if providerID.Equals(nodeID) {
				n.log.Info("Found node", "name", node.Name)
				matchingNode = node
				return true, nil
			}
		}

		return false, nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "timed out waiting for matching node")
	}
	return &matchingNode, nil
}

func (n *MachineCreator) waitForNodeReady(newNode *v1.Node, timeout time.Duration) error {
	// wait for NodeReady
	nodeHostname := hostnameForNode(newNode)
	if nodeHostname == "" {
		n.log.Info("unable to find hostname for node", "node", newNode.Name)
		return errors.Errorf("unable to find hostname for node %s", newNode.Name)
	}
	err := wait.PollImmediate(15*time.Second, timeout, func() (bool, error) {
		ready := n.isReady(nodeHostname)
		return ready, nil
	})
	if err != nil {
		return errors.Wrapf(err, "components on node %s are not ready", newNode.Name)
	}
	return nil
}

func (n *MachineCreator) isReady(nodeHostname string) bool {
	n.log.Info("Component health check for node", "hostname", nodeHostname)

	components := []string{"etcd", "kube-apiserver", "kube-scheduler", "kube-controller-manager"}
	requiredConditions := sets.NewString("PodScheduled", "Initialized", "Ready", "ContainersReady")

	for _, component := range components {
		foundConditions := sets.NewString()

		podName := fmt.Sprintf("%s-%v", component, nodeHostname)
		log := n.log.WithValues("pod", podName)

		log.Info("Getting pod")
		pod, err := n.podGetter.Get(podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			log.Info("Pod not found yet")
			return false
		} else if err != nil {
			log.Error(err, "error getting pod")
			return false
		}

		for _, condition := range pod.Status.Conditions {
			if condition.Status == "True" {
				foundConditions.Insert(string(condition.Type))
			}
		}

		missingConditions := requiredConditions.Difference(foundConditions)
		if missingConditions.Len() > 0 {
			missingDescription := strings.Join(missingConditions.List(), ",")
			log.Info("pod is missing some required conditions", "conditions", missingDescription)
			return false
		}
	}

	return true
}

// MachineCreatorOptions are the functional options for the MachineCreator.
type MachineCreatorOption func(*MachineCreator)

// ShouldWaitForProviderID allows the MachineCreator to skip waiting for the ProviderID to appear on the Machine.
func ShouldWaitForProviderID(should bool) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.shouldWaitForProviderID = should
	}
}

// ShouldWaitForMatchingNode allows the MachineCreator to skip waiting for the backing node to a machine to appearn
func ShouldWaitForMatchingNode(should bool) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.shouldWaitForMatchingNode = should
	}
}

// ShouldWaitForNodeReady allows the MachineCreator to skip waiting for the backing node to become ready.
func ShouldWaitForNodeReady(should bool) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.shouldWaitForNodeReady = should
	}
}

func WithPodGetter(pg podGetter) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.podGetter = pg
	}
}
func WithNodeLister(nl nodeLister) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.nodeLister = nl
	}
}
func WithMachineOptions(mo MachineOptions) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.MachineOptions = mo
	}
}
func WithLogger(l logr.Logger) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.log = l
	}
}
func WithProviderIDTimeout(timeout time.Duration) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.providerIDTimeout = timeout
	}
}
func WithMatchingNodeTimeout(timeout time.Duration) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.matchingNodeTimeout = timeout
	}
}
func WithNodeReadyTimeout(timeout time.Duration) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.nodeReadyTimeout = timeout
	}
}
func WithManagementClient(c ctrlclient.Client) MachineCreatorOption {
	return func(n *MachineCreator) {
		n.managementClient = c
	}
}

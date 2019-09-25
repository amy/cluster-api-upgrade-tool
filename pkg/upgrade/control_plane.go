// Copyright 2019 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/blang/semver"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/vmware/cluster-api-upgrade-tool/pkg/internal/kubernetes"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterapiv1alpha2 "sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	etcdCACertFile = "/etc/kubernetes/pki/etcd/ca.crt"
	etcdCertFile   = "/etc/kubernetes/pki/etcd/peer.crt"
	etcdKeyFile    = "/etc/kubernetes/pki/etcd/peer.key"
	KindAWSMachine = "AWSMachine"

	// UpgradeIDAnnotationKey is the annotation key for this tool's upgrade-id
	UpgradeIDAnnotationKey = "upgrade-id"
)

type ControlPlaneUpgrader struct {
	*base
	oldNodeToEtcdMember map[string]string
}

func NewControlPlaneUpgrader(log logr.Logger, config Config) (*ControlPlaneUpgrader, error) {
	b, err := newBase(log, config)
	if err != nil {
		return nil, errors.Wrap(err, "error initializing upgrader")
	}

	return &ControlPlaneUpgrader{
		base: b,
	}, nil
}

// Upgrade does the upgrading of the control plane.
func (u *ControlPlaneUpgrader) Upgrade() error {
	machines, err := u.listMachines()
	if err != nil {
		return err
	}

	if machines == nil || len(machines.Items) == 0 {
		return errors.New("Found 0 control plane machines")
	}

	min, max, err := u.minMaxControlPlaneVersions(machines)
	if err != nil {
		return errors.Wrap(err, "error determining current control plane versions")
	}

	// default the desired version if the user did not specify it
	if unsetVersion.EQ(u.userVersion) {
		u.desiredVersion = max
	}

	if isMinorVersionUpgrade(min, u.desiredVersion) {
		err = u.updateKubeletConfigMapIfNeeded(u.desiredVersion)
		if err != nil {
			return err
		}

		err = u.updateKubeletRbacIfNeeded(u.desiredVersion)
		if err != nil {
			return err
		}
	}

	if err := u.etcdClusterHealthCheck(time.Minute * 1); err != nil {
		return err
	}

	if err := u.UpdateProviderIDsToNodes(); err != nil {
		return err
	}

	if err := u.updateAndUploadKubeadmKubernetesVersion(); err != nil {
		return err
	}

	return u.updateCRDs(machines)
}

func isMinorVersionUpgrade(base, update semver.Version) bool {
	return base.Major == update.Major && base.Minor < update.Minor
}

func (u *ControlPlaneUpgrader) minMaxControlPlaneVersions(machines *clusterapiv1alpha2.MachineList) (semver.Version, semver.Version, error) {
	var min, max semver.Version

	for _, machine := range machines.Items {
		if *machine.Spec.Version != "" {
			machineVersion, err := semver.ParseTolerant(*machine.Spec.Version)
			if err != nil {
				return min, max, errors.Wrapf(err, "invalid control plane version %q for machine %s/%s", *machine.Spec.Version, machine.Namespace, machine.Name)
			}
			if min.EQ(unsetVersion) || machineVersion.LT(min) {
				min = machineVersion
			}
			if max.EQ(unsetVersion) || machineVersion.GT(max) {
				max = machineVersion
			}
		}
	}

	return min, max, nil
}

func (u *ControlPlaneUpgrader) updateKubeletConfigMapIfNeeded(version semver.Version) error {
	// Check if the desired configmap already exists
	desiredKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor)
	_, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get(desiredKubeletConfigMapName, metav1.GetOptions{})
	if err == nil {
		u.log.Info("kubelet configmap already exists", "configMapName", desiredKubeletConfigMapName)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "error determining if configmap %s exists", desiredKubeletConfigMapName)
	}

	// If we get here, we have to make the configmap
	previousMinorVersionKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor-1)
	cm, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get(previousMinorVersionKubeletConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errors.Errorf("unable to find current kubelet configmap %s", previousMinorVersionKubeletConfigMapName)
	}
	cm.Name = desiredKubeletConfigMapName
	cm.ResourceVersion = ""

	_, err = u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Create(cm)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "error creating configmap %s", desiredKubeletConfigMapName)
	}

	return nil
}

func (u *ControlPlaneUpgrader) updateKubeletRbacIfNeeded(version semver.Version) error {
	majorMinor := fmt.Sprintf("%d.%d", version.Major, version.Minor)
	roleName := fmt.Sprintf("kubeadm:kubelet-config-%s", majorMinor)

	_, err := u.targetKubernetesClient.RbacV1().Roles("kube-system").Get(roleName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		newRole := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "kube-system",
				Name:      roleName,
			},
			Rules: []rbacv1.PolicyRule{
				{
					Verbs:         []string{"get"},
					APIGroups:     []string{""},
					Resources:     []string{"configmaps"},
					ResourceNames: []string{fmt.Sprintf("kubelet-config-%s", majorMinor)},
				},
			},
		}

		_, err := u.targetKubernetesClient.RbacV1().Roles("kube-system").Create(newRole)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating role %s", roleName)
		}
	} else if err != nil {
		return errors.Wrapf(err, "error determining if role %s exists", roleName)
	}

	_, err = u.targetKubernetesClient.RbacV1().RoleBindings("kube-system").Get(roleName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		newRoleBinding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "kube-system",
				Name:      roleName,
			},
			Subjects: []rbacv1.Subject{
				{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "Group",
					Name:     "system:nodes",
				},
				{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "Group",
					Name:     "system:bootstrappers:kubeadm:default-node-token",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     roleName,
			},
		}

		_, err = u.targetKubernetesClient.RbacV1().RoleBindings("kube-system").Create(newRoleBinding)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating rolebinding %s", roleName)
		}
	} else if err != nil {
		return errors.Wrapf(err, "error determining if rolebinding %s exists", roleName)
	}

	return nil
}

func (u *ControlPlaneUpgrader) etcdClusterHealthCheck(timeout time.Duration) error {
	members, err := u.listEtcdMembers(timeout)
	if err != nil {
		return err
	}

	var endpoints []string
	for _, member := range members {
		endpoints = append(endpoints, member.ClientURLs...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// TODO: we can switch back to using --cluster instead of --endpoints when we no longer need to support etcd 3.2
	// (which is the version kubeadm installs for Kubernetes v1.13.x). kubeadm switched to etcd 3.3 with v1.14.x.

	// TODO: use '-w json' when it's in the minimum supported etcd version.
	_, _, err = u.etcdctl(ctx, "endpoint health --endpoints", strings.Join(endpoints, ","))
	return err
}

func (u *ControlPlaneUpgrader) updateObjectReference(name string, ref *v1.ObjectReference) (*v1.ObjectReference, *unstructured.Unstructured, error) {

	if ref.Namespace == "" {
		ref.Namespace = "default"
	}
	object, err := external.Get(u.ctrlClient, ref, ref.Namespace)
	if err != nil {
		return &v1.ObjectReference{}, nil, err
	}

	object.SetResourceVersion("")
	object.SetName(name)
	object.SetOwnerReferences([]metav1.OwnerReference{})
	unstructured.RemoveNestedField(object.UnstructuredContent(), "spec", "providerID")

	ref.ResourceVersion = ""
	ref.Name = name

	return ref, object, nil
}

func (u *ControlPlaneUpgrader) updateMachine(name string, machine clusterapiv1alpha2.Machine, machineCreator *MachineCreator) error {
	originalProviderID, err := noderefutil.NewProviderID(*machine.Spec.ProviderID)
	if err != nil {
		return err
	}

	oldNode := u.GetNodeFromProviderID(originalProviderID.ID())
	if oldNode == nil {
		u.log.Info("Couldn't retrieve oldNode", "id", originalProviderID.String())
		return fmt.Errorf("unknown previous node %q", originalProviderID.String())
	}

	oldHostName := hostnameForNode(oldNode)

	newMachine, node, err := machineCreator.NewMachine(name, &machine)
	if err != nil {
		return err
	}
	nodeHostname := hostnameForNode(node)

	// This used to happen when a new machine was created as a side effect. Must still update the mapping.
	if err := u.UpdateProviderIDsToNodes(); err != nil {
		return err
	}

	// delete old etcd member
	err = u.deleteEtcdMember(time.Minute*1, nodeHostname, u.oldNodeToEtcdMember[oldHostName])
	if err != nil {
		return errors.Wrapf(err, "unable to delete old etcd member %s", u.oldNodeToEtcdMember[oldHostName])
	}

	if err := u.deleteMachine(&machine); err != nil {
		return err
	}

	if err := u.applyAnnotation(newMachine); err != nil {
		return err
	}
	return nil
}

func (u *ControlPlaneUpgrader) updateCRDs(machines *clusterapiv1alpha2.MachineList) error {
	// save all etcd member id corresponding to node before upgrade starts
	err := u.oldNodeToEtcdMemberId(time.Minute * 1)
	if err != nil {
		return err
	}

	mo := MachineOptions{
		ImageID:        u.imageID,
		ImageField:     u.imageField,
		DesiredVersion: u.desiredVersion,
	}

	machineCreator := NewMachineCreator(
		WithControllerRuntimeClient(u.ctrlClient),
		WithMachineGetter(u.machineGetter),
		WithNamespace(u.clusterNamespace),
		WithNodeLister(u.targetKubernetesClient.CoreV1().Nodes()),
		WithPodGetter(u.targetKubernetesClient.CoreV1().Pods("kube-system")),
		WithMachineOptions(mo),
		WithLogger(u.log.WithName("machine-creator")),
	)

	// TODO add more error logs on failure conditions
	for _, machine := range machines.Items {
		annotations := machine.GetAnnotations()
		// Skip any machine that already has the annotation we're looking for
		if val, ok := annotations[UpgradeIDAnnotationKey]; ok && val == u.upgradeID {
			continue
		}

		if machine.Spec.ProviderID == nil {
			u.log.Info("unable to upgrade machine as it has no spec.providerID", "name", machine.Name)
			continue
		}

		// assume the original name is controlplane-<index> or controlplane-<index>-<timestamp>
		// let's set the new name to controlplane-<index>-<timestamp>
		nameParts := strings.Split(machine.Name, "-")
		if len(nameParts) < 2 {
			return errors.Errorf("machine name %q does not match expected format <name>-<index> or <name>-<index>-<timestamp>", machine.Name)
		}
		name := fmt.Sprintf("%s-%s-%d", nameParts[0], nameParts[1], time.Now().Unix())
		// TODO: generate the name based off each respective object

		infraMachine, object, err := u.updateObjectReference(name, &machine.Spec.InfrastructureRef)
		if err != nil {
			return err
		}
		err = u.ctrlClient.Create(context.TODO(), object)
		if err != nil {
			return err
		}
		machine.Spec.InfrastructureRef = *infraMachine

		bootstrap, object, err := u.updateObjectReference(name, machine.Spec.Bootstrap.ConfigRef)
		if err != nil {
			return err
		}
		machine.Spec.Bootstrap.ConfigRef = bootstrap

		if err := u.updateMachine(name, machine, machineCreator); err != nil {
			return err
		}
	}

	return nil
}

func (u *ControlPlaneUpgrader) applyAnnotation(m *clusterapiv1alpha2.Machine) error {
	original := m.DeepCopy()
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[UpgradeIDAnnotationKey] = u.upgradeID
	updated := m.DeepCopy()

	// TODO: double check this patch strategy
	u.ctrlClient.Patch(context.TODO(), updated, ctrlclient.MergeFrom(original))

	return nil
}

// retry the given function for the given number of times with the given interval
func (u *ControlPlaneUpgrader) retry(node *v1.Node, count int, interval time.Duration, fn func(hp *v1.Node) error) error {
	if err := fn(node); err != nil {
		if count--; count > 0 {
			time.Sleep(interval)
			return u.retry(node, count, interval, fn)
		}

		return err
	}

	return nil
}

func (u *ControlPlaneUpgrader) deleteMachine(machine *clusterapiv1alpha2.Machine) error {
	u.log.Info("Deleting existing machine", "namespace", machine.Namespace, "name", machine.Name)

	err := u.ctrlClient.Delete(context.TODO(), machine, ctrlclient.PropagationPolicy(metav1.DeletePropagationForeground))
	return errors.WithStack(err)
}

func hostnameForNode(node *v1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == v1.NodeHostName {
			return address.Address
		}
	}
	return ""
}

// Split this into getting machines
// Then pulling provider IDs
func (u *ControlPlaneUpgrader) listMachines() (*clusterapiv1alpha2.MachineList, error) {

	labels := ctrlclient.MatchingLabels{
		"cluster.x-k8s.io/cluster-name":  u.clusterName,
		"cluster.x-k8s.io/control-plane": "true",
	}
	selectors := []ctrlclient.ListOption{
		labels,
		ctrlclient.InNamespace(u.clusterNamespace),
	}
	machines := &clusterapiv1alpha2.MachineList{}

	u.log.Info("Listing machines", "labelSelector", labels)
	err := u.ctrlClient.List(context.TODO(), machines, selectors...)
	if err != nil {
		return nil, errors.Wrap(err, "error listing machines")
	}

	return machines, nil
}

type etcdMembersResponse struct {
	Members []etcdMember `json:"members"`
}

type etcdMember struct {
	ID         uint64   `json:"ID"`
	Name       string   `json:"name"`
	ClientURLs []string `json:"clientURLs"`
}

func (u *ControlPlaneUpgrader) listEtcdMembers(timeout time.Duration) ([]etcdMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stdout, _, err := u.etcdctl(ctx, "member list -w json")
	if err != nil {
		return []etcdMember{}, err
	}

	var resp etcdMembersResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		return []etcdMember{}, errors.Wrap(err, "unable to parse etcdctl member list json output")
	}

	return resp.Members, nil
}

func (u *ControlPlaneUpgrader) oldNodeToEtcdMemberId(timeout time.Duration) error {
	members, err := u.listEtcdMembers(timeout)
	if err != nil {
		return err
	}

	m := make(map[string]string)
	for _, member := range members {
		// etcd expects member IDs in hex, so convert to base 16
		id := strconv.FormatUint(member.ID, 16)
		m[member.Name] = id
	}

	u.oldNodeToEtcdMember = m

	return nil
}

// deleteEtcdMember deletes the old etcd member
func (u *ControlPlaneUpgrader) deleteEtcdMember(timeout time.Duration, newNode string, etcdMemberId string) error {
	u.log.Info("deleteEtcdMember")
	pods, err := u.listEtcdPods()
	if err != nil {
		return err
	}

	if len(pods) == 0 {
		return errors.New("found 0 etcd pods")
	}

	var pod *v1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == newNode {
			pod = p
			break
		}
	}

	if pod == nil {
		return errors.New("no new etcd pod found running on node" + newNode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, _, err = u.etcdctlForPod(ctx, pod, "member", "remove", etcdMemberId)
	return err
}

func (u *ControlPlaneUpgrader) listEtcdPods() ([]v1.Pod, error) {
	// get pods in kube-system with label component=etcd
	list, err := u.targetKubernetesClient.CoreV1().Pods("kube-system").List(metav1.ListOptions{LabelSelector: "component=etcd"})
	if err != nil {
		return []v1.Pod{}, errors.Wrap(err, "error listing pods")
	}
	return list.Items, nil
}

func (u *ControlPlaneUpgrader) etcdctl(ctx context.Context, args ...string) (string, string, error) {
	pods, err := u.listEtcdPods()
	if err != nil {
		return "", "", err
	}
	if len(pods) == 0 {
		return "", "", errors.New("found 0 etcd pods")
	}

	// get the first one
	firstPod := pods[0]

	return u.etcdctlForPod(ctx, &firstPod, args...)
}

func (u *ControlPlaneUpgrader) etcdctlForPod(ctx context.Context, pod *v1.Pod, args ...string) (string, string, error) {
	endpoint := fmt.Sprintf("https://%s:2379", pod.Status.PodIP)

	fullArgs := []string{
		"ETCDCTL_API=3",
		"etcdctl",
		"--cacert", etcdCACertFile,
		"--cert", etcdCertFile,
		"--key", etcdKeyFile,
		"--endpoints", endpoint,
	}

	fullArgs = append(fullArgs, args...)

	opts := kubernetes.PodExecInput{
		RestConfig:       u.targetRestConfig,
		KubernetesClient: u.targetKubernetesClient,
		Namespace:        pod.Namespace,
		Name:             pod.Name,
		Command: []string{
			"sh",
			"-c",
			strings.Join(fullArgs, " "),
		},
	}

	opts.Command = append(opts.Command, args...)

	stdout, stderr, err := kubernetes.PodExec(ctx, opts)

	// TODO figure out how we want logs to show up in this library
	u.log.Info(fmt.Sprintf("etcdctl stdout: %s", stdout))
	u.log.Info(fmt.Sprintf("etcdctl stderr: %s", stderr))

	return stdout, stderr, err
}

func (u *ControlPlaneUpgrader) updateAndUploadKubeadmKubernetesVersion() error {
	original, err := u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Get("kubeadm-config", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error getting kubeadm configmap from target cluster")
	}

	updated, err := updateKubeadmKubernetesVersion(original, "v"+u.desiredVersion.String())
	if err != nil {
		return err
	}

	if _, err = u.targetKubernetesClient.CoreV1().ConfigMaps("kube-system").Update(updated); err != nil {
		return errors.Wrap(err, "error updating kubeadm configmap")
	}

	return nil
}

func updateKubeadmKubernetesVersion(original *v1.ConfigMap, version string) (*v1.ConfigMap, error) {
	cm := original.DeepCopy()

	clusterConfig := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &clusterConfig); err != nil {
		return nil, errors.Wrap(err, "error decoding kubeadm configmap ClusterConfiguration")
	}

	clusterConfig["kubernetesVersion"] = version

	updated, err := yaml.Marshal(clusterConfig)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding kubeadm configmap ClusterConfiguration")
	}

	cm.Data["ClusterConfiguration"] = string(updated)

	return cm, nil
}

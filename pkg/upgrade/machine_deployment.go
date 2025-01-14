// Copyright 2019 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	clusterapiv1alpha2 "sigs.k8s.io/cluster-api/api/v1alpha2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type MachineDeploymentUpgrader struct {
	*base
}

func NewMachineDeploymentUpgrader(log logr.Logger, config Config) (*MachineDeploymentUpgrader, error) {
	b, err := newBase(log, config)
	if err != nil {
		return nil, errors.Wrap(err, "error initializing upgrader")
	}

	return &MachineDeploymentUpgrader{
		base: b,
	}, nil
}

func (u *MachineDeploymentUpgrader) Upgrade() error {
	machineDeployments, err := u.listMachineDeployments()
	if err != nil {
		return err
	}

	if machineDeployments == nil || len(machineDeployments.Items) == 0 {
		return errors.New("Found 0 machine deployments")
	}

	return u.upgradeMachineDeployments(machineDeployments)
}

func (u *MachineDeploymentUpgrader) listMachineDeployments() (*clusterapiv1alpha2.MachineDeploymentList, error) {
	u.log.Info("Listing machine deployments")

	selectors := []ctrlclient.ListOption{
		ctrlclient.MatchingLabels{
			"cluster.k8s.io/cluster-name": u.clusterName,
			"set":                         "node",
		},
		ctrlclient.InNamespace(u.clusterNamespace),
	}
	list := &clusterapiv1alpha2.MachineDeploymentList{}
	err := u.ctrlClient.List(context.TODO(), list, selectors...)
	if err != nil {
		return nil, errors.Wrap(err, "error listing machines")
	}

	return list, nil
}

func (u *MachineDeploymentUpgrader) upgradeMachineDeployments(list *clusterapiv1alpha2.MachineDeploymentList) error {
	for _, machineDeployment := range list.Items {
		// Skip any machineDeployments that already have this upgrade annotation id
		if val, ok := machineDeployment.Spec.Template.Annotations[UpgradeIDAnnotationKey]; ok && val == u.upgradeID {
			continue
		}
		if err := u.updateMachineDeployment(&machineDeployment); err != nil {
			u.log.Error(err, "Failed to create new MachineDeployment", "namespace", machineDeployment.Namespace, "name", machineDeployment.Name)
			return err
		}
	}
	return nil
}

func (u *MachineDeploymentUpgrader) updateMachineDeployment(machineDeployment *clusterapiv1alpha2.MachineDeployment) error {
	u.log.Info("Updating MachineDeployment", "namespace", machineDeployment.Namespace, "name", machineDeployment.Name)

	// Get the original, pre-modified version in json
	original := machineDeployment.DeepCopy()

	// Make the modification(s)
	desiredVersion := u.desiredVersion.String()
	machineDeployment.Spec.Template.Spec.Version = &desiredVersion
	// Add the upgrade ID to this template so all machines get it
	if machineDeployment.Spec.Template.Annotations == nil {
		machineDeployment.Spec.Template.Annotations = map[string]string{}
	}
	machineDeployment.Spec.Template.Annotations[UpgradeIDAnnotationKey] = u.upgradeID

	if u.imageField != "" && u.imageID != "" {
		if err := updateMachineSpecImage(&machineDeployment.Spec.Template.Spec, u.imageField, u.imageID); err != nil {
			return err
		}
	}

	// Get the updated version in json
	updated := machineDeployment.DeepCopy()

	err := u.ctrlClient.Patch(context.TODO(), updated, ctrlclient.MergeFrom(original))
	if err != nil {
		return errors.Wrapf(err, "error patching machinedeployment %s", machineDeployment.Name)
	}

	return nil
}

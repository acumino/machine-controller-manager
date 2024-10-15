// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/controller/autoscaler"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machineutils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
)

// rolloutInplace implements the logic for rolling  a machine set without replacing it.
func (dc *controller) rolloutInplace(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList) error {
	clusterAutoscalerScaleDownAnnotations := make(map[string]string)
	clusterAutoscalerScaleDownAnnotations[autoscaler.ClusterAutoscalerScaleDownDisabledAnnotationKey] = autoscaler.ClusterAutoscalerScaleDownDisabledAnnotationValue

	// We do this to avoid accidentally deleting the user provided annotations.
	clusterAutoscalerScaleDownAnnotations[autoscaler.ClusterAutoscalerScaleDownDisabledAnnotationByMCMKey] = autoscaler.ClusterAutoscalerScaleDownDisabledAnnotationByMCMValue

	newIS, oldISs, err := dc.getAllMachineSetsAndSyncRevision(ctx, d, isList, machineMap, true)
	if err != nil {
		return err
	}
	allISs := append(oldISs, newIS)

	err = dc.taintNodesBackingMachineSets(
		ctx,
		oldISs, &v1.Taint{
			Key:    PreferNoScheduleKey,
			Value:  "True",
			Effect: "PreferNoSchedule",
		},
	)

	if dc.autoscalerScaleDownAnnotationDuringRollout {
		// Add the annotation on the all machinesets if there are any old-machinesets and not scaled-to-zero.
		// This also helps in annotating the node under new-machineset, incase the reconciliation is failing in next
		// status-rollout steps.
		if len(oldISs) > 0 && !dc.machineSetsScaledToZero(oldISs) {
			// Annotate all the nodes under this machine-deployment, as roll-out is on-going.
			err := dc.annotateNodesBackingMachineSets(ctx, allISs, clusterAutoscalerScaleDownAnnotations)
			if err != nil {
				klog.Errorf("Failed to add %s on all nodes. Error: %s", clusterAutoscalerScaleDownAnnotations, err)
				return err
			}
		}
	}

	if err != nil {
		klog.Warningf("Failed to add %s on all nodes. Error: %s", PreferNoScheduleKey, err)
	}

	// prepare old ISs for update
	machinesSelectedForUpdate, err := dc.reconcileOldMachineSetsInPlace(ctx, allISs, FilterActiveMachineSets(oldISs), newIS, d)
	if err != nil {
		return err
	}
	if machinesSelectedForUpdate {
		// Update DeploymentStatus
		return dc.syncRolloutStatus(ctx, allISs, newIS, d)
	}

	// mcm provider will drain the machine and GNA mark it that the node/machine updated is successful or failed.

	// Here we will try to scale up the new machine set and those which has `machine.sapcloud.io/update-successful` label
	// can tranfer there owener ship to the new machine set.
	// here it need to be taken care that, while owner ship is being transferred, the machine should not be deleted
	// and the old machine set should not be scaled up to create the machine again.

	// Scale up, if we can.
	scaledUp, err := dc.reconcileNewMachineSetInPlace(ctx, oldISs, newIS, d)
	if err != nil {
		return err
	}
	if scaledUp {
		// Update DeploymentStatus
		return dc.syncRolloutStatus(ctx, allISs, newIS, d)
	}

	if MachineDeploymentComplete(d, &d.Status) {
		if dc.autoscalerScaleDownAnnotationDuringRollout {
			// Check if any of the machine under this MachineDeployment contains the by-mcm annotation, and
			// remove the original autoscaler annotation only after.
			err := dc.removeAutoscalerAnnotationsIfRequired(ctx, allISs, clusterAutoscalerScaleDownAnnotations)
			if err != nil {
				return err
			}
		}
		if err := dc.cleanupMachineDeployment(ctx, oldISs, d); err != nil {
			return err
		}
	}

	// Sync deployment status
	return dc.syncRolloutStatus(ctx, allISs, newIS, d)
}

func (dc *controller) reconcileNewMachineSetInPlace(ctx context.Context, oldISs []*v1alpha1.MachineSet, newIS *v1alpha1.MachineSet, deployment *v1alpha1.MachineDeployment) (bool, error) {
	if (newIS.Spec.Replicas) == (deployment.Spec.Replicas) {
		// Scaling not required.
		return false, nil
	}
	if (newIS.Spec.Replicas) > (deployment.Spec.Replicas) {
		// Scale down.
		scaled, _, err := dc.scaleMachineSetAndRecordEvent(ctx, newIS, (deployment.Spec.Replicas), deployment)
		return scaled, err
	}

	addedNewReplicasCount := int32(0)

	for _, is := range oldISs {
		transfferedMachineCount := int32(0)
		// get the machines for the machine set.
		machines, err := dc.machineLister.List(labels.SelectorFromSet(is.Spec.Selector.MatchLabels))
		if err != nil {
			return false, nil
		}

		// select the nodes which has been updated successfully.
		// I am  hoping machine lable selector goes on the nodes // it doesn't
		nodes, err := dc.nodeLister.List(labels.SelectorFromSet(map[string]string{v1alpha1.LabelKeyMachineUpdateSuccessful: "true"}))
		if err != nil {
			return false, nil
		}

		for _, node := range nodes {
			// here change the labels on the machine to add the new machine set label.
			// and remove the old machine set label.
			// also change the owner reference of the machine to the new machine set.
			// down scale the old machine set.

			// get the machine from the node.
			// error need to be handled.
			machine, err := getMachineNameFromNode(machines, node)
			if err != nil {
				continue
			}
			// removes labels not present in newIS so that the machine is not selected by the old machine set
			// TODO : will have to update labels on the node maybe or MCM will do itself after the ownerreference update.
			machineNewLabels := OverwritePresentDropNotPresent(machine.Labels, is.Spec.Selector.MatchLabels, newIS.Spec.Selector.MatchLabels)

			// TODO: here add label patch also or do and update call maybe. // done need to test
			// should be all done in one call that for sure
			addControllerPatch := fmt.Sprintf(
				`{"metadata":{"ownerReferences":[{"apiVersion":"machine.sapcloud.io/v1alpha1","kind":"%s","name":"%s","uid":"%s","controller":true,"blockOwnerDeletion":true}],"labels":%s,"uid":"%s"}}`,
				v1alpha1.SchemeGroupVersion.WithKind("MachineSet"),
				newIS.GetName(), newIS.GetUID(), machineNewLabels, machine.UID)
			err = dc.machineControl.PatchMachine(ctx, machine.Namespace, machine.Name, []byte(addControllerPatch))

			// what if there is no error can it lead to machine deletion in the new machine set since the set will have more replicas.
			// the set in the replica field in the machine set.
			// mostly will have to adapt the logic that during the inplace take care of these stuff.
			// which can lead to scary situations.
			if err != nil {
				scaled, _, err2 := dc.scaleMachineSetAndRecordEvent(ctx, newIS, newIS.Spec.Replicas+addedNewReplicasCount, deployment)
				// if things get errored here can it lead to machine deletion
				// not sure mostly not since the owner reference is not updated.
				// and no is has been scaled up or down.
				if err2 != nil {
					return addedNewReplicasCount > 0, err2
				}
				return scaled, err
			}

			transfferedMachineCount++ // scale down the old machine set.
			addedNewReplicasCount++   // scale up the new machine set.
		}

		// TODO: add the logic to down scale the old machiene set.
		// can we safely do it, can it lead to machine deletion.
		// but since(maybe) the machine has been shifted to the new machine set, it should be safe to do it.
		_, _, err = dc.scaleMachineSetAndRecordEvent(ctx, is, is.Spec.Replicas-transfferedMachineCount, deployment)
		if err != nil {
			return addedNewReplicasCount > 0, err
		}
	}

	scaled, _, err := dc.scaleMachineSetAndRecordEvent(ctx, newIS, newIS.Spec.Replicas+addedNewReplicasCount, deployment)
	return scaled, err
}

func getMachineNameFromNode(machines []*v1alpha1.Machine, node *v1.Node) (*v1alpha1.Machine, error) {
	for _, machine := range machines {
		machineName, ok := node.Labels[machineutils.MachineLabelKey]
		if !ok || machineName != machine.Name {
			continue
		}

		return machine, nil
	}

	return nil, fmt.Errorf("machine not found for node %s", node.Name)
}

// TODO: during the change of the ownership of the machine, scale or delete of machine issue can occur.
// TODO: Will have to add the logic for `prepare for update`/`ready for update` machins should be counted in the not ready replicas. - done
func (dc *controller) reconcileOldMachineSetsInPlace(ctx context.Context, allISs []*v1alpha1.MachineSet, oldISs []*v1alpha1.MachineSet, newIS *v1alpha1.MachineSet, deployment *v1alpha1.MachineDeployment) (bool, error) {
	oldMachinesCount := GetReplicaCountForMachineSets(oldISs)
	if oldMachinesCount == 0 {
		// Can't scale down further
		return false, nil
	}

	allMachinesCount := GetReplicaCountForMachineSets(allISs)
	klog.V(3).Infof("New machine set %s has %d available machines.", newIS.Name, newIS.Status.AvailableReplicas)
	maxUnavailable := MaxUnavailable(*deployment)

	minAvailable := (deployment.Spec.Replicas) - maxUnavailable
	newISUnavailableMachineCount := (newIS.Spec.Replicas) - newIS.Status.AvailableReplicas
	oldISsMachineInUpdateProcess, err := dc.getMachineInUpdateProcess(oldISs)
	if err != nil {
		return false, err
	}

	klog.V(3).Infof("some data allMachinesCount:%d,  minAvailable:%d,  newISUnavailableMachineCount:%d,  oldISsMachineInUpdateProcess:%d", allMachinesCount, minAvailable, newISUnavailableMachineCount, oldISsMachineInUpdateProcess)

	maxUpdatePossible := allMachinesCount - minAvailable - newISUnavailableMachineCount - oldISsMachineInUpdateProcess
	if maxUpdatePossible <= 0 {
		return false, nil
	}

	klog.V(3).Infof("call prepareMachineForUpdate, maxUpdatePossible %d", maxUpdatePossible)
	// preapre machine from old machine sets for get updated, need check maxUnavailable to ensure we can select machines for update.
	allISs = append(oldISs, newIS)
	machineSelectedForUpdate, err := dc.prepareMachineForUpdate(ctx, allISs, oldISs, deployment)
	if err != nil {
		return false, err
	}

	return machineSelectedForUpdate > 0, nil
}

// OverwritePresentDropNotPresent returns a map with the keys present in newIS and current, and the values from newIS.
func OverwritePresentDropNotPresent[T any](current, oldIS, newIS map[string]T) map[string]T {
	out := make(map[string]T, len(newIS))

	for k := range newIS {
		out[k] = newIS[k]
	}

	for k := range current {
		if _, ok := oldIS[k]; !ok {
			out[k] = current[k]
		}
	}

	return out
}

// MergeStringMaps merges the content of the newMaps with the oldMap. If a key already exists then
// it gets overwritten by the last value with the same key.
func MergeStringMaps[T any](oldMap map[string]T, newMaps ...map[string]T) map[string]T {
	var out map[string]T

	if oldMap != nil {
		out = make(map[string]T, len(oldMap))
	}
	for k, v := range oldMap {
		out[k] = v
	}

	for _, newMap := range newMaps {
		if newMap != nil && out == nil {
			out = make(map[string]T)
		}

		for k, v := range newMap {
			out[k] = v
		}
	}

	return out
}

func (dc *controller) getMachineInUpdateProcess(oldISs []*v1alpha1.MachineSet) (int32, error) {
	machineInUpdateProcess := int32(0)
	for _, is := range oldISs {
		// TODO: merge map of is labelselectors also - Done

		//// Labels are not being removed prepare for update will alwyas be there till end.
		// also handle the case of ready for update machines in the not ready replicas.
		// both list be considered for the not ready replicas and duplicates should be counted ones.

		// ** Need to check if failed to update machine should be dealed in special manner, as of now they will be counted in the not ready replicas.

		// Ones drain completed MCM provider put the label of ready for update.
		machines, err := dc.machineLister.List(labels.SelectorFromSet(MergeStringMaps(is.Spec.Selector.MatchLabels, map[string]string{v1alpha1.LabelKeyMachinePrepareForUpdate: "true"})))
		if err != nil {
			return 0, nil
		}
		machineInUpdateProcess += int32(len(machines))
	}

	return machineInUpdateProcess, nil
}

func (dc *controller) prepareMachineForUpdate(ctx context.Context, allISs []*v1alpha1.MachineSet, oldISs []*v1alpha1.MachineSet, deployment *v1alpha1.MachineDeployment) (int32, error) {
	klog.V(3).Infof("inside prepareMachineForUpdate")
	maxUnavailable := MaxUnavailable(*deployment)

	// TODO: here also consider machine under the update. - done
	// Check if we can pick machines from old ISes for updating to new IS.
	minAvailable := (deployment.Spec.Replicas) - maxUnavailable
	oldISsMachineInUpdateProcess, err := dc.getMachineInUpdateProcess(oldISs)
	if err != nil {
		return int32(0), err
	}
	// Find the number of available machines.
	availableMachineCount := GetAvailableReplicaCountForMachineSets(allISs) - oldISsMachineInUpdateProcess
	if availableMachineCount <= minAvailable {
		// Cannot pick for updating.
		return 0, nil
	}

	sort.Sort(MachineSetsByCreationTimestamp(oldISs))

	totalReadyForUpdate := int32(0)
	totalReadyForUpdateCount := availableMachineCount - minAvailable
	for _, targetIS := range oldISs {
		if totalReadyForUpdate >= totalReadyForUpdateCount {
			// No further updating required.
			break
		}
		if (targetIS.Spec.Replicas) == 0 {
			// cannot pick this ReplicaSet.
			continue
		}
		// prepare for update
		readyForUpdateCount := int32(integer.IntMin(int((targetIS.Spec.Replicas)), int(totalReadyForUpdateCount-totalReadyForUpdate)))
		newReplicasCount := (targetIS.Spec.Replicas) - readyForUpdateCount

		klog.V(3).Infof("call pickMachineToPrepareforUpdate, newReplicasCount %d", newReplicasCount)
		if newReplicasCount > (targetIS.Spec.Replicas) {
			return 0, fmt.Errorf("when selecting machine from old IS for update, got invalid request %s %d -> %d", targetIS.Name, (targetIS.Spec.Replicas), newReplicasCount)
		}
		err := dc.pickMachineToPrepareforUpdate(ctx, targetIS, newReplicasCount)
		if err != nil {
			return totalReadyForUpdate, err
		}

		totalReadyForUpdate += readyForUpdateCount
	}

	return totalReadyForUpdate, nil
}

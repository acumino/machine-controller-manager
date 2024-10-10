// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeBuilder used to register the Machine resource.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	localSchemeBuilder = &SchemeBuilder

	// AddToScheme is a pointer to SchemeBuilder.AddToScheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

const (
	// GroupName is the group name use in this package
	GroupName = "machine.sapcloud.io"

	// LabelKeyMachinePrepareForUpdate is the label key for the machine indicating the machine is prepared for update.
	LabelKeyMachinePrepareForUpdate = "machine.sapcloud.io/prepare-for-update"
	// LabelKeyMachineClass is the label key for the machine indicating the machine is ready for update.
	LabelKeyMachineIsReadyForUpdate = "machine.sapcloud.io/is-ready-for-update"
	// LabelKeyMachineUpdateSuccessful is the label key for the machine indicating the machine update was successful.
	LabelKeyMachineUpdateSuccessful = "machine.sapcloud.io/update-successful"
	// LabelKeyMachineUpdateFailed is the label key for the machine indicating the machine update failed.
	LabelKeyMachineUpdateFailed = "machine.sapcloud.io/update-failed"
)

// SchemeGroupVersion is group version used to register these objects
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// func Init() {
// 	// We only register manually written functions here. The registration of the
// 	// generated functions takes place in the generated files. The separation
// 	// makes the code compile even when the generated files are missing.
// 	SchemeBuilder.Register(addKnownTypes)
// }

// Adds the list of known types to api.Scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&MachineClass{},
		&MachineClassList{},

		&Machine{},
		&MachineList{},

		&MachineSet{},
		&MachineSetList{},

		&MachineDeployment{},
		&MachineDeploymentList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	machine_internal "github.com/gardener/machine-controller-manager/pkg/apis/machine"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	faketyped "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned/typed/machine/v1alpha1/fake"
	machineinformers "github.com/gardener/machine-controller-manager/pkg/client/informers/externalversions"
	customfake "github.com/gardener/machine-controller-manager/pkg/fakeclient"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/cache"

	"github.com/gardener/machine-controller-manager/pkg/util/provider/drain"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/driver"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/options"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	coreinformers "k8s.io/client-go/informers"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

func TestMachineControllerSuite(t *testing.T) {
	//to enable goroutine leak check , currently not using as it fails tests for less important leaks
	//defer goleak.VerifyNone(t)

	//for filtering out warning logs. Reflector short watch warning logs won't print now
	klog.SetOutput(io.Discard)
	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	_ = flags.Set("logtostderr", "false")

	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine Controller Suite")
}

var (
	controllerKindMachine = v1alpha1.SchemeGroupVersion.WithKind("Machine")
	MachineClass          = "MachineClass"
	TestMachineClass      = "machineClass-0"
)

func newMachineDeployment(
	specTemplate *v1alpha1.MachineTemplateSpec,
	replicas int32,
	minReadySeconds int32,
	statusTemplate *v1alpha1.MachineDeploymentStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
) *v1alpha1.MachineDeployment {
	return newMachineDeployments(1, specTemplate, replicas, minReadySeconds, statusTemplate, owner, annotations, labels)[0]
}

func newMachineDeployments(
	machineDeploymentCount int,
	specTemplate *v1alpha1.MachineTemplateSpec,
	replicas int32,
	minReadySeconds int32,
	statusTemplate *v1alpha1.MachineDeploymentStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
) []*v1alpha1.MachineDeployment {

	intStr1 := intstr.FromInt(1)
	machineDeployments := make([]*v1alpha1.MachineDeployment, machineDeploymentCount)
	for i := range machineDeployments {
		machineDeployment := &v1alpha1.MachineDeployment{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "machine.sapcloud.io",
				Kind:       "MachineDeployment",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("machinedeployment-%d", i),
				Namespace: testNamespace,
				Labels:    labels,
			},
			Spec: v1alpha1.MachineDeploymentSpec{
				MinReadySeconds: minReadySeconds,
				Replicas:        replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: deepCopy(specTemplate.ObjectMeta.Labels),
				},
				Strategy: v1alpha1.MachineDeploymentStrategy{
					RollingUpdate: &v1alpha1.RollingUpdateMachineDeployment{
						UpdateConfiguration: v1alpha1.UpdateConfiguration{
							MaxSurge:       &intStr1,
							MaxUnavailable: &intStr1,
						},
					},
				},
				Template: *specTemplate.DeepCopy(),
			},
		}

		if statusTemplate != nil {
			machineDeployment.Status = *statusTemplate.DeepCopy()
		}

		if owner != nil {
			machineDeployment.OwnerReferences = append(machineDeployment.OwnerReferences, *owner.DeepCopy())
		}

		if annotations != nil {
			machineDeployment.Annotations = annotations
		}

		machineDeployments[i] = machineDeployment
	}
	return machineDeployments
}

func newMachineSetFromMachineDeployment(
	machineDeployment *v1alpha1.MachineDeployment,
	replicas int32,
	statusTemplate *v1alpha1.MachineSetStatus,
	annotations map[string]string,
	labels map[string]string,
) *v1alpha1.MachineSet {
	return newMachineSetsFromMachineDeployment(1, machineDeployment, replicas, statusTemplate, annotations, labels)[0]
}

func newMachineSetsFromMachineDeployment(
	machineSetCount int,
	machineDeployment *v1alpha1.MachineDeployment,
	replicas int32,
	statusTemplate *v1alpha1.MachineSetStatus,
	annotations map[string]string,
	labels map[string]string,
) []*v1alpha1.MachineSet {

	finalLabels := make(map[string]string)
	for k, v := range labels {
		finalLabels[k] = v
	}
	for k, v := range machineDeployment.Spec.Template.Labels {
		finalLabels[k] = v
	}

	t := &machineDeployment.TypeMeta

	return newMachineSets(
		machineSetCount,
		&machineDeployment.Spec.Template,
		replicas,
		machineDeployment.Spec.MinReadySeconds,
		statusTemplate,
		&metav1.OwnerReference{
			APIVersion:         t.APIVersion,
			Kind:               t.Kind,
			Name:               machineDeployment.Name,
			UID:                machineDeployment.UID,
			BlockOwnerDeletion: pointer.BoolPtr(true),
			Controller:         pointer.BoolPtr(true),
		},
		annotations,
		finalLabels,
	)
}

func newMachineSet(
	specTemplate *v1alpha1.MachineTemplateSpec,
	replicas int32,
	minReadySeconds int32,
	statusTemplate *v1alpha1.MachineSetStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
) *v1alpha1.MachineSet {
	return newMachineSets(1, specTemplate, replicas, minReadySeconds, statusTemplate, owner, annotations, labels)[0]
}

func newMachineSets(
	machineSetCount int,
	specTemplate *v1alpha1.MachineTemplateSpec,
	replicas int32,
	minReadySeconds int32,
	statusTemplate *v1alpha1.MachineSetStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
) []*v1alpha1.MachineSet {

	machineSets := make([]*v1alpha1.MachineSet, machineSetCount)
	for i := range machineSets {
		ms := &v1alpha1.MachineSet{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "machine.sapcloud.io",
				Kind:       "MachineSet",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("machineset-%d", i),
				Namespace: testNamespace,
				Labels:    labels,
			},
			Spec: v1alpha1.MachineSetSpec{
				MachineClass:    *specTemplate.Spec.Class.DeepCopy(),
				MinReadySeconds: minReadySeconds,
				Replicas:        replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: deepCopy(specTemplate.ObjectMeta.Labels),
				},
				Template: *specTemplate.DeepCopy(),
			},
		}

		if statusTemplate != nil {
			ms.Status = *statusTemplate.DeepCopy()
		}

		if owner != nil {
			ms.OwnerReferences = append(ms.OwnerReferences, *owner.DeepCopy())
		}

		if annotations != nil {
			ms.Annotations = annotations
		}

		machineSets[i] = ms
	}
	return machineSets
}

func deepCopy(m map[string]string) map[string]string {
	r := make(map[string]string, len(m))
	for k := range m {
		r[k] = m[k]
	}
	return r
}

func newMachineFromMachineSet(
	machineSet *v1alpha1.MachineSet,
	statusTemplate *v1alpha1.MachineStatus,
	annotations map[string]string,
	labels map[string]string,
	addFinalizer bool,
) *v1alpha1.Machine {
	return newMachinesFromMachineSet(1, machineSet, statusTemplate, annotations, labels, addFinalizer)[0]
}

func newMachinesFromMachineSet(
	machineCount int,
	machineSet *v1alpha1.MachineSet,
	statusTemplate *v1alpha1.MachineStatus,
	annotations map[string]string,
	labels map[string]string,
	addFinalizer bool,
) []*v1alpha1.Machine {
	t := &machineSet.TypeMeta

	finalLabels := make(map[string]string, 0)
	for k, v := range labels {
		finalLabels[k] = v
	}
	for k, v := range machineSet.Spec.Template.Labels {
		finalLabels[k] = v
	}

	return newMachines(
		machineCount,
		&machineSet.Spec.Template,
		statusTemplate,
		&metav1.OwnerReference{
			APIVersion:         t.APIVersion,
			Kind:               t.Kind,
			Name:               machineSet.Name,
			UID:                machineSet.UID,
			BlockOwnerDeletion: boolPtr(true),
			Controller:         boolPtr(true),
		},
		annotations,
		finalLabels,
		addFinalizer,
		metav1.Now(),
	)
}

func newMachine(
	specTemplate *v1alpha1.MachineTemplateSpec,
	statusTemplate *v1alpha1.MachineStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
	addFinalizer bool,
	creationTimestamp metav1.Time,
) *v1alpha1.Machine {
	machine := newMachines(1, specTemplate, statusTemplate, owner, annotations, labels, addFinalizer, creationTimestamp)[0]
	if specTemplate.Name != "" {
		machine.Name = specTemplate.Name
	}
	return machine
}

func newHealthyMachine(generateName, nodeLabelKey string, phase v1alpha1.MachinePhase) *v1alpha1.Machine {
	return newMachine(
		&v1alpha1.MachineTemplateSpec{ObjectMeta: *newObjectMeta(&metav1.ObjectMeta{GenerateName: generateName}, 0)},
		&v1alpha1.MachineStatus{
			Conditions:    nodeConditions(true, false, false, false, false),
			CurrentStatus: v1alpha1.CurrentStatus{Phase: phase, LastUpdateTime: metav1.Now()},
		},
		nil, nil, map[string]string{v1alpha1.NodeLabelKey: nodeLabelKey}, true, metav1.Now(),
	)
}

func newMachines(
	machineCount int,
	specTemplate *v1alpha1.MachineTemplateSpec,
	statusTemplate *v1alpha1.MachineStatus,
	owner *metav1.OwnerReference,
	annotations map[string]string,
	labels map[string]string,
	addFinalizer bool,
	creationTimestamp metav1.Time,
) []*v1alpha1.Machine {
	machines := make([]*v1alpha1.Machine, machineCount)

	if annotations == nil {
		annotations = make(map[string]string, 0)
	}
	if labels == nil {
		labels = make(map[string]string, 0)
	}

	for i := range machines {
		m := &v1alpha1.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "machine.sapcloud.io",
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: func(generateName string, i int) string {
					if generateName != "" {
						return fmt.Sprintf("%s-%d", generateName, i)
					} else {
						return fmt.Sprintf("machine-%d", i)
					}
				}(specTemplate.ObjectMeta.GenerateName, i),
				Namespace:         testNamespace,
				Labels:            labels,
				Annotations:       annotations,
				CreationTimestamp: creationTimestamp,
				DeletionTimestamp: &creationTimestamp, //TODO: Add parametrize this
			},
			Spec: *newMachineSpec(&specTemplate.Spec, i),
		}
		finalizers := sets.NewString(m.Finalizers...)

		if addFinalizer {
			finalizers.Insert(MCMFinalizerName)
		}
		m.Finalizers = finalizers.List()

		if statusTemplate != nil {
			m.Status = *newMachineStatus(statusTemplate)
		}

		if owner != nil {
			m.OwnerReferences = append(m.OwnerReferences, *owner.DeepCopy())
		}

		machines[i] = m
	}
	return machines
}

func newNode(
	_ int,
	labels map[string]string,
	annotations map[string]string,
	nodeSpec *corev1.NodeSpec,
	nodeStatus *corev1.NodeStatus,
) *corev1.Node {
	return newNodes(1, labels, annotations, nodeSpec, nodeStatus)[0]
}

func newNodes(
	nodeCount int,
	labels map[string]string,
	annotations map[string]string,
	nodeSpec *corev1.NodeSpec,
	nodeStatus *corev1.NodeStatus,
) []*corev1.Node {

	nodes := make([]*corev1.Node, nodeCount)
	for i := range nodes {
		node := &corev1.Node{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Node",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:        fmt.Sprintf("node-%d", i),
				Labels:      labels,
				Annotations: annotations,
			},
			Spec:   *nodeSpec.DeepCopy(),
			Status: *nodeStatus.DeepCopy(),
		}

		nodes[i] = node
	}
	return nodes
}

func newObjectMeta(meta *metav1.ObjectMeta, index int) *metav1.ObjectMeta {
	r := meta.DeepCopy()

	if r.Name != "" {
		return r
	}

	if r.GenerateName != "" {
		r.Name = fmt.Sprintf("%s-%d", r.GenerateName, index)
		return r
	}

	r.Name = fmt.Sprintf("machine-%d", index)
	return r
}

func newMachineSpec(specTemplate *v1alpha1.MachineSpec, index int) *v1alpha1.MachineSpec {
	r := specTemplate.DeepCopy()
	if r.ProviderID == "" {
		return r
	}

	r.ProviderID = fmt.Sprintf("%s-%d", r.ProviderID, index)
	return r
}

func newMachineStatus(statusTemplate *v1alpha1.MachineStatus) *v1alpha1.MachineStatus {
	if statusTemplate == nil {
		return &v1alpha1.MachineStatus{}
	}

	return statusTemplate.DeepCopy()
}

func newSecretReference(meta *metav1.ObjectMeta, index int) *corev1.SecretReference {
	r := &corev1.SecretReference{
		Namespace: meta.Namespace,
	}

	if meta.Name != "" {
		r.Name = meta.Name
		return r
	}

	if meta.GenerateName != "" {
		r.Name = fmt.Sprintf("%s-%d", meta.GenerateName, index)
		return r
	}

	r.Name = fmt.Sprintf("machine-%d", index)
	return r
}

func boolPtr(b bool) *bool {
	return &b
}

func nodeConditions(kubeletReady, readOnlyFileSystem, networkUnavailable, diskPressure, kernelDeadlock bool) []corev1.NodeCondition {
	// KernelDeadlock,ReadonlyFilesystem,DiskPressure,NetworkUnavailable
	conditions := []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionStatus(strings.Title(strconv.FormatBool(kubeletReady))),
		},
		{
			Type:   "KernelDeadlock",
			Status: corev1.ConditionStatus(strings.Title(strconv.FormatBool(kernelDeadlock))),
		},
		{
			Type:   "ReadonlyFilesystem",
			Status: corev1.ConditionStatus(strings.Title(strconv.FormatBool(readOnlyFileSystem))),
		},
		{
			Type:   "DiskPressure",
			Status: corev1.ConditionStatus(strings.Title(strconv.FormatBool(diskPressure))),
		},
		{
			Type:   "NetworkUnavailable",
			Status: corev1.ConditionStatus(strings.Title(strconv.FormatBool(networkUnavailable))),
		},
	}

	return conditions
}

func createController(
	stop <-chan struct{},
	namespace string,
	controlMachineObjects, controlCoreObjects, targetCoreObjects []runtime.Object,
	fakedriver driver.Driver,
	noTargetCluster bool,
) (*controller, *customfake.FakeObjectTrackers) {

	fakeControlMachineClient, controlMachineObjectTracker := customfake.NewMachineClientSet(controlMachineObjects...)
	fakeTypedMachineClient := &faketyped.FakeMachineV1alpha1{
		Fake: &fakeControlMachineClient.Fake,
	}
	fakeControlCoreClient, controlCoreObjectTracker := customfake.NewCoreClientSet(controlCoreObjects...)
	fakeTargetCoreClient, targetCoreObjectTracker := customfake.NewCoreClientSet(targetCoreObjects...)
	fakeObjectTrackers := customfake.NewFakeObjectTrackers(
		controlMachineObjectTracker,
		controlCoreObjectTracker,
		targetCoreObjectTracker,
	)
	fakeObjectTrackers.Start()

	coreTargetInformerFactory := coreinformers.NewFilteredSharedInformerFactory(
		fakeTargetCoreClient,
		100*time.Millisecond,
		namespace,
		nil,
	)
	defer coreTargetInformerFactory.Start(stop)

	coreControlInformerFactory := coreinformers.NewFilteredSharedInformerFactory(
		fakeControlCoreClient,
		100*time.Millisecond,
		namespace,
		nil,
	)
	defer coreControlInformerFactory.Start(stop)

	coreControlSharedInformers := coreControlInformerFactory.Core().V1()
	secrets := coreControlSharedInformers.Secrets()

	controlMachineInformerFactory := machineinformers.NewFilteredSharedInformerFactory(
		fakeControlMachineClient,
		100*time.Millisecond,
		namespace,
		nil,
	)
	defer controlMachineInformerFactory.Start(stop)

	machineSharedInformers := controlMachineInformerFactory.Machine().V1alpha1()
	machineClass := machineSharedInformers.MachineClasses()
	machines := machineSharedInformers.Machines()

	internalExternalScheme := runtime.NewScheme()
	Expect(machine_internal.AddToScheme(internalExternalScheme)).To(Succeed())
	Expect(v1alpha1.AddToScheme(internalExternalScheme)).To(Succeed())

	safetyOptions := options.SafetyOptions{
		MachineCreationTimeout:                   metav1.Duration{Duration: 20 * time.Minute},
		MachineHealthTimeout:                     metav1.Duration{Duration: 10 * time.Minute},
		MachineDrainTimeout:                      metav1.Duration{Duration: 5 * time.Minute},
		MachineSafetyOrphanVMsPeriod:             metav1.Duration{Duration: 30 * time.Minute},
		MachineSafetyAPIServerStatusCheckPeriod:  metav1.Duration{Duration: 1 * time.Minute},
		MachineSafetyAPIServerStatusCheckTimeout: metav1.Duration{Duration: 30 * time.Second},
		MaxEvictRetries:                          drain.DefaultMaxEvictRetries,
	}

	controller := &controller{
		namespace:                   namespace,
		nodeConditions:              "KernelDeadlock,ReadonlyFilesystem,DiskPressure,NetworkUnavailable",
		driver:                      fakedriver,
		safetyOptions:               safetyOptions,
		machineClassLister:          machineClass.Lister(),
		machineClassSynced:          machineClass.Informer().HasSynced,
		controlCoreClient:           fakeControlCoreClient,
		controlMachineClient:        fakeTypedMachineClient,
		internalExternalScheme:      internalExternalScheme,
		secretLister:                secrets.Lister(),
		machineLister:               machines.Lister(),
		machineSynced:               machines.Informer().HasSynced,
		secretSynced:                secrets.Informer().HasSynced,
		machineClassQueue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineclass"),
		secretQueue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "secret"),
		nodeQueue:                   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "node"),
		machineQueue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machine"),
		machineTerminationQueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machinetermination"),
		machineSafetyOrphanVMsQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machinesafetyorphanvms"),
		machineSafetyAPIServerQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machinesafetyapiserver"),
		recorder:                    record.NewBroadcaster().NewRecorder(nil, corev1.EventSource{Component: ""}),
	}

	if !noTargetCluster {
		controller.targetCoreClient = fakeTargetCoreClient

		coreTargetSharedInformers := coreTargetInformerFactory.Core().V1()
		controller.nodeLister = coreTargetSharedInformers.Nodes().Lister()
		controller.nodeSynced = coreTargetSharedInformers.Nodes().Informer().HasSynced
		controller.pvcLister = coreTargetSharedInformers.PersistentVolumeClaims().Lister()
		controller.pvLister = coreTargetSharedInformers.PersistentVolumes().Lister()
		controller.podLister = coreTargetSharedInformers.Pods().Lister()
		controller.podSynced = coreTargetSharedInformers.Pods().Informer().HasSynced
	}

	// controller.internalExternalScheme = runtime.NewScheme()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(fakeControlCoreClient.CoreV1().RESTClient()).Events(namespace)})

	return controller, fakeObjectTrackers
}

func waitForCacheSync(stop <-chan struct{}, controller *controller) {
	Expect(cache.WaitForCacheSync(
		stop,
		controller.machineClassSynced,
		controller.machineSynced,
		controller.secretSynced,
	)).To(BeTrue())

	if controller.nodeLister != nil {
		Expect(cache.WaitForCacheSync(stop, controller.nodeSynced)).To(BeTrue())
	}
}

var _ = Describe("#createController", func() {
	objMeta := &metav1.ObjectMeta{
		GenerateName: "machine",
		Namespace:    "test",
	}

	It("success", func() {
		machine0 := newMachine(&v1alpha1.MachineTemplateSpec{
			ObjectMeta: *objMeta,
		}, nil, nil, nil, nil, false, metav1.Now())

		stop := make(chan struct{})
		defer close(stop)

		c, trackers := createController(stop, objMeta.Namespace, nil, nil, nil, nil, false)
		defer trackers.Stop()

		waitForCacheSync(stop, c)

		Expect(c).NotTo(BeNil())

		allMachineWatch, err := c.controlMachineClient.Machines(objMeta.Namespace).Watch(context.TODO(), metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		defer allMachineWatch.Stop()

		machine0Watch, err := c.controlMachineClient.Machines(objMeta.Namespace).Watch(context.TODO(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", machine0.Name),
		})
		Expect(err).NotTo(HaveOccurred())
		defer machine0Watch.Stop()

		go func() {
			_, err := c.controlMachineClient.Machines(objMeta.Namespace).Create(context.TODO(), machine0, metav1.CreateOptions{})
			if err != nil {
				fmt.Printf("Error creating machine: %s", err)
			}
		}()

		var event watch.Event
		Eventually(allMachineWatch.ResultChan()).Should(Receive(&event))
		Expect(event.Type).To(Equal(watch.Added))
		Expect(event.Object).To(Equal(machine0))

		Eventually(machine0Watch.ResultChan()).Should(Receive(&event))
		Expect(event.Type).To(Equal(watch.Added))
		Expect(event.Object).To(Equal(machine0))
	})
})

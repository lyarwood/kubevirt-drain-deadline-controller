package functional_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	vmiRunningTimeout  = 5 * time.Minute
	migrationTimeout   = 10 * time.Minute
	eventuallyInterval = 5 * time.Second

	annotationDrainDeadline = "deadline-eviction.kubevirt.io/drain-deadline"
	annotationManaged       = "deadline-eviction.kubevirt.io/managed"
)

func newCirrosVMI(name, namespace string) *kubevirtv1.VirtualMachineInstance {
	evictionStrategy := kubevirtv1.EvictionStrategyExternal
	return &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			EvictionStrategy: &evictionStrategy,
			Domain: kubevirtv1.DomainSpec{
				Resources: kubevirtv1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				Devices: kubevirtv1.Devices{
					Disks: []kubevirtv1.Disk{
						{
							Name: "disk0",
							DiskDevice: kubevirtv1.DiskDevice{
								Disk: &kubevirtv1.DiskTarget{Bus: "virtio"},
							},
						},
					},
				},
			},
			Volumes: []kubevirtv1.Volume{
				{
					Name: "disk0",
					VolumeSource: kubevirtv1.VolumeSource{
						ContainerDisk: &kubevirtv1.ContainerDiskSource{
							Image: "quay.io/kubevirt/cirros-container-disk-demo:latest",
						},
					},
				},
			},
		},
	}
}

func waitForVMIRunning(vmi *kubevirtv1.VirtualMachineInstance) *kubevirtv1.VirtualMachineInstance {
	GinkgoHelper()
	var running *kubevirtv1.VirtualMachineInstance
	Eventually(func(g Gomega) {
		var err error
		running, err = virtClient.VirtualMachineInstance(vmi.Namespace).Get(ctx, vmi.Name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(running.Status.Phase).To(Equal(kubevirtv1.Running))
		g.Expect(running.Status.NodeName).NotTo(BeEmpty())
	}, vmiRunningTimeout, eventuallyInterval).Should(Succeed(), "VMI should reach Running phase")
	return running
}

func setNodeDrainDeadline(nodeName, deadline string) {
	GinkgoHelper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotationDrainDeadline, deadline)
	_, err := virtClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func removeNodeDrainDeadline(nodeName string) {
	GinkgoHelper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, annotationDrainDeadline)
	_, err := virtClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func markVMIForEvacuation(vmi *kubevirtv1.VirtualMachineInstance) {
	GinkgoHelper()
	updated, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(ctx, vmi.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	updated.Status.EvacuationNodeName = updated.Status.NodeName
	_, err = virtClient.VirtualMachineInstance(vmi.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Deadline Eviction Controller", func() {
	Describe("should create a migration when a VMI is marked for evacuation", func() {
		It("creates a managed migration within the deadline", func() {
			vmi := newCirrosVMI("vmi-migration-created", testNamespace)
			_, err := virtClient.VirtualMachineInstance(testNamespace).Create(ctx, vmi, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = virtClient.VirtualMachineInstance(testNamespace).Delete(ctx, vmi.Name, metav1.DeleteOptions{})
			})

			vmi = waitForVMIRunning(vmi)

			futureDeadline := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
			setNodeDrainDeadline(vmi.Status.NodeName, futureDeadline)
			DeferCleanup(func() { removeNodeDrainDeadline(vmi.Status.NodeName) })

			markVMIForEvacuation(vmi)

			Eventually(func(g Gomega) {
				list, err := virtClient.VirtualMachineInstanceMigration(testNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				found := false
				for _, m := range list.Items {
					if m.Spec.VMIName == vmi.Name && m.Annotations[annotationManaged] == "true" {
						found = true
					}
				}
				g.Expect(found).To(BeTrue(), "managed migration should be created")
			}, migrationTimeout, eventuallyInterval).Should(Succeed())
		})
	})

	Describe("should delete a VMI that cannot be migrated within the deadline", func() {
		It("deletes the VMI after the deadline and the VM controller recreates it", func() {
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vm-deadline-exceeded",
					Namespace: testNamespace,
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
					Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
						Spec: newCirrosVMI("", testNamespace).Spec,
					},
				},
			}
			_, err := virtClient.VirtualMachine(testNamespace).Create(ctx, vm, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = virtClient.VirtualMachine(testNamespace).Delete(ctx, vm.Name, metav1.DeleteOptions{})
			})

			var vmi *kubevirtv1.VirtualMachineInstance
			Eventually(func(g Gomega) {
				vmi, err = virtClient.VirtualMachineInstance(testNamespace).Get(ctx, vm.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
			}, vmiRunningTimeout, eventuallyInterval).Should(Succeed())

			vmi = waitForVMIRunning(vmi)

			pastDeadline := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
			setNodeDrainDeadline(vmi.Status.NodeName, pastDeadline)
			DeferCleanup(func() { removeNodeDrainDeadline(vmi.Status.NodeName) })

			markVMIForEvacuation(vmi)

			originalUID := vmi.UID

			Eventually(func() bool {
				_, err := virtClient.VirtualMachineInstance(testNamespace).Get(ctx, vmi.Name, metav1.GetOptions{})
				return errors.IsNotFound(err)
			}, vmiRunningTimeout, eventuallyInterval).Should(BeTrue(), "VMI should be deleted after deadline")

			// RunStrategy: Always means the VM controller creates a new VMI.
			Eventually(func(g Gomega) {
				newVMI, err := virtClient.VirtualMachineInstance(testNamespace).Get(ctx, vm.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(newVMI.UID).NotTo(Equal(originalUID), "a new VMI should have been created")
			}, vmiRunningTimeout, eventuallyInterval).Should(Succeed())
		})
	})

	Describe("should not delete a VMI that successfully migrated before the deadline", func() {
		It("leaves the VMI alive after a successful migration", func() {
			vmi := newCirrosVMI("vmi-already-migrated", testNamespace)
			_, err := virtClient.VirtualMachineInstance(testNamespace).Create(ctx, vmi, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = virtClient.VirtualMachineInstance(testNamespace).Delete(ctx, vmi.Name, metav1.DeleteOptions{})
			})

			vmi = waitForVMIRunning(vmi)

			futureDeadline := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
			setNodeDrainDeadline(vmi.Status.NodeName, futureDeadline)
			DeferCleanup(func() { removeNodeDrainDeadline(vmi.Status.NodeName) })

			markVMIForEvacuation(vmi)

			// Wait for our controller to create a migration.
			Eventually(func(g Gomega) {
				list, err := virtClient.VirtualMachineInstanceMigration(testNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				found := false
				for _, m := range list.Items {
					if m.Spec.VMIName == vmi.Name && m.Annotations[annotationManaged] == "true" {
						found = true
					}
				}
				g.Expect(found).To(BeTrue())
			}, vmiRunningTimeout, eventuallyInterval).Should(Succeed())

			// Wait for migration to succeed (KubeVirt drives this, not our controller).
			Eventually(func(g Gomega) {
				updated, err := virtClient.VirtualMachineInstance(testNamespace).Get(ctx, vmi.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updated.Status.NodeName).NotTo(Equal(vmi.Status.EvacuationNodeName),
					"VMI should have migrated to a different node")
			}, migrationTimeout, eventuallyInterval).Should(Succeed())

			// Our controller should not delete the VMI since it already migrated.
			Consistently(func() bool {
				_, err := virtClient.VirtualMachineInstance(testNamespace).Get(ctx, vmi.Name, metav1.GetOptions{})
				return errors.IsNotFound(err)
			}, 30*time.Second, eventuallyInterval).Should(BeFalse(), "VMI should remain alive after successful migration")
		})
	})
})

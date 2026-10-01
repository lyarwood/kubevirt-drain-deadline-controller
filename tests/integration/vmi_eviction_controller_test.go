package integration_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testNamespace      = "default"
	eventuallyTimeout  = 5 * time.Second
	eventuallyInterval = 100 * time.Millisecond
)

func makeNode(name string, deadlineAnnotation string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if deadlineAnnotation != "" {
		node.Annotations = map[string]string{
			"deadline-eviction.kubevirt.io/drain-deadline": deadlineAnnotation,
		}
	}
	return node
}

func makeVMI(name, nodeName, evacuationNodeName string) *kubevirtv1.VirtualMachineInstance {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Domain: kubevirtv1.DomainSpec{},
		},
	}
	vmi.Status.NodeName = nodeName
	vmi.Status.EvacuationNodeName = evacuationNodeName
	return vmi
}

func makeVM(name string, runStrategy kubevirtv1.VirtualMachineRunStrategy) *kubevirtv1.VirtualMachine {
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &runStrategy,
		},
	}
}

func vmiWithVMOwner(vmi *kubevirtv1.VirtualMachineInstance, vm *kubevirtv1.VirtualMachine) *kubevirtv1.VirtualMachineInstance {
	t := true
	vmi.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         "kubevirt.io/v1",
			Kind:               "VirtualMachine",
			Name:               vm.Name,
			UID:                vm.UID,
			Controller:         &t,
			BlockOwnerDeletion: &t,
		},
	}
	return vmi
}

func createNode(node *corev1.Node) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(ctx, node)
	})
}

func createVMI(vmi *kubevirtv1.VirtualMachineInstance) {
	GinkgoHelper()
	desiredStatus := *vmi.Status.DeepCopy()

	Expect(k8sClient.Create(ctx, vmi)).To(Succeed())

	statusPatch := client.MergeFrom(vmi.DeepCopy())
	vmi.Status = desiredStatus
	Expect(k8sClient.Status().Patch(ctx, vmi, statusPatch)).To(Succeed())

	DeferCleanup(func() {
		_ = k8sClient.Delete(ctx, vmi)
	})
}

var _ = Describe("VMIEvictionReconciler", func() {
	Describe("VMI with no EvacuationNodeName", func() {
		It("is a no-op — no migration is created", func() {
			node := makeNode("node-noop", "")
			createNode(node)

			vmi := makeVMI("vmi-noop", "node-noop", "")
			createVMI(vmi)

			Consistently(func(g Gomega) {
				list := &kubevirtv1.VirtualMachineInstanceMigrationList{}
				g.Expect(k8sClient.List(ctx, list, client.InNamespace(testNamespace))).To(Succeed())
				for _, m := range list.Items {
					g.Expect(m.Spec.VMIName).NotTo(Equal("vmi-noop"))
				}
			}, 1*time.Second, eventuallyInterval).Should(Succeed())
		})
	})

	Describe("VMI evacuating within deadline", func() {
		It("creates a managed migration but does not delete the VMI", func() {
			futureDeadline := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
			node := makeNode("node-within", futureDeadline)
			createNode(node)

			vmi := makeVMI("vmi-within", "node-within", "node-within")
			createVMI(vmi)

			Eventually(func(g Gomega) {
				list := &kubevirtv1.VirtualMachineInstanceMigrationList{}
				g.Expect(k8sClient.List(ctx, list, client.InNamespace(testNamespace))).To(Succeed())
				found := false
				for _, m := range list.Items {
					if m.Spec.VMIName == "vmi-within" && m.Annotations["deadline-eviction.kubevirt.io/managed"] == "true" {
						found = true
					}
				}
				g.Expect(found).To(BeTrue(), "managed migration should be created")
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vmi-within", Namespace: testNamespace}, &kubevirtv1.VirtualMachineInstance{})).To(Succeed())
		})
	})

	Describe("VMI evacuating, deadline exceeded, VMI still on source node", func() {
		It("deletes the VMI", func() {
			pastDeadline := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
			node := makeNode("node-exceeded", pastDeadline)
			createNode(node)

			vmi := makeVMI("vmi-exceeded", "node-exceeded", "node-exceeded")
			createVMI(vmi)

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "vmi-exceeded", Namespace: testNamespace}, &kubevirtv1.VirtualMachineInstance{})
				return errors.IsNotFound(err)
			}, eventuallyTimeout, eventuallyInterval).Should(BeTrue(), "VMI should be deleted after deadline")
		})
	})

	Describe("VMI evacuating, deadline exceeded, VMI already migrated away", func() {
		It("does NOT delete the VMI", func() {
			pastDeadline := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
			node := makeNode("node-src", pastDeadline)
			createNode(node)

			vmi := makeVMI("vmi-migrated", "node-dest", "node-src")
			createVMI(vmi)

			Consistently(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "vmi-migrated", Namespace: testNamespace}, &kubevirtv1.VirtualMachineInstance{})
				return errors.IsNotFound(err)
			}, 1*time.Second, eventuallyInterval).Should(BeFalse(), "VMI should not be deleted when already migrated")
		})
	})

	Describe("VMI with a failed managed migration", func() {
		It("creates a new migration", func() {
			futureDeadline := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
			node := makeNode("node-retry", futureDeadline)
			createNode(node)

			vmi := makeVMI("vmi-retry", "node-retry", "node-retry")
			createVMI(vmi)

			var firstMigrationName string
			Eventually(func(g Gomega) {
				list := &kubevirtv1.VirtualMachineInstanceMigrationList{}
				g.Expect(k8sClient.List(ctx, list, client.InNamespace(testNamespace))).To(Succeed())
				for _, m := range list.Items {
					if m.Spec.VMIName == "vmi-retry" && m.Annotations["deadline-eviction.kubevirt.io/managed"] == "true" {
						firstMigrationName = m.Name
					}
				}
				g.Expect(firstMigrationName).NotTo(BeEmpty())
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			migration := &kubevirtv1.VirtualMachineInstanceMigration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: firstMigrationName, Namespace: testNamespace}, migration)).To(Succeed())
			statusPatch := client.MergeFrom(migration.DeepCopy())
			migration.Status.Phase = kubevirtv1.MigrationFailed
			Expect(k8sClient.Status().Patch(ctx, migration, statusPatch)).To(Succeed())

			Eventually(func(g Gomega) {
				list := &kubevirtv1.VirtualMachineInstanceMigrationList{}
				g.Expect(k8sClient.List(ctx, list, client.InNamespace(testNamespace))).To(Succeed())
				count := 0
				for _, m := range list.Items {
					if m.Spec.VMIName == "vmi-retry" && m.Annotations["deadline-eviction.kubevirt.io/managed"] == "true" {
						count++
					}
				}
				g.Expect(count).To(BeNumerically(">=", 2), "a new migration should be created after failure")
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
		})
	})

	Describe("VMI with RunStrategy: Always parent VM, deadline exceeded", func() {
		It("deletes the VMI", func() {
			pastDeadline := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
			node := makeNode("node-always", pastDeadline)
			createNode(node)

			vm := makeVM("vm-always", kubevirtv1.RunStrategyAlways)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: testNamespace}, vm)).To(Succeed())

			vmi := vmiWithVMOwner(makeVMI("vmi-always", "node-always", "node-always"), vm)
			createVMI(vmi)

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "vmi-always", Namespace: testNamespace}, &kubevirtv1.VirtualMachineInstance{})
				return errors.IsNotFound(err)
			}, eventuallyTimeout, eventuallyInterval).Should(BeTrue(), "VMI should be deleted; VM controller will restart it elsewhere")
		})
	})

	Describe("VMI with RunStrategy: Manual parent VM, deadline exceeded", func() {
		It("deletes the VMI with a warning logged", func() {
			pastDeadline := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
			node := makeNode("node-manual", pastDeadline)
			createNode(node)

			vm := makeVM("vm-manual", kubevirtv1.RunStrategyManual)
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, vm) })

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: testNamespace}, vm)).To(Succeed())

			vmi := vmiWithVMOwner(makeVMI("vmi-manual", "node-manual", "node-manual"), vm)
			createVMI(vmi)

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: "vmi-manual", Namespace: testNamespace}, &kubevirtv1.VirtualMachineInstance{})
				return errors.IsNotFound(err)
			}, eventuallyTimeout, eventuallyInterval).Should(BeTrue(), "VMI should be deleted even with Manual run strategy")
		})
	})
})

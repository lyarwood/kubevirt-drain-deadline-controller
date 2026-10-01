package controller_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lyarwood/kubevirt-deadline-eviction-plugin/pkg/controller"
)

var _ = Describe("VMIEvictionReconciler (unit)", func() {
	var (
		fakeClient client.Client
		reconciler *controller.VMIEvictionReconciler
		ctx        = context.Background()
		testNS     = "default"
	)

	buildReconciler := func(defaultDeadline time.Duration, objs ...client.Object) {
		s := runtime.NewScheme()
		Expect(corev1.AddToScheme(s)).To(Succeed())
		Expect(kubevirtv1.AddToScheme(s)).To(Succeed())
		fakeClient = fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		reconciler = &controller.VMIEvictionReconciler{
			Client:                       fakeClient,
			DefaultDrainDeadlineDuration: defaultDeadline,
		}
	}

	do := func(name string) (ctrl.Result, error) {
		return reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNS},
		})
	}

	vmiExists := func(name string) bool {
		err := fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &kubevirtv1.VirtualMachineInstance{})
		return !k8serrors.IsNotFound(err)
	}

	managedMigrationCount := func(vmiName string) int {
		list := &kubevirtv1.VirtualMachineInstanceMigrationList{}
		Expect(fakeClient.List(ctx, list, client.InNamespace(testNS))).To(Succeed())
		count := 0
		for _, m := range list.Items {
			if m.Spec.VMIName == vmiName && m.Annotations[controller.AnnotationManaged] == "true" {
				count++
			}
		}
		return count
	}

	// --- helpers ---

	node := func(name, deadline string) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if deadline != "" {
			n.Annotations = map[string]string{controller.AnnotationDrainDeadline: deadline}
		}
		return n
	}

	vmi := func(name, nodeName, evacuationNodeName string) *kubevirtv1.VirtualMachineInstance {
		v := &kubevirtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec:       kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{}},
		}
		v.Status.NodeName = nodeName
		v.Status.EvacuationNodeName = evacuationNodeName
		return v
	}

	// managedMigration returns a managed migration created `age` ago.
	managedMigration := func(vmiName string, age time.Duration, phase kubevirtv1.VirtualMachineInstanceMigrationPhase) *kubevirtv1.VirtualMachineInstanceMigration {
		m := &kubevirtv1.VirtualMachineInstanceMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmiName + "-migration",
				Namespace: testNS,
				Annotations: map[string]string{
					controller.AnnotationManaged: "true",
				},
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
			},
			Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{VMIName: vmiName},
		}
		m.Status.Phase = phase
		return m
	}

	vm := func(name string, runStrategy kubevirtv1.VirtualMachineRunStrategy) *kubevirtv1.VirtualMachine {
		return &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec:       kubevirtv1.VirtualMachineSpec{RunStrategy: &runStrategy},
		}
	}

	withVMOwner := func(v *kubevirtv1.VirtualMachineInstance, ownerVM *kubevirtv1.VirtualMachine) *kubevirtv1.VirtualMachineInstance {
		t := true
		v.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine",
			Name: ownerVM.Name, UID: ownerVM.UID,
			Controller: &t, BlockOwnerDeletion: &t,
		}}
		return v
	}

	// --- specs ---

	Describe("no EvacuationNodeName", func() {
		It("is a no-op and returns an empty result", func() {
			v := vmi("vmi-1", "node-1", "")
			buildReconciler(time.Hour, node("node-1", ""), v)

			result, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(managedMigrationCount("vmi-1")).To(Equal(0))
		})
	})

	Describe("first observation of EvacuationNodeName", func() {
		It("creates a managed migration and requeues", func() {
			futureDeadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			v := vmi("vmi-1", "node-1", "node-1")
			buildReconciler(time.Hour, node("node-1", futureDeadline), v)

			result, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(managedMigrationCount("vmi-1")).To(Equal(1))
			Expect(vmiExists("vmi-1")).To(BeTrue())
		})
	})

	Describe("within deadline", func() {
		It("does not create a second migration when one is already active", func() {
			futureDeadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			v := vmi("vmi-1", "node-1", "node-1")
			existing := managedMigration("vmi-1", 5*time.Minute, kubevirtv1.MigrationRunning)
			buildReconciler(time.Hour, node("node-1", futureDeadline), v, existing)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(managedMigrationCount("vmi-1")).To(Equal(1))
			Expect(vmiExists("vmi-1")).To(BeTrue())
		})

		It("creates a new migration when the existing one failed", func() {
			futureDeadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			v := vmi("vmi-1", "node-1", "node-1")
			failed := managedMigration("vmi-1", 5*time.Minute, kubevirtv1.MigrationFailed)
			buildReconciler(time.Hour, node("node-1", futureDeadline), v, failed)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(managedMigrationCount("vmi-1")).To(Equal(2))
			Expect(vmiExists("vmi-1")).To(BeTrue())
		})

		It("uses the default deadline when node has no annotation", func() {
			// Migration started 30min ago; default is 1h → not expired.
			v := vmi("vmi-1", "node-1", "node-1")
			existing := managedMigration("vmi-1", 30*time.Minute, kubevirtv1.MigrationRunning)
			buildReconciler(time.Hour, node("node-1", ""), v, existing)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(vmiExists("vmi-1")).To(BeTrue())
		})
	})

	Describe("past deadline", func() {
		pastDeadline := func() string {
			return time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		}

		It("deletes a VMI still on the source node", func() {
			v := vmi("vmi-1", "node-1", "node-1")
			existing := managedMigration("vmi-1", 2*time.Minute, kubevirtv1.MigrationRunning)
			buildReconciler(time.Hour, node("node-1", pastDeadline()), v, existing)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(vmiExists("vmi-1")).To(BeFalse())
		})

		It("does not delete a VMI that already migrated away", func() {
			v := vmi("vmi-1", "node-dest", "node-1")
			existing := managedMigration("vmi-1", 2*time.Minute, kubevirtv1.MigrationRunning)
			buildReconciler(time.Hour, node("node-1", pastDeadline()), v, existing)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(vmiExists("vmi-1")).To(BeTrue())
		})

		It("expires when migration age exceeds the default deadline", func() {
			// Default = 30s; migration started 1min ago → expired.
			v := vmi("vmi-1", "node-1", "node-1")
			existing := managedMigration("vmi-1", time.Minute, kubevirtv1.MigrationRunning)
			buildReconciler(30*time.Second, node("node-1", ""), v, existing)

			_, err := do("vmi-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(vmiExists("vmi-1")).To(BeFalse())
		})

		Describe("RunStrategy handling", func() {
			It("deletes a VMI owned by RunStrategy:Always VM", func() {
				parent := vm("vm-1", kubevirtv1.RunStrategyAlways)
				v := withVMOwner(vmi("vm-1", "node-1", "node-1"), parent)
				existing := managedMigration("vm-1", 2*time.Minute, kubevirtv1.MigrationRunning)
				buildReconciler(time.Hour, node("node-1", pastDeadline()), v, parent, existing)

				_, err := do("vm-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(vmiExists("vm-1")).To(BeFalse())
			})

			It("deletes a VMI owned by RunStrategy:Manual VM", func() {
				parent := vm("vm-1", kubevirtv1.RunStrategyManual)
				v := withVMOwner(vmi("vm-1", "node-1", "node-1"), parent)
				existing := managedMigration("vm-1", 2*time.Minute, kubevirtv1.MigrationRunning)
				buildReconciler(time.Hour, node("node-1", pastDeadline()), v, parent, existing)

				_, err := do("vm-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(vmiExists("vm-1")).To(BeFalse())
			})

			It("deletes a bare VMI with no parent VM", func() {
				v := vmi("vmi-1", "node-1", "node-1")
				existing := managedMigration("vmi-1", 2*time.Minute, kubevirtv1.MigrationRunning)
				buildReconciler(time.Hour, node("node-1", pastDeadline()), v, existing)

				_, err := do("vmi-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(vmiExists("vmi-1")).To(BeFalse())
			})
		})
	})
})

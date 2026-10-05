package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	AnnotationDrainDeadline = "deadline-eviction.kubevirt.io/drain-deadline"
	AnnotationManaged       = "deadline-eviction.kubevirt.io/managed"

	requeueInterval = 30 * time.Second
)

type VMIEvictionReconciler struct {
	client.Client
	DefaultDrainDeadlineDuration time.Duration
}

func (r *VMIEvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := r.Get(ctx, req.NamespacedName, vmi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if vmi.Status.EvacuationNodeName == "" || vmi.IsFinal() || vmi.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Use the oldest managed migration's CreationTimestamp as the evacuation clock.
	// On the first reconcile no migration exists yet, so evacuationStart ≈ now.
	// ensureMigration creates one immediately after, so by the next reconcile we
	// have a stable, server-assigned timestamp to anchor the deadline to.
	evacuationStart, err := r.evacuationStartTime(ctx, vmi)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureMigration(ctx, vmi); err != nil {
		return ctrl.Result{}, err
	}

	deadline, err := r.resolveDeadline(ctx, vmi.Status.EvacuationNodeName, evacuationStart)
	if err != nil {
		return ctrl.Result{}, err
	}

	if time.Now().After(deadline) && vmi.Status.NodeName == vmi.Status.EvacuationNodeName {
		logger.Info("deadline exceeded, deleting VMI", "vmi", req.NamespacedName, "deadline", deadline)
		if err := r.deleteVMI(ctx, vmi); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// evacuationStartTime returns the CreationTimestamp of the oldest managed migration for
// this VMI. If no managed migration exists yet it returns time.Now(), which is the
// approximate time the first migration is about to be created.
func (r *VMIEvictionReconciler) evacuationStartTime(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance) (time.Time, error) {
	migrationList := &kubevirtv1.VirtualMachineInstanceMigrationList{}
	if err := r.List(ctx, migrationList, client.InNamespace(vmi.Namespace)); err != nil {
		return time.Time{}, fmt.Errorf("listing migrations for evacuation start: %w", err)
	}
	var earliest time.Time
	for _, m := range migrationList.Items {
		if m.Spec.VMIName != vmi.Name || m.Annotations[AnnotationManaged] != "true" {
			continue
		}
		t := m.CreationTimestamp.Time
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		return time.Now(), nil
	}
	return earliest, nil
}

func (r *VMIEvictionReconciler) resolveDeadline(ctx context.Context, nodeName string, evacuationStart time.Time) (time.Time, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return time.Time{}, fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	if raw, ok := node.Annotations[AnnotationDrainDeadline]; ok {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing node drain-deadline annotation: %w", err)
		}
		return t, nil
	}

	return evacuationStart.Add(r.DefaultDrainDeadlineDuration), nil
}

func (r *VMIEvictionReconciler) ensureMigration(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance) error {
	migrationList := &kubevirtv1.VirtualMachineInstanceMigrationList{}
	if err := r.List(ctx, migrationList, client.InNamespace(vmi.Namespace)); err != nil {
		return fmt.Errorf("listing migrations: %w", err)
	}

	for i := range migrationList.Items {
		m := &migrationList.Items[i]
		if m.Spec.VMIName != vmi.Name || m.Annotations[AnnotationManaged] != "true" {
			continue
		}
		// An active managed migration exists — nothing to do.
		if m.Status.Phase != kubevirtv1.MigrationFailed {
			return nil
		}
		// Failed migration: fall through to create a new one.
	}

	migration := &kubevirtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-deadline-eviction-", vmi.Name),
			Namespace:    vmi.Namespace,
			Annotations:  map[string]string{AnnotationManaged: "true"},
		},
		Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
			VMIName: vmi.Name,
		},
	}

	if err := r.Create(ctx, migration); err != nil {
		return fmt.Errorf("creating migration: %w", err)
	}
	return nil
}

// deleteVMI deletes the VMI, logging a warning when a parent VM won't restart it automatically.
func (r *VMIEvictionReconciler) deleteVMI(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance) error {
	logger := log.FromContext(ctx)

	for _, ref := range vmi.OwnerReferences {
		if ref.Kind != "VirtualMachine" {
			continue
		}
		vm := &kubevirtv1.VirtualMachine{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: vmi.Namespace}, vm); err != nil {
			if errors.IsNotFound(err) {
				break
			}
			return fmt.Errorf("getting parent VM: %w", err)
		}
		if vm.Spec.RunStrategy != nil {
			switch *vm.Spec.RunStrategy {
			case kubevirtv1.RunStrategyAlways, kubevirtv1.RunStrategyRerunOnFailure:
				// VM controller will restart the VMI on another node — desired drain behaviour.
			default:
				logger.Info("warning: VMI will not be automatically restarted after forced deletion",
					"vmi", vmi.Name, "runStrategy", *vm.Spec.RunStrategy)
			}
		}
		break
	}

	if err := r.Delete(ctx, vmi); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting VMI: %w", err)
	}
	return nil
}

func (r *VMIEvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Re-enqueue the parent VMI when a migration we created changes phase (e.g. fails),
	// so we don't have to wait for the 30s requeue to retry.
	migrationToVMI := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		m, ok := obj.(*kubevirtv1.VirtualMachineInstanceMigration)
		if !ok || m.Annotations[AnnotationManaged] != "true" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{Name: m.Spec.VMIName, Namespace: m.Namespace},
		}}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachineInstance{}).
		Watches(&kubevirtv1.VirtualMachineInstanceMigration{}, migrationToVMI).
		Complete(r)
}

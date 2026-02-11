/*
Copyright 2024 The etcd-operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	etcdaenixiov1alpha1 "github.com/aenix-io/etcd-operator/api/v1alpha1"
	"github.com/aenix-io/etcd-operator/internal/controller/factory"
	"github.com/aenix-io/etcd-operator/internal/log"
)

const (
	restoreFinalizerName = "etcd.aenix.io/restore-cleanup"
)

// EtcdRestoreReconciler reconciles an EtcdRestore object.
type EtcdRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=etcd.aenix.io,resources=etcdrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=etcd.aenix.io,resources=etcdrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=etcd.aenix.io,resources=etcdrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=etcd.aenix.io,resources=etcdclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="apps",resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="batch",resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch

func (r *EtcdRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Debug(ctx, "reconciling etcdrestore")

	restore := &etcdaenixiov1alpha1.EtcdRestore{}
	if err := r.Get(ctx, req.NamespacedName, restore); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: remove pause annotation to unblock cluster
	if !restore.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, restore)
	}

	// Skip terminal phases
	if restore.Status.Phase == etcdaenixiov1alpha1.EtcdRestorePhaseCompleted ||
		restore.Status.Phase == etcdaenixiov1alpha1.EtcdRestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	// Initialize phase
	if restore.Status.Phase == "" {
		restore.Status.Phase = etcdaenixiov1alpha1.EtcdRestorePhasePending
		now := metav1.Now()
		restore.Status.StartTime = &now
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Add finalizer for non-terminal phases
	if !controllerutil.ContainsFinalizer(restore, restoreFinalizerName) {
		controllerutil.AddFinalizer(restore, restoreFinalizerName)
		if err := r.Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the target cluster
	cluster := &etcdaenixiov1alpha1.EtcdCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.ClusterName,
		Namespace: restore.Namespace,
	}, cluster); err != nil {
		if errors.IsNotFound(err) {
			return r.failRestore(ctx, restore, fmt.Errorf("EtcdCluster %q not found", restore.Spec.ClusterName))
		}
		return ctrl.Result{}, err
	}

	// State machine
	switch restore.Status.Phase {
	case etcdaenixiov1alpha1.EtcdRestorePhasePending:
		return r.reconcilePending(ctx, restore, cluster)
	case etcdaenixiov1alpha1.EtcdRestorePhaseScalingDown:
		return r.reconcileScalingDown(ctx, restore, cluster)
	case etcdaenixiov1alpha1.EtcdRestorePhasePreparingPVCs:
		return r.reconcilePreparingPVCs(ctx, restore, cluster)
	case etcdaenixiov1alpha1.EtcdRestorePhaseRestoring:
		return r.reconcileRestoring(ctx, restore, cluster)
	case etcdaenixiov1alpha1.EtcdRestorePhaseUpdatingConfig:
		return r.reconcileUpdatingConfig(ctx, restore, cluster)
	case etcdaenixiov1alpha1.EtcdRestorePhaseScalingUp:
		return r.reconcileScalingUp(ctx, restore, cluster)
	}

	return ctrl.Result{}, nil
}

// reconcilePending validates prerequisites and begins the restore by pausing the cluster.
func (r *EtcdRestoreReconciler) reconcilePending(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	log.Info(ctx, "restore pending: validating and pausing cluster",
		"cluster", cluster.Name)

	// Validate backup source
	if restore.Spec.BackupSource.PVC == nil {
		return r.failRestore(ctx, restore, fmt.Errorf("backupSource.pvc is required"))
	}

	// Verify backup PVC exists
	backupPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.BackupSource.PVC.ClaimName,
		Namespace: restore.Namespace,
	}, backupPVC); err != nil {
		if errors.IsNotFound(err) {
			return r.failRestore(ctx, restore, fmt.Errorf("backup PVC %q not found", restore.Spec.BackupSource.PVC.ClaimName))
		}
		return ctrl.Result{}, err
	}

	// Check no other active restore for this cluster
	restoreList := &etcdaenixiov1alpha1.EtcdRestoreList{}
	if err := r.List(ctx, restoreList, client.InNamespace(restore.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range restoreList.Items {
		if other.Name == restore.Name {
			continue
		}
		if other.Spec.ClusterName == restore.Spec.ClusterName &&
			other.Status.Phase != etcdaenixiov1alpha1.EtcdRestorePhaseCompleted &&
			other.Status.Phase != etcdaenixiov1alpha1.EtcdRestorePhaseFailed &&
			other.Status.Phase != "" {
			return r.failRestore(ctx, restore,
				fmt.Errorf("another restore %q is already active for cluster %q", other.Name, cluster.Name))
		}
	}

	// Save original replicas
	restore.Status.OriginalReplicas = cluster.Spec.Replicas

	// Set pause annotation on the cluster
	if cluster.Annotations == nil {
		cluster.Annotations = make(map[string]string)
	}
	cluster.Annotations[etcdaenixiov1alpha1.EtcdClusterPausedAnnotation] = restore.Name
	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Scale StatefulSet to 0
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts); err != nil {
		if errors.IsNotFound(err) {
			// No STS yet, skip scaling
			return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhasePreparingPVCs)
		}
		return ctrl.Result{}, err
	}
	zero := int32(0)
	sts.Spec.Replicas = &zero
	if err := r.Update(ctx, sts); err != nil {
		return ctrl.Result{}, err
	}

	return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhaseScalingDown)
}

// reconcileScalingDown waits for all pods to be terminated.
// If pods are stuck in Terminating for more than 15 seconds, they are force-deleted.
func (r *EtcdRestoreReconciler) reconcileScalingDown(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	log.Info(ctx, "waiting for StatefulSet to scale to zero", "cluster", cluster.Name)

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts); err != nil {
		if errors.IsNotFound(err) {
			return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhasePreparingPVCs)
		}
		return ctrl.Result{}, err
	}

	if sts.Status.Replicas != 0 || sts.Status.ReadyReplicas != 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Verify no pods remain
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(cluster.Namespace),
		client.MatchingLabels(factory.PodLabels(cluster))); err != nil {
		return ctrl.Result{}, err
	}

	if len(podList.Items) > 0 {
		// Check how long we've been in ScalingDown phase
		scalingDownCond := meta.FindStatusCondition(restore.Status.Conditions, string(etcdaenixiov1alpha1.EtcdRestorePhaseScalingDown))
		if scalingDownCond != nil && time.Since(scalingDownCond.LastTransitionTime.Time) > 15*time.Second {
			// Force-delete stuck terminating pods
			gracePeriod := int64(0)
			for i := range podList.Items {
				pod := &podList.Items[i]
				log.Info(ctx, "force-deleting stuck pod", "pod", pod.Name)
				if err := r.Delete(ctx, pod, &client.DeleteOptions{
					GracePeriodSeconds: &gracePeriod,
				}); err != nil && !errors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to force-delete pod %s: %w", pod.Name, err)
				}
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhasePreparingPVCs)
}

// reconcilePreparingPVCs handles PVC deletion/recreation if StorageClassOverride is set,
// or verifies existing PVCs otherwise.
func (r *EtcdRestoreReconciler) reconcilePreparingPVCs(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	log.Info(ctx, "preparing PVCs for restore", "cluster", cluster.Name)

	if restore.Spec.StorageClassOverride != nil {
		// Check if PVCs already exist with the target StorageClass (from a previous reconcile)
		exist, err := factory.AllDataPVCsExist(ctx, cluster, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}

		if exist {
			// PVCs exist — check if they're using the correct StorageClass and are Bound
			correctSC, err := factory.AllDataPVCsHaveStorageClass(ctx, cluster, *restore.Spec.StorageClassOverride, r.Client)
			if err != nil {
				return ctrl.Result{}, err
			}

			if correctSC {
				// PVCs exist with correct SC, just wait for Bound
				bound, err := factory.AllDataPVCsBound(ctx, cluster, r.Client)
				if err != nil {
					return ctrl.Result{}, err
				}
				if !bound {
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
				}
				// All good, proceed to restoring
			} else {
				// PVCs exist but with wrong StorageClass — delete them
				if err := factory.DeleteDataPVCs(ctx, cluster, r.Client); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		} else {
			// No PVCs exist — check if we need to wait for deletion to finish, or create new ones
			gone, err := factory.AllDataPVCsGone(ctx, cluster, r.Client)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !gone {
				// Still waiting for old PVCs to be deleted
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			// Create new PVCs with overridden StorageClass
			if err := factory.CreateDataPVCs(ctx, cluster, *restore.Spec.StorageClassOverride, r.Client); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	} else {
		// Verify PVCs exist (they should from the StatefulSet)
		exist, err := factory.AllDataPVCsExist(ctx, cluster, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !exist {
			return r.failRestore(ctx, restore, fmt.Errorf("data PVCs not found for cluster %q", cluster.Name))
		}
	}

	return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhaseRestoring)
}

// reconcileRestoring creates the restore Job and waits for it to complete.
func (r *EtcdRestoreReconciler) reconcileRestoring(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	jobName := fmt.Sprintf("%s-restore", restore.Name)
	restore.Status.RestoreJobName = jobName

	// Check if Job already exists
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: restore.Namespace}, existingJob)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if errors.IsNotFound(err) {
		// Create the restore Job
		log.Info(ctx, "creating restore job", "job", jobName)
		job := factory.BuildRestoreJob(restore, cluster)
		if err := controllerutil.SetControllerReference(restore, job, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot set controller reference on job: %w", err)
		}
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create restore job: %w", err)
		}

		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check Job status
	if isJobComplete(existingJob) {
		log.Info(ctx, "restore job completed successfully", "job", jobName)
		return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhaseUpdatingConfig)
	}

	if isJobFailed(existingJob) {
		return r.failRestore(ctx, restore, fmt.Errorf("restore job %q failed", jobName))
	}

	// Still running
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// reconcileUpdatingConfig patches the cluster-state ConfigMap to force a new cluster bootstrap.
func (r *EtcdRestoreReconciler) reconcileUpdatingConfig(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	log.Info(ctx, "updating cluster state configmap", "cluster", cluster.Name)

	cmName := factory.GetClusterStateConfigMapName(cluster)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: cluster.Namespace}, cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get cluster state configmap: %w", err)
	}

	restoreToken := fmt.Sprintf("%s-%s-restore", cluster.Name, cluster.Namespace)
	cm.Data["ETCD_INITIAL_CLUSTER_STATE"] = "new"
	cm.Data["ETCD_INITIAL_CLUSTER_TOKEN"] = restoreToken

	if err := r.Update(ctx, cm); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update cluster state configmap: %w", err)
	}

	return r.transitionPhase(ctx, restore, etcdaenixiov1alpha1.EtcdRestorePhaseScalingUp)
}

// reconcileScalingUp restores the StatefulSet replicas and waits for readiness.
func (r *EtcdRestoreReconciler) reconcileScalingUp(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) (ctrl.Result, error) {
	log.Info(ctx, "scaling up cluster", "cluster", cluster.Name)

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts); err != nil {
		return ctrl.Result{}, err
	}

	// Restore original replica count
	targetReplicas := restore.Status.OriginalReplicas
	if targetReplicas == nil {
		targetReplicas = cluster.Spec.Replicas
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != *targetReplicas {
		sts.Spec.Replicas = targetReplicas
		if err := r.Update(ctx, sts); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for all replicas to be ready
	if sts.Status.ReadyReplicas != *targetReplicas {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Remove pause annotation from cluster
	if err := r.removePauseAnnotation(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up restore Job
	if restore.Status.RestoreJobName != "" {
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: restore.Status.RestoreJobName, Namespace: restore.Namespace,
		}, job); err == nil {
			propagation := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				log.Warn(ctx, "failed to clean up restore job", "error", err)
			}
		}
	}

	// Mark complete
	now := metav1.Now()
	restore.Status.CompletionTime = &now
	restore.Status.Phase = etcdaenixiov1alpha1.EtcdRestorePhaseCompleted
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionTrue,
		Reason:             "RestoreSucceeded",
		Message:            "etcd cluster restored successfully",
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(restore, restoreFinalizerName)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}

	log.Info(ctx, "restore completed successfully", "cluster", cluster.Name)
	return ctrl.Result{}, nil
}

// handleDeletion ensures cleanup when an EtcdRestore CR is deleted mid-operation.
func (r *EtcdRestoreReconciler) handleDeletion(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(restore, restoreFinalizerName) {
		return ctrl.Result{}, nil
	}

	log.Info(ctx, "handling restore deletion, removing pause annotation")

	// Remove pause annotation from cluster to unblock it
	cluster := &etcdaenixiov1alpha1.EtcdCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.ClusterName,
		Namespace: restore.Namespace,
	}, cluster); err == nil {
		if err := r.removePauseAnnotation(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(restore, restoreFinalizerName)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// failRestore transitions to the Failed phase and removes the pause annotation.
func (r *EtcdRestoreReconciler) failRestore(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	reason error,
) (ctrl.Result, error) {
	log.Error(ctx, reason, "restore failed", "cluster", restore.Spec.ClusterName)

	now := metav1.Now()
	restore.Status.Phase = etcdaenixiov1alpha1.EtcdRestorePhaseFailed
	restore.Status.CompletionTime = &now
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionFalse,
		Reason:             "RestoreFailed",
		Message:            reason.Error(),
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}

	// Always remove pause annotation on failure to avoid stuck cluster
	cluster := &etcdaenixiov1alpha1.EtcdCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.ClusterName,
		Namespace: restore.Namespace,
	}, cluster); err == nil {
		_ = r.removePauseAnnotation(ctx, cluster)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(restore, restoreFinalizerName)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// transitionPhase updates the restore status to a new phase and records the transition time.
func (r *EtcdRestoreReconciler) transitionPhase(
	ctx context.Context,
	restore *etcdaenixiov1alpha1.EtcdRestore,
	phase etcdaenixiov1alpha1.EtcdRestorePhase,
) (ctrl.Result, error) {
	log.Info(ctx, "transitioning restore phase", "from", restore.Status.Phase, "to", phase)
	restore.Status.Phase = phase
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               string(phase),
		Status:             metav1.ConditionTrue,
		Reason:             "PhaseEntered",
		Message:            fmt.Sprintf("Entered phase %s", phase),
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// removePauseAnnotation removes the pause annotation from the EtcdCluster.
func (r *EtcdRestoreReconciler) removePauseAnnotation(
	ctx context.Context,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) error {
	if _, ok := cluster.Annotations[etcdaenixiov1alpha1.EtcdClusterPausedAnnotation]; !ok {
		return nil
	}
	delete(cluster.Annotations, etcdaenixiov1alpha1.EtcdClusterPausedAnnotation)
	return r.Update(ctx, cluster)
}

// isJobComplete returns true if the Job has completed successfully.
func isJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobFailed returns true if the Job has failed.
func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager registers the EtcdRestore controller with the manager.
func (r *EtcdRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdaenixiov1alpha1.EtcdRestore{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

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
	"strings"
	"time"

	etcdaenixiov1alpha1 "github.com/aenix-io/etcd-operator/api/v1alpha1"
	"github.com/aenix-io/etcd-operator/internal/controller/factory"
	"github.com/aenix-io/etcd-operator/internal/log"
	clientv3 "go.etcd.io/etcd/client/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	lastRecoveryAnnotation    = "etcd.aenix.io/last-member-recovery"
	defaultRecoveryCooldownMin = 5
	minRestartCount           = 5
)

// maybeMemberRecovery detects etcd members stuck in CrashLoopBackOff due to
// raft log corruption (e.g. PVC data loss) and automatically recovers them by
// removing the stale member, deleting the PVC + pod, and re-adding the member.
func (r *EtcdClusterReconciler) maybeMemberRecovery(
	ctx context.Context,
	instance *etcdaenixiov1alpha1.EtcdCluster,
	clusterClient *clientv3.Client,
	state *observables,
) error {
	// Only recover established clusters (past first quorum)
	readyCond := meta.FindStatusCondition(instance.Status.Conditions, etcdaenixiov1alpha1.EtcdConditionReady)
	if readyCond == nil || readyCond.Reason == string(etcdaenixiov1alpha1.EtcdCondTypeWaitingForFirstQuorum) {
		return nil
	}

	// Check cooldown
	if ann := instance.Annotations[lastRecoveryAnnotation]; ann != "" {
		lastRecovery, err := time.Parse(time.RFC3339, ann)
		if err != nil {
			log.Error(ctx, err, "failed to parse last-member-recovery annotation, ignoring cooldown", "annotation", ann)
		} else if time.Since(lastRecovery) < time.Duration(defaultRecoveryCooldownMin)*time.Minute {
			log.Debug(ctx, "member recovery cooldown not elapsed", "lastRecovery", ann)
			return nil
		}
	}

	// List pods for this StatefulSet
	podList := &corev1.PodList{}
	labels := factory.NewLabelsBuilder().WithName().WithInstance(instance.Name).WithManagedBy()
	if err := r.List(ctx, podList, client.InNamespace(instance.Namespace), client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	// Find pods in CrashLoopBackOff with sufficient restart count
	var crashingPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isCrashLooping(pod) {
			crashingPod = pod
			break // Only recover one member per reconcile
		}
	}
	if crashingPod == nil {
		return nil
	}

	log.Info(ctx, "detected CrashLoopBackOff pod for potential member recovery",
		"pod", crashingPod.Name,
		"restartCount", getRestartCount(crashingPod))

	// Get the member list from a healthy endpoint
	memberListCtx, cancel := context.WithTimeout(ctx, etcdDefaultTimeout)
	defer cancel()
	memberListResp, err := clusterClient.MemberList(memberListCtx)
	if err != nil {
		return fmt.Errorf("failed to get member list: %w", err)
	}

	// Match crashing pod to a member
	headlessService := factory.GetHeadlessServiceName(instance)
	peerURL := fmt.Sprintf("https://%s.%s.%s.svc:2380", crashingPod.Name, headlessService, instance.Namespace)

	var staleMember *clientv3.Member
	for _, m := range memberListResp.Members {
		for _, url := range m.PeerURLs {
			if url == peerURL {
				staleMember = (*clientv3.Member)(m)
				break
			}
		}
		if staleMember != nil {
			break
		}
		// Also match by name
		if m.Name == crashingPod.Name {
			staleMember = (*clientv3.Member)(m)
			break
		}
	}

	if staleMember == nil {
		log.Info(ctx, "crashing pod does not match any etcd member, skipping recovery",
			"pod", crashingPod.Name, "expectedPeerURL", peerURL)
		return nil
	}

	// Verify quorum safety: after removing this member, healthy members must still form quorum
	totalMembers := len(memberListResp.Members)
	healthyMembers := countHealthyMembers(state)
	// After removal, we need (totalMembers-1)/2+1 healthy members, but only healthyMembers remain
	if healthyMembers <= totalMembers/2 {
		log.Info(ctx, "skipping member recovery: removing member would break quorum",
			"healthyMembers", healthyMembers, "totalMembers", totalMembers)
		return nil
	}

	log.Info(ctx, "starting member recovery",
		"pod", crashingPod.Name,
		"memberID", staleMember.ID,
		"peerURL", peerURL,
		"healthyMembers", healthyMembers,
		"totalMembers", totalMembers)

	// Step 1: Remove stale member
	removeCtx, removeCancel := context.WithTimeout(ctx, etcdDefaultTimeout)
	defer removeCancel()
	if _, err := clusterClient.MemberRemove(removeCtx, staleMember.ID); err != nil {
		return fmt.Errorf("failed to remove stale member %d: %w", staleMember.ID, err)
	}
	log.Info(ctx, "removed stale member", "memberID", staleMember.ID, "pod", crashingPod.Name)

	// Step 2: Delete the PVC
	pvcName := fmt.Sprintf("data-%s", crashingPod.Name)
	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Name = pvcName
	pvc.Namespace = instance.Namespace
	if err := r.Delete(ctx, pvc); err != nil {
		// If PVC doesn't exist, that's fine — it may have already been deleted
		if !strings.Contains(err.Error(), "not found") {
			log.Error(ctx, err, "failed to delete PVC, continuing with recovery", "pvc", pvcName)
		}
	} else {
		log.Info(ctx, "deleted PVC", "pvc", pvcName)
	}

	// Step 3: Delete the pod (StatefulSet controller recreates it)
	if err := r.Delete(ctx, crashingPod); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			log.Error(ctx, err, "failed to delete pod, continuing with recovery", "pod", crashingPod.Name)
		}
	} else {
		log.Info(ctx, "deleted pod", "pod", crashingPod.Name)
	}

	// Step 4: Re-add member
	addCtx, addCancel := context.WithTimeout(ctx, etcdDefaultTimeout)
	defer addCancel()
	if _, err := clusterClient.MemberAdd(addCtx, []string{peerURL}); err != nil {
		return fmt.Errorf("failed to re-add member with peer URL %s: %w", peerURL, err)
	}
	log.Info(ctx, "re-added member", "peerURL", peerURL)

	// Step 5: Update cooldown annotation
	patch := client.MergeFrom(instance.DeepCopy())
	if instance.Annotations == nil {
		instance.Annotations = make(map[string]string)
	}
	instance.Annotations[lastRecoveryAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, instance, patch); err != nil {
		log.Error(ctx, err, "failed to update last-member-recovery annotation")
		return err
	}

	// Step 6: Set status condition for visibility
	metav1Cond := metav1.Condition{
		Type:    etcdaenixiov1alpha1.EtcdConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "MemberRecovery",
		Message: fmt.Sprintf("Recovered member %s (ID %d) from CrashLoopBackOff", crashingPod.Name, staleMember.ID),
	}
	meta.SetStatusCondition(&instance.Status.Conditions, metav1Cond)

	log.Info(ctx, "member recovery completed successfully",
		"pod", crashingPod.Name, "memberID", staleMember.ID)

	return nil
}

// isCrashLooping returns true if the pod has a container in CrashLoopBackOff
// with at least minRestartCount restarts.
func isCrashLooping(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount >= int32(minRestartCount) &&
			cs.State.Waiting != nil &&
			cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// getRestartCount returns the maximum restart count across all containers in a pod.
func getRestartCount(pod *corev1.Pod) int32 {
	var max int32
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	return max
}

// countHealthyMembers counts how many etcd endpoints are responding successfully.
func countHealthyMembers(state *observables) int {
	healthy := 0
	for _, s := range state.etcdStatuses {
		if s.endpointStatus != nil && s.endpointStatusError == nil {
			healthy++
		}
	}
	return healthy
}

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EtcdRestorePhase represents the current phase of the restore operation.
type EtcdRestorePhase string

const (
	EtcdRestorePhasePending        EtcdRestorePhase = "Pending"
	EtcdRestorePhaseScalingDown    EtcdRestorePhase = "ScalingDown"
	EtcdRestorePhasePreparingPVCs  EtcdRestorePhase = "PreparingPVCs"
	EtcdRestorePhaseRestoring      EtcdRestorePhase = "Restoring"
	EtcdRestorePhaseUpdatingConfig EtcdRestorePhase = "UpdatingConfig"
	EtcdRestorePhaseScalingUp      EtcdRestorePhase = "ScalingUp"
	EtcdRestorePhaseCompleted      EtcdRestorePhase = "Completed"
	EtcdRestorePhaseFailed         EtcdRestorePhase = "Failed"
)

// BackupSourcePVC specifies a PVC-based backup source.
type BackupSourcePVC struct {
	// ClaimName is the name of the PVC containing the etcd snapshot.
	ClaimName string `json:"claimName"`
	// SnapshotFile is the path to the snapshot file within the PVC.
	SnapshotFile string `json:"snapshotFile"`
}

// BackupSource specifies where the backup data is located.
type BackupSource struct {
	// PVC specifies a PersistentVolumeClaim backup source.
	// +optional
	PVC *BackupSourcePVC `json:"pvc,omitempty"`
}

// EtcdRestoreSpec defines the desired state of EtcdRestore.
type EtcdRestoreSpec struct {
	// ClusterName is the name of the EtcdCluster to restore.
	ClusterName string `json:"clusterName"`
	// BackupSource specifies the backup to restore from.
	BackupSource BackupSource `json:"backupSource"`
	// StorageClassOverride optionally overrides the StorageClass for data PVCs.
	// When set, existing data PVCs are deleted and recreated with the new StorageClass.
	// +optional
	StorageClassOverride *string `json:"storageClassOverride,omitempty"`
}

// EtcdRestoreStatus defines the observed state of EtcdRestore.
type EtcdRestoreStatus struct {
	// Phase is the current phase of the restore operation.
	Phase EtcdRestorePhase `json:"phase,omitempty"`
	// Conditions represent the latest available observations of the restore's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// StartTime is the time the restore operation started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is the time the restore operation completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// RestoreJobName is the name of the Job performing the snapshot restore.
	// +optional
	RestoreJobName string `json:"restoreJobName,omitempty"`
	// OriginalReplicas stores the original replica count before scaling down.
	// +optional
	OriginalReplicas *int32 `json:"originalReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EtcdRestore is the Schema for the etcdrestores API.
type EtcdRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdRestoreSpec   `json:"spec,omitempty"`
	Status EtcdRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdRestoreList contains a list of EtcdRestore.
type EtcdRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdRestore{}, &EtcdRestoreList{})
}

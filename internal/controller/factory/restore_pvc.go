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

package factory

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdaenixiov1alpha1 "github.com/aenix-io/etcd-operator/api/v1alpha1"
)

// dataPVCName returns the PVC name for a given member index,
// matching the StatefulSet naming convention: {pvcName}-{clusterName}-{index}.
func dataPVCName(cluster *etcdaenixiov1alpha1.EtcdCluster, index int) string {
	return fmt.Sprintf("%s-%s-%d", GetPVCName(cluster), cluster.Name, index)
}

// DeleteDataPVCs deletes all data PVCs for the cluster members.
func DeleteDataPVCs(ctx context.Context, cluster *etcdaenixiov1alpha1.EtcdCluster, cli client.Client) error {
	replicas := int(*cluster.Spec.Replicas)
	for i := 0; i < replicas; i++ {
		pvc := &corev1.PersistentVolumeClaim{}
		name := dataPVCName(cluster, i)
		err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, pvc)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to get PVC %s: %w", name, err)
		}
		if err := cli.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete PVC %s: %w", name, err)
		}
	}
	return nil
}

// AllDataPVCsGone returns true if all data PVCs for the cluster have been deleted.
func AllDataPVCsGone(ctx context.Context, cluster *etcdaenixiov1alpha1.EtcdCluster, cli client.Client) (bool, error) {
	replicas := int(*cluster.Spec.Replicas)
	for i := 0; i < replicas; i++ {
		pvc := &corev1.PersistentVolumeClaim{}
		name := dataPVCName(cluster, i)
		err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, pvc)
		if err == nil {
			return false, nil
		}
		if !errors.IsNotFound(err) {
			return false, fmt.Errorf("failed to check PVC %s: %w", name, err)
		}
	}
	return true, nil
}

// CreateDataPVCs creates data PVCs for all cluster members with the given StorageClass.
// It copies the storage size from the cluster's volume claim template spec.
func CreateDataPVCs(
	ctx context.Context,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
	storageClassName string,
	cli client.Client,
) error {
	replicas := int(*cluster.Spec.Replicas)
	storageSize := cluster.Spec.Storage.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
	if storageSize.IsZero() {
		storageSize = resource.MustParse("1Gi")
	}
	labels := PVCLabels(cluster)

	for i := 0; i < replicas; i++ {
		name := dataPVCName(cluster, i)

		// Check if already exists (idempotent)
		existing := &corev1.PersistentVolumeClaim{}
		err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, existing)
		if err == nil {
			continue
		}
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to check PVC %s: %w", name, err)
		}

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels:    labels,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &storageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: storageSize,
					},
				},
			},
		}

		if err := cli.Create(ctx, pvc); err != nil {
			return fmt.Errorf("failed to create PVC %s: %w", name, err)
		}
	}
	return nil
}

// AllDataPVCsBound returns true if all data PVCs for the cluster are in Bound phase.
func AllDataPVCsBound(ctx context.Context, cluster *etcdaenixiov1alpha1.EtcdCluster, cli client.Client) (bool, error) {
	replicas := int(*cluster.Spec.Replicas)
	for i := 0; i < replicas; i++ {
		pvc := &corev1.PersistentVolumeClaim{}
		name := dataPVCName(cluster, i)
		err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, pvc)
		if err != nil {
			return false, fmt.Errorf("failed to get PVC %s: %w", name, err)
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			return false, nil
		}
	}
	return true, nil
}

// AllDataPVCsExist returns true if all data PVCs for the cluster exist.
func AllDataPVCsExist(ctx context.Context, cluster *etcdaenixiov1alpha1.EtcdCluster, cli client.Client) (bool, error) {
	replicas := int(*cluster.Spec.Replicas)
	for i := 0; i < replicas; i++ {
		pvc := &corev1.PersistentVolumeClaim{}
		name := dataPVCName(cluster, i)
		err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, pvc)
		if errors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to check PVC %s: %w", name, err)
		}
	}
	return true, nil
}

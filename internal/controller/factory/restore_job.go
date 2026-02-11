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
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	etcdaenixiov1alpha1 "github.com/aenix-io/etcd-operator/api/v1alpha1"
)

// getEtcdImage returns the etcd container image from the cluster spec,
// falling back to DefaultEtcdImage if not specified.
func getEtcdImage(cluster *etcdaenixiov1alpha1.EtcdCluster) string {
	for _, c := range cluster.Spec.PodTemplate.Spec.Containers {
		if c.Name == etcdContainerName && c.Image != "" {
			return c.Image
		}
	}
	return etcdaenixiov1alpha1.DefaultEtcdImage
}

// BuildRestoreJob creates a Job that runs etcdutl snapshot restore for each
// cluster member, populating their data PVCs from a snapshot stored on a backup PVC.
func BuildRestoreJob(
	restore *etcdaenixiov1alpha1.EtcdRestore,
	cluster *etcdaenixiov1alpha1.EtcdCluster,
) *batchv1.Job {
	replicas := int(*cluster.Spec.Replicas)
	headlessSvc := GetHeadlessServiceName(cluster)
	clusterService := fmt.Sprintf("%s.%s.svc:2380", headlessSvc, cluster.Namespace)
	image := getEtcdImage(cluster)
	snapshotFile := restore.Spec.BackupSource.PVC.SnapshotFile
	restoreToken := fmt.Sprintf("%s-%s-restore", cluster.Name, cluster.Namespace)

	// Build initial-cluster string
	members := make([]string, replicas)
	for i := 0; i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", cluster.Name, i)
		members[i] = fmt.Sprintf("%s=https://%s.%s", podName, podName, clusterService)
	}
	initialCluster := strings.Join(members, ",")

	// Build restore script (uses /tools/etcdutl copied by init container)
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -e\n")
	for i := 0; i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", cluster.Name, i)
		dataDir := fmt.Sprintf("/data-%d/default.etcd", i)
		peerURL := fmt.Sprintf("https://%s.%s", podName, clusterService)

		// Remove existing data directory if present (idempotent re-run)
		script.WriteString(fmt.Sprintf("rm -rf %s\n", dataDir))
		script.WriteString(fmt.Sprintf(
			"/tools/etcdutl snapshot restore /backup/%s"+
				" --name %s"+
				" --data-dir %s"+
				" --initial-cluster %s"+
				" --initial-advertise-peer-urls %s"+
				" --initial-cluster-token %s"+
				" --skip-hash-check\n",
			snapshotFile, podName, dataDir, initialCluster, peerURL, restoreToken,
		))
		script.WriteString(fmt.Sprintf("echo 'Restored member %s'\n", podName))
	}
	script.WriteString("echo 'All members restored successfully'\n")

	// Volume mounts: backup PVC (read-only) + one data PVC per member
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "backup",
			MountPath: "/backup",
			ReadOnly:  true,
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "backup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: restore.Spec.BackupSource.PVC.ClaimName,
					ReadOnly:  true,
				},
			},
		},
	}

	// Add tools emptyDir volume for sharing etcdutl binary
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "tools",
		MountPath: "/tools",
	})
	volumes = append(volumes, corev1.Volume{
		Name: "tools",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	pvcName := GetPVCName(cluster)
	for i := 0; i < replicas; i++ {
		volName := fmt.Sprintf("data-%d", i)
		claimName := fmt.Sprintf("%s-%s-%d", pvcName, cluster.Name, i)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: fmt.Sprintf("/data-%d", i),
		})
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				},
			},
		})
	}

	backoffLimit := int32(3)
	jobName := fmt.Sprintf("%s-restore", restore.Name)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: restore.Namespace,
			Labels:    NewLabelsBuilder().WithName().WithInstance(cluster.Name).WithManagedBy(),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					InitContainers: []corev1.Container{
						{
							Name:    "copy-etcdutl",
							Image:   image,
							Command: []string{"cp", "/usr/local/bin/etcdutl", "/tools/etcdutl"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "tools", MountPath: "/tools"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:         "restore",
							Image:        "alpine:3.20",
							Command:      []string{"/bin/sh", "-c", script.String()},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	return job
}

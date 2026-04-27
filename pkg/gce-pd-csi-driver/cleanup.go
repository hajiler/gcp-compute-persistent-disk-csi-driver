package gceGCEDriver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/api/googleapi"
	kubeApiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/common"
	"sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/constants"
	gcecloudprovider "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/pkg/gce-cloud-provider/compute"
)

const (
	selfLinkPrefix = "https://www.googleapis.com/compute/v1/"

	pvcNamespaceKey = "kubernetes.io/created-for/pvc/namespace"
	pvcNameKey      = "kubernetes.io/created-for/pvc/name"
	createdByKey    = "storage.gke.io/created-by"
)

func labelFilter(key, value string) string {
	return fmt.Sprintf("labels.%s=%s", key, value)
}

func (gceCS *GCEControllerServer) CleanupRoutine(ctx context.Context) error {
	klog.V(4).Infof("Starting disk leak cleanup cycle")

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get Kubernetes config: %w", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	clusterFilter := labelFilter(constants.ClusterIDLabel, gceCS.clusterID)
	statusFilter := labelFilter(constants.VolumePublishStatus, constants.ProvisioningStatus)
	filter := fmt.Sprintf("%s AND %s", clusterFilter, statusFilter)
	disks, _, err := gceCS.CloudProvider.ListDisksWithFilter(ctx, []googleapi.Field{}, filter)
	if err != nil {
		return fmt.Errorf("failed to list disks: %w", err)
	}
	klog.V(4).Infof("Listed %d disks with filter: %s", len(disks), filter)

	for _, disk := range disks {
		volumeID := strings.TrimPrefix(disk.SelfLink, selfLinkPrefix)
		project, volKey, err := common.VolumeIDToKey(volumeID)
		if err != nil {
			return fmt.Errorf("failed to parse volume ID %s: %w", volumeID, err)
		}

		var tags map[string]string
		if err := json.Unmarshal([]byte(disk.Description), &tags); err != nil {
			return fmt.Errorf("failed to unmarshal disk description for disk: %w", err)
		}
		// Skip disks not created by the driver.
		if tags[createdByKey] != constants.DriverName {
			return nil
		}

		name := tags[pvcNameKey]
		namespace := tags[pvcNamespaceKey]
		_, err = kc.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if kubeApiErrors.IsNotFound(err) {
				klog.Warningf("Deleting leaked disk %s", volumeID)
				err = gceCS.CloudProvider.DeleteDisk(ctx, project, volKey)
				if err != nil {
					klog.Errorf("Failed to delete leaked disk %v: %v", volKey, err)
				}
				continue
			}
			klog.Errorf("Failed to get pvc %s/%s: %v", namespace, name, err)
			continue
		}

		disk.Labels[constants.VolumePublishStatus] = constants.ProvisionedStatus
		err = gceCS.CloudProvider.SetDiskLabels(ctx, project, volKey, gcecloudprovider.CloudDiskFromV1(disk), disk.Labels)
		if err != nil {
			klog.Warningf("failed to update status label for %s: %v", volKey, err)
		}
	}
	return nil
}

package gceGCEDriver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	kubeApiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"google.golang.org/api/googleapi"
)

func (gceCS *GCEControllerServer) CleanupRoutine(ctx context.Context) {
	if gceCS.KubeClient == nil {
		klog.Warningf("CleanupRoutine disabled because KubeClient is not configured")
		return
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	// Initial wait
	klog.V(2).Infof("CleanupRoutine started. Next run in 10 minutes.")

	for {
		select {
		case <-ctx.Done():
			klog.Infof("CleanupRoutine stopping")
			return
		case <-ticker.C:
			gceCS.runCleanupCycle(ctx)
		}
	}
}

func (gceCS *GCEControllerServer) runCleanupCycle(ctx context.Context) {
	klog.V(4).Infof("Starting disk leak cleanup cycle")

	gceCS.LeakMutex.Lock()
	defer gceCS.LeakMutex.Unlock()

	project := gceCS.CloudProvider.GetDefaultProject()
	var emptyFields []googleapi.Field
	disks, _, err := gceCS.CloudProvider.ListDisks(ctx, emptyFields)
	if err != nil {
		klog.Errorf("Failed to list disks for cleanup: %v", err)
		return
	}

	for _, disk := range disks {
		if disk.Description == "" {
			continue
		}

		var tags map[string]string
		if err := json.Unmarshal([]byte(disk.Description), &tags); err != nil {
			continue
		}

		pvcName := tags["kubernetes.io/created-for/pvc/name"]
		pvcNamespace := tags["kubernetes.io/created-for/pvc/namespace"]

		if pvcName == "" || pvcNamespace == "" {
			continue
		}

		if disk.Labels != nil && disk.Labels["pvc-exists"] == "true" {
			continue
		}

		klog.V(4).Infof("Checking PVC %s/%s for unlabeled disk %s", pvcNamespace, pvcName, disk.Name)
		_, err = gceCS.KubeClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
		
		var volKey *meta.Key
		if disk.Zone != "" {
			zoneIdx := strings.LastIndex(disk.Zone, "/")
			zone := disk.Zone
			if zoneIdx != -1 {
				zone = disk.Zone[zoneIdx+1:]
			}
			volKey = meta.ZonalKey(disk.Name, zone)
		} else if disk.Region != "" {
			regionIdx := strings.LastIndex(disk.Region, "/")
			region := disk.Region
			if regionIdx != -1 {
				region = disk.Region[regionIdx+1:]
			}
			volKey = meta.RegionalKey(disk.Name, region)
		} else {
			klog.Errorf("Unable to determine zone/region for leaked disk %s", disk.Name)
			continue
		}

		if err != nil {
			if kubeApiErrors.IsNotFound(err) {
				klog.Warningf("PVC %s/%s missing. Deleting leaked disk %s", pvcNamespace, pvcName, disk.Name)
				err = gceCS.CloudProvider.DeleteDisk(ctx, project, volKey)
				if err != nil {
					klog.Errorf("Failed to delete leaked disk %v: %v", volKey, err)
				}
				continue
			}
			klog.Errorf("Error checking PVC %s/%s: %v", pvcNamespace, pvcName, err)
			continue
		}

		klog.V(4).Infof("PVC %s/%s verified. Labeling disk %s with pvc-exists: true", pvcNamespace, pvcName, disk.Name)
		err = gceCS.CloudProvider.SetDiskLabels(ctx, project, volKey, map[string]string{"pvc-exists": "true"})
		if err != nil {
			klog.Warningf("Failed to set pvc-exists label on disk %v: %v", volKey, err)
		}
	}
	klog.V(4).Infof("Cleanup cycle completed")
}

package rkecontrolplancondition

import (
	"context"
	"fmt"

	v2 "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	catalogv1 "github.com/rancher/rancher/pkg/generated/controllers/catalog.cattle.io/v1"
	rocontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	rkecontrollers "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
)

type handler struct {
	clusterName               string
	appCache                  catalogv1.AppCache
	upstreamProvClustersCache rocontrollers.ClusterCache
}

func Register(ctx context.Context, context *config.UserContext) {
	logrus.Infof("==== [rkecontrolplancondition] registering system agent controller")
	mgmtWrangler := context.Management.Wrangler

	h := handler{
		clusterName:               context.ClusterName,
		appCache:                  context.Catalog.V1().App().Cache(),
		upstreamProvClustersCache: mgmtWrangler.Provisioning.Cluster().Cache(),
	}

	rkecontrollers.RegisterRKEControlPlaneStatusHandler(ctx, mgmtWrangler.RKE.RKEControlPlane(),
		"", "sync-system-upgrade-controller-status", h.syncSystemUpgradeControllerStatus)
}

// syncSystemUpgradeControllerStatus queries the managed system-upgrade-controller chart and determines if it is properly configured for a given
// version of Kubernetes. It applies a condition onto the control-plane object to be used by the planner when handling Kubernetes upgrades.
func (h *handler) syncSystemUpgradeControllerStatus(obj *rkev1.RKEControlPlane, status rkev1.RKEControlPlaneStatus) (rkev1.RKEControlPlaneStatus, error) {
	// v1cluster, err := h.upstreamProvClustersCache.Get(obj.Namespace, obj.Name)
	// if err != nil {
	// 	return status, err
	// }
	// if v1cluster.Status.ClusterName != h.clusterName {
	// 	return status, err
	// }
	if obj.Name != h.clusterName {
		return status, nil
	}
	logrus.Infof("==== [rkecontrolplancondition] sync staus for cluster %s", h.clusterName)

	app, err := h.appCache.Get("cattle-system", "system-upgrade-controller")
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Infof("==== [rkecontrolplancondition] suc app is not found on cluster %s", h.clusterName)
			// if we couldn't find the app then we know it's not ready
			capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("unable to find system-upgrade-controller app: %v", err))
			capr.SystemUpgradeControllerReady.Message(&status, "")
			capr.SystemUpgradeControllerReady.False(&status)
			// don't return the error, otherwise the status won't be set to 'false'
			return status, nil
		}
		logrus.Errorf("==== [rkecontrolplancondition] rkecluster %s/%s: error encountered while retrieving app %s: %v", obj.Namespace, obj.Name, "system-upgrade-controller", err)
		return status, err
	}
	targetVersion := settings.SystemUpgradeControllerChartVersion.Get()
	if app.Spec.Chart.Metadata.Version != targetVersion && targetVersion != "" {
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for system-upgrade-controller app to update to the latest version %s", targetVersion))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.False(&status)
		return status, nil
	}
	switch {
	case app.Status.Summary.State == string(v2.StatusDeployed):
		capr.SystemUpgradeControllerReady.Reason(&status, "")
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.True(&status)
	case app.Status.Summary.Error:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("failed to install system-upgrade-controlle app (current state: %s)", app.Status.Summary.State))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.False(&status)
	case app.Status.Summary.Transitioning:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for system-upgrade-controller app roll out (current state: %s)", app.Status.Summary.State))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.Unknown(&status)
	default:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for system-upgrade-controller app roll out (current state: %s)", app.Status.Summary.State))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.Unknown(&status)
	}
	return status, nil
}

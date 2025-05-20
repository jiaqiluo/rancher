package rkecontrolplanecondition

import (
	"context"
	"fmt"

	catalog "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	catalogv1 "github.com/rancher/rancher/pkg/generated/controllers/catalog.cattle.io/v1"
	rkecontrollers "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/types/config"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type handler struct {
	mgmtClusterName         string
	downstreamAppController catalogv1.AppClient
}

func Register(ctx context.Context, context *config.UserContext) {
	mgmtWrangler := context.Management.Wrangler

	h := handler{
		mgmtClusterName:         context.ClusterName,
		downstreamAppController: context.Catalog.V1().App(),
	}

	rkecontrollers.RegisterRKEControlPlaneStatusHandler(ctx, mgmtWrangler.RKE.RKEControlPlane(),
		"", "sync-system-upgrade-controller-status", h.syncSystemUpgradeControllerStatus)
}

// syncSystemUpgradeControllerStatus checks the status of the system-upgrade-controller app in the downstream cluster
// and sets a condition on the control-plane object
func (h *handler) syncSystemUpgradeControllerStatus(obj *rkev1.RKEControlPlane, status rkev1.RKEControlPlaneStatus) (rkev1.RKEControlPlaneStatus, error) {
	if obj == nil || obj.DeletionTimestamp != nil {
		return status, nil
	}
	if obj.Spec.ManagementClusterName != h.mgmtClusterName {
		return status, nil
	}

	name := appName(obj.Spec.ClusterName)
	app, err := h.downstreamAppController.Get(namespace.System, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// if we couldn't find the app then we know it's not ready
			capr.SystemUpgradeControllerReady.Reason(&status, err.Error())
			capr.SystemUpgradeControllerReady.Message(&status, "")
			capr.SystemUpgradeControllerReady.False(&status)
			// don't return the error, otherwise the status won't be set to 'false'
			return status, nil
		}
		return status, fmt.Errorf("[rkecontrolplanecondition] failed to get app %s: %v", name, err)
	}
	if app == nil {
		return status, nil
	}
	if app.DeletionTimestamp != nil {
		// if we couldn't find the app then we know it's not ready
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("the app %s is uninstalled", app.Name))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.False(&status)
		return status, nil
	}

	targetVersion := settings.SystemUpgradeControllerChartVersion.Get()
	version := app.Spec.Chart.Metadata.Version
	if version != targetVersion && targetVersion != "" {
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for %s to update to version %s", app.Name, targetVersion))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.False(&status)
		return status, nil
	}

	state := app.Status.Summary.State
	switch {
	case state == string(catalog.StatusDeployed):
		capr.SystemUpgradeControllerReady.Reason(&status, "")
		capr.SystemUpgradeControllerReady.Message(&status, fmt.Sprintf("deployed chart version: %s", version))
		capr.SystemUpgradeControllerReady.True(&status)
	case app.Status.Summary.Error:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("failed to install %s: %s", app.Name, state))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.False(&status)
	case app.Status.Summary.Transitioning:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for %s to roll out: %s", app.Name, state))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.Unknown(&status)
	default:
		capr.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("waiting for %s to roll out: %s", app.Name, state))
		capr.SystemUpgradeControllerReady.Message(&status, "")
		capr.SystemUpgradeControllerReady.Unknown(&status)
	}
	return status, nil
}

func appName(clusterName string) string {
	return capr.SafeConcatName(capr.MaxHelmReleaseNameLength, "mcc",
		capr.SafeConcatName(48, clusterName, "managed", "system-upgrade-controller"))
}

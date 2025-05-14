package managesystemagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Masterminds/semver/v3"
	rancherv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	fleetconst "github.com/rancher/rancher/pkg/fleet"
	fleetcontrollers "github.com/rancher/rancher/pkg/generated/controllers/fleet.cattle.io/v1alpha1"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	v1 "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	namespaces "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/provisioningv2/image"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemtemplate"
	"github.com/rancher/rancher/pkg/wrangler"
	upgradev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/apply"
	corev1controllers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	generationSecretName                  = "system-agent-upgrade-generation"
	upgradeAPIVersion                     = "upgrade.cattle.io/v1"
	upgradeDigestAnnotation               = "upgrade.cattle.io/digest"
	systemAgentUpgraderServiceAccountName = "system-agent-upgrader"
	appliedSystemAgentHashAnnotation      = "rke.cattle.io/applied-system-agent-hash"
)

var (
	Kubernetes125 = semver.MustParse("v1.25.0")

	// GH5551FixedVersions is a slice of rke2 versions
	// which have resolved GH-5551 for Windows nodes.
	// ref:  https://github.com/rancher/rke2/issues/5551
	// The SUC should not deploy plans to Windows nodes
	// running a version less than the below for each minor.
	// This check can be removed when 1.31.x is the lowest supported
	// rke2 version.
	GH5551FixedVersions = map[int]*semver.Version{
		30: semver.MustParse("v1.30.4"),
		29: semver.MustParse("v1.29.8"),
		28: semver.MustParse("v1.28.13"),
		27: semver.MustParse("v1.27.16"),
	}

	// globalCounter keeps track of the number of clusters for which the handler is concurrently uninstalling the Fleet-based app.
	// An atomic integer is used for efficiency, as it is lighter than a traditional lock.
	globalCounter atomic.Int32
)

type handler struct {
	clusterRegistrationTokens v3.ClusterRegistrationTokenCache
	bundles                   fleetcontrollers.BundleController
	provClusters              rocontrollers.ClusterCache
	controlPlanes             v1.RKEControlPlaneController
	managedCharts             v3.ManagedChartController
	secrets                   corev1controllers.SecretController
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := &handler{
		clusterRegistrationTokens: clients.Mgmt.ClusterRegistrationToken().Cache(),
		bundles:                   clients.Fleet.Bundle(),
		provClusters:              clients.Provisioning.Cluster().Cache(),
		controlPlanes:             clients.RKE.RKEControlPlane(),
		managedCharts:             clients.Mgmt.ManagedChart(),
		secrets:                   clients.Core.Secret(),
	}

	// v1.RegisterRKEControlPlaneStatusHandler(ctx, clients.RKE.RKEControlPlane(),
	// 	"", "monitor-system-upgrade-controller-readiness", h.syncSystemUpgradeControllerStatus)

	clients.Provisioning.Cluster().OnChange(ctx, "uninstall-fleet-managed-suc-and-system-agent", h.UninstallFleetBasedApps)
	clients.Provisioning.Cluster().OnChange(ctx, "install-system-agent", h.InstallSystemAgent)

}
func (h *handler) InstallSystemAgent(_ string, cluster *rancherv1.Cluster) (*rancherv1.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return cluster, nil
	}
	if cluster.Spec.RKEConfig == nil || settings.SystemAgentUpgradeImage.Get() == "" {
		return cluster, nil
	}
	// skip if the cluster is undergoing an upgrade or not in the ready state
	if !capr.Updated.IsTrue(cluster) || !capr.Provisioned.IsTrue(cluster) || !capr.Ready.IsTrue(cluster) {
		return cluster, nil
	}
	// skip if the cluster's kubeconfig is not populated
	if cluster.Status.ClientSecretName == "" {
		return cluster, nil
	}

	cp, err := h.controlPlanes.Cache().Get(cluster.Namespace, cluster.Name)
	if err != nil {
		logrus.Errorf("==== [managed-system-agent] Error encountered getting RKE control plane while determining SUC readiness: %v", err)
		return cluster, err
	}

	// skip if the SUC app is not ready,
	// because plans may depend on functionality of a newer SUC version
	if !capr.SystemUpgradeControllerReady.IsTrue(cp) {
		logrus.Debugf("==== [managed-system-agent] the SUC is not yet ready, waiting to create system agent upgrade plans (SUC status: %s)", capr.SystemUpgradeControllerReady.GetStatus(cp))
		return cluster, nil
	}

	logrus.Infof("==== [managed-system-agent] attempt to Install System-Agent on %s", cluster.Name)
	var (
		secretName = "stv-aggregation"
		resources  []runtime.Object
	)

	if cluster.Status.ClusterName == "local" && cluster.Namespace == fleetconst.ClustersLocalNamespace {
		secretName += "-local-"

		token, err := h.clusterRegistrationTokens.Get(cluster.Status.ClusterName, "default-token")
		if err != nil {
			return cluster, err
		}
		if token.Status.Token == "" {
			return cluster, fmt.Errorf("token not yet generated for %s/%s", token.Namespace, token.Name)
		}

		digest := sha256.New()
		digest.Write([]byte(settings.InternalServerURL.Get()))
		digest.Write([]byte(token.Status.Token))
		digest.Write([]byte(systemtemplate.InternalCAChecksum()))
		d := digest.Sum(nil)
		secretName += hex.EncodeToString(d[:])[:12]

		resources = append(resources, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespaces.System,
			},
			Data: map[string][]byte{
				"CATTLE_SERVER":      []byte(settings.InternalServerURL.Get()),
				"CATTLE_TOKEN":       []byte(token.Status.Token),
				"CATTLE_CA_CHECKSUM": []byte(systemtemplate.InternalCAChecksum()),
			},
		})
	}

	resources = append(resources, installer(cluster, secretName)...)

	// Caculate a hash value of the templates
	data, err := json.Marshal(resources)
	if err != nil {
		return cluster, err
	}
	hash := sha256.Sum256(data)
	b64 := base64.StdEncoding.EncodeToString(hash[:])
	shortHash := b64[:12]

	val, _ := cp.Annotations[appliedSystemAgentHashAnnotation]
	if shortHash == val {
		logrus.Infof("=== [managed-system-agent] applied templates on cluster %s is already up-to-date", cluster.Name)
		return cluster, nil
	}

	// construct an Apply object
	kcSecret, err := h.secrets.Cache().Get(cluster.Namespace, cluster.Status.ClientSecretName)
	if err != nil {
		if errors.IsNotFound(err) {
			return cluster, nil
		}
		return cluster, err
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kcSecret.Data["value"])
	if err != nil {
		return cluster, err
	}
	apply, err := apply.NewForConfig(restConfig)
	if err != nil {
		return cluster, err
	}
	logrus.Infof("==== [managed-system-agent] use Apply to create reseouces on %s", cluster.Name)

	err = apply.
		WithSetID("managed-system-agent").
		WithDynamicLookup().
		WithListerNamespace(namespaces.System).
		WithDefaultNamespace(namespaces.System).
		ApplyObjects(resources...)
	if err != nil {
		logrus.Infof("==== [managed-system-agent] Apply return errors: %s", err.Error())
		return cluster, err
	}

	// update the annotation
	cp = cp.DeepCopy()
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations[appliedSystemAgentHashAnnotation] = shortHash
	if _, err := h.controlPlanes.Update(cp); err != nil {
		logrus.Infof("==== [managed-system-agent] failed to update the annotation on the controlPlane: %s", err.Error())
		return cluster, err
	}

	return cluster, nil
}

func installer(cluster *rancherv1.Cluster, secretName string) []runtime.Object {
	upgradeImage := strings.SplitN(settings.SystemAgentUpgradeImage.Get(), ":", 2)
	version := "latest"
	if len(upgradeImage) == 2 {
		version = upgradeImage[1]
	}

	var env []corev1.EnvVar
	for _, e := range cluster.Spec.AgentEnvVars {
		env = append(env, corev1.EnvVar{
			Name:  e.Name,
			Value: e.Value,
		})
	}

	// Merge the env vars with the AgentTLSModeStrict
	found := false
	for _, ev := range env {
		if ev.Name == "STRICT_VERIFY" {
			found = true // The user has specified `STRICT_VERIFY`, we should not attempt to overwrite it.
		}
	}
	if !found {
		if settings.AgentTLSMode.Get() == settings.AgentTLSModeStrict {
			env = append(env, corev1.EnvVar{
				Name:  "STRICT_VERIFY",
				Value: "true",
			})
		} else {
			env = append(env, corev1.EnvVar{
				Name:  "STRICT_VERIFY",
				Value: "false",
			})
		}
	}

	if len(cluster.Spec.RKEConfig.MachineSelectorConfig) == 0 {
		env = append(env, corev1.EnvVar{
			Name:  "CATTLE_ROLE_WORKER",
			Value: "true",
		})
	}

	if cluster.Spec.RKEConfig.DataDirectories.SystemAgent != "" {
		env = append(env, corev1.EnvVar{
			Name:  capr.SystemAgentDataDirEnvVar,
			Value: capr.GetSystemAgentDataDir(&cluster.Spec.RKEConfig.RKEClusterSpecCommon),
		})
	}
	var plans []runtime.Object

	plan := &upgradev1.Plan{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Plan",
			APIVersion: upgradeAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      systemAgentUpgraderServiceAccountName,
			Namespace: namespaces.System,
			Annotations: map[string]string{
				upgradeDigestAnnotation: "spec.upgrade.envs,spec.upgrade.envFrom",
			},
		},
		Spec: upgradev1.PlanSpec{
			Concurrency: 10,
			Version:     version,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			},
			},
			NodeSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      corev1.LabelOSStable,
						Operator: metav1.LabelSelectorOpIn,
						Values: []string{
							"linux",
						},
					},
				},
			},
			ServiceAccountName: systemAgentUpgraderServiceAccountName,
			// envFrom is still the source of CATTLE_ vars in plan, however secrets will trigger an update when changed.
			Secrets: []upgradev1.SecretSpec{
				{
					Name: "stv-aggregation",
				},
			},
			Upgrade: &upgradev1.ContainerSpec{
				Image: image.ResolveWithCluster(upgradeImage[0], cluster),
				Env:   env,
				EnvFrom: []corev1.EnvFromSource{{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secretName,
						},
					},
				}},
			},
		},
	}
	plans = append(plans, plan)

	if CurrentVersionResolvesGH5551(cluster.Spec.KubernetesVersion) {
		windowsPlan := winsUpgradePlan(cluster, env, secretName)
		if cluster.Spec.RedeploySystemAgentGeneration != 0 {
			windowsPlan.Spec.Secrets = append(windowsPlan.Spec.Secrets, upgradev1.SecretSpec{
				Name: generationSecretName,
			})
		}
		plans = append(plans, windowsPlan)
	}

	objs := []runtime.Object{
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      systemAgentUpgraderServiceAccountName,
				Namespace: namespaces.System,
			},
		},
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: systemAgentUpgraderServiceAccountName,
			},
			Rules: []rbacv1.PolicyRule{{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"nodes"},
			}},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: systemAgentUpgraderServiceAccountName,
			},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      systemAgentUpgraderServiceAccountName,
				Namespace: namespaces.System,
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     systemAgentUpgraderServiceAccountName,
			},
		},
	}

	// The stv-aggregation secret is managed separately, and SUC will trigger a plan upgrade automatically when the
	// secret is updated. This prevents us from having to manually update the plan every time the secret changes
	// (which is not often, and usually never).
	if cluster.Spec.RedeploySystemAgentGeneration != 0 {
		plan.Spec.Secrets = append(plan.Spec.Secrets, upgradev1.SecretSpec{
			Name: generationSecretName,
		})

		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      generationSecretName,
				Namespace: namespaces.System,
			},
			StringData: map[string]string{
				"cluster-uid": string(cluster.UID),
				"generation":  strconv.Itoa(int(cluster.Spec.RedeploySystemAgentGeneration)),
			},
		})
	}

	return append(plans, objs...)
}

func winsUpgradePlan(cluster *rancherv1.Cluster, env []corev1.EnvVar, secretName string) *upgradev1.Plan {
	winsUpgradeImage := strings.SplitN(settings.WinsAgentUpgradeImage.Get(), ":", 2)
	winsVersion := "latest"
	if len(winsUpgradeImage) == 2 {
		winsVersion = winsUpgradeImage[1]
	}

	return &upgradev1.Plan{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Plan",
			APIVersion: upgradeAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "system-agent-upgrader-windows",
			Namespace: namespaces.System,
			Annotations: map[string]string{
				upgradeDigestAnnotation: "spec.upgrade.envs,spec.upgrade.envFrom",
			},
		},
		Spec: upgradev1.PlanSpec{
			Concurrency: 10,
			Version:     winsVersion,
			Tolerations: []corev1.Toleration{
				{
					Operator: corev1.TolerationOpExists,
				},
			},
			NodeSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      corev1.LabelOSStable,
						Operator: metav1.LabelSelectorOpIn,
						Values: []string{
							"windows",
						},
					},
				},
			},
			ServiceAccountName: systemAgentUpgraderServiceAccountName,
			Upgrade: &upgradev1.ContainerSpec{
				Image: image.ResolveWithCluster(winsUpgradeImage[0], cluster),
				Env:   env,
				SecurityContext: &corev1.SecurityContext{
					WindowsOptions: &corev1.WindowsSecurityContextOptions{
						HostProcess:   toBoolPointer(true),
						RunAsUserName: toStringPointer("NT AUTHORITY\\SYSTEM"),
					},
				},
				EnvFrom: []corev1.EnvFromSource{{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secretName,
						},
					},
				}},
			},
		},
	}
}

func toBoolPointer(x bool) *bool {
	return &x
}

func toStringPointer(x string) *string {
	return &x
}

// CurrentVersionResolvesGH5551 determines if the given rke2 version
// has fixed the RKE2 bug outlined in GH-5551. Windows SUC plans cannot be delivered
// to clusters running versions containing this bug. This function can be removed
// when v1.31.x is the lowest supported version offered by Rancher.
func CurrentVersionResolvesGH5551(version string) bool {

	// remove leading v and trailing distro identifier
	v := strings.TrimPrefix(version, "v")
	verSplit := strings.Split(v, "+")
	if len(verSplit) != 2 {
		return false
	}

	curSemVer, err := semver.NewVersion(verSplit[0])
	if err != nil {
		return false
	}

	minor := curSemVer.Minor()
	if minor >= 31 {
		return true
	}
	if minor <= 26 {
		return false
	}

	return curSemVer.GreaterThanEqual(GH5551FixedVersions[int(minor)])
}

func (h *handler) UninstallFleetBasedApps(_ string, cluster *rancherv1.Cluster) (*rancherv1.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return cluster, nil
	}
	if cluster.Spec.RKEConfig == nil {
		return cluster, nil
	}
	// the absence of the FleetWorkspaceName indicates that Fleet is not ready on the cluster, so do Fleet bundles
	if cluster.Status.FleetWorkspaceName == "" {
		return cluster, nil
	}
	// skip if the cluster is undergoing an upgrade or not in the ready state
	if !(capr.Updated.IsTrue(cluster) && capr.Provisioned.IsTrue(cluster) && capr.Ready.IsTrue(cluster)) {
		return cluster, nil
	}
	if globalCounter.Load() < int32(settings.K3sBasedUpgraderUninstallConcurrency.GetInt()) {
		globalCounter.Add(1)
		defer globalCounter.Add(-1)
		// Step 1: uninstall the system-agent bundle
		logrus.Infof("==== [managed-system-agent] attempt to uninstall the bundle [%s] on %s", systemAgentAppName(cluster.Name), cluster.Name)
		bundle, err := h.bundles.Cache().Get(cluster.Status.FleetWorkspaceName, systemAgentAppName(cluster.Name))
		if err != nil {
			if errors.IsNotFound(err) {
				// todo: change to debug-level
				logrus.Infof("==== [managed-system-agent] system agent bundle does not exist")
			} else {
				return nil, err
			}
		}
		if bundle != nil {
			if err := h.bundles.Delete(bundle.Namespace, bundle.Name, &metav1.DeleteOptions{}); err != nil {
				logrus.Errorf("==== [managed-system-agent] hit errors: %v", err)
				if !errors.IsNotFound(err) {
					return nil, err
				}
			}
			logrus.Infof("==== [managed-system-agent] system-agent bunddles is deleted successfully")
		}

		// step 2: uninstall the system-upgrade-controller managedChart(which is translated into a Fleet Bundle)
		sucName := systemUpgradeControllerAppName(cluster.Name)
		logrus.Infof("==== [managed-system-agent] attempt to uninstall SUC managed chart [%s] on %s", sucName, cluster.Name)
		managedChart, err := h.managedCharts.Cache().Get(cluster.Status.FleetWorkspaceName, sucName)
		if err != nil {
			if errors.IsNotFound(err) {
				// todo: change to debug-level
				logrus.Infof("==== [managed-system-agent] system-upgrade-controller managed chart does not exist")
			} else {
				return nil, err
			}
		}
		if managedChart != nil {
			if err := h.managedCharts.Delete(managedChart.Namespace, managedChart.Name, &metav1.DeleteOptions{}); err != nil {
				logrus.Errorf("==== [managed-system-agent] hit errors: %v", err)
				if !errors.IsNotFound(err) {
					return nil, err
				}
			}
			logrus.Infof("==== [managed-system-agent] system-upgrade-controller managed charts is deleted successfully")
		}
	}
	return cluster, nil
}

func systemAgentAppName(clusterName string) string {
	return capr.SafeConcatName(capr.MaxHelmReleaseNameLength, clusterName, "managed", "system", "agent")
}

func systemUpgradeControllerAppName(clusterName string) string {
	// we must limit the output of name.SafeConcatName to at most 48 characters because
	// a) the chart release name cannot exceed 53 characters, and
	// b) upon creation of this resource the prefix 'mcc-' will be added to the release name, hence the limiting to 48 characters
	return capr.SafeConcatName(48, clusterName, "managed", "system-upgrade-controller")
}

package managesystemagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	rancherv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	fleetconst "github.com/rancher/rancher/pkg/fleet"
	fleetcontrollers "github.com/rancher/rancher/pkg/generated/controllers/fleet.cattle.io/v1alpha1"
	mgmtcontollers "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	mgmtv3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	rkecontrollers "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	namespaces "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/provisioningv2/image"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemtemplate"
	"github.com/rancher/rancher/pkg/wrangler"
	upgradev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/apply"
	wranglercore "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	generationSecretName                  = "system-agent-upgrade-generation"
	upgradeAPIVersion                     = "upgrade.cattle.io/v1"
	upgradeDigestAnnotation               = "upgrade.cattle.io/digest"
	systemAgentUpgraderServiceAccountName = "system-agent-upgrader"
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
)

type handler struct {
	clusterRegistrationTokens mgmtv3.ClusterRegistrationTokenCache
	bundles                   fleetcontrollers.BundleClient
	provClusters              rocontrollers.ClusterCache
	managedCharts             mgmtcontollers.ManagedChartController
	controlPlanesCache        rkecontrollers.RKEControlPlaneCache
	secrets                   wranglercore.SecretController
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := &handler{
		clusterRegistrationTokens: clients.Mgmt.ClusterRegistrationToken().Cache(),
		bundles:                   clients.Fleet.Bundle(),
		provClusters:              clients.Provisioning.Cluster().Cache(),
		managedCharts:             clients.Mgmt.ManagedChart(),
		controlPlanesCache:        clients.RKE.RKEControlPlane().Cache(),
		secrets:                   clients.Core.Secret(),
	}

	clients.Provisioning.Cluster().OnChange(ctx, "uninstall-suc-managed-chart", h.OnChangeUninstallSUCManagedChart)
	clients.Provisioning.Cluster().OnChange(ctx, "uninstall-system-agent-bundle", h.OnChangeUninstallSystemAgentBundle)
	clients.Provisioning.Cluster().OnChange(ctx, "install-system-agent", h.OnChangeInstallSystemAgent)

}

func installer(cluster *rancherv1.Cluster, secretName, releaseName string) []runtime.Object {
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
				"meta.helm.sh/release-name":      releaseName,
				"meta.helm.sh/release-namespace": namespaces.System,
				upgradeDigestAnnotation:          "spec.upgrade.envs,spec.upgrade.envFrom",
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
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
		windowsPlan := winsUpgradePlan(cluster, env, secretName, releaseName)
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
				Annotations: map[string]string{
					"meta.helm.sh/release-name":      releaseName,
					"meta.helm.sh/release-namespace": namespaces.System,
				},
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "Helm",
				},
			},
		},
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: systemAgentUpgraderServiceAccountName,
				Annotations: map[string]string{
					"meta.helm.sh/release-name":      releaseName,
					"meta.helm.sh/release-namespace": namespaces.System,
				},
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "Helm",
				},
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
				Annotations: map[string]string{
					"meta.helm.sh/release-name":      releaseName,
					"meta.helm.sh/release-namespace": namespaces.System,
				},
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "Helm",
				},
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

	if cluster.Spec.RedeploySystemAgentGeneration != 0 {
		plan.Spec.Secrets = append(plan.Spec.Secrets, upgradev1.SecretSpec{
			Name: generationSecretName,
		})

		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      generationSecretName,
				Namespace: namespaces.System,
				Annotations: map[string]string{
					"meta.helm.sh/release-name":      releaseName,
					"meta.helm.sh/release-namespace": namespaces.System,
				},
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "Helm",
				},
			},
			StringData: map[string]string{
				"cluster-uid": string(cluster.UID),
				"generation":  strconv.Itoa(int(cluster.Spec.RedeploySystemAgentGeneration)),
			},
		})
	}

	return append(plans, objs...)
}

func winsUpgradePlan(cluster *rancherv1.Cluster, env []corev1.EnvVar, secretName, releaseName string) *upgradev1.Plan {
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
				"meta.helm.sh/release-name":      releaseName,
				"meta.helm.sh/release-namespace": namespaces.System,
				upgradeDigestAnnotation:          "spec.upgrade.envs,spec.upgrade.envFrom",
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
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

func (h *handler) OnChangeUninstallSUCManagedChart(_ string, cluster *rancherv1.Cluster) (*rancherv1.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return cluster, nil
	}
	if cluster.Spec.RKEConfig == nil {
		return cluster, nil
	}
	if cluster.Status.FleetWorkspaceName == "" {
		return cluster, nil
	}
	sucName := capr.SafeConcatName(48, cluster.Name, "managed", "system-upgrade-controller")
	logrus.Infof("==== [managed-system-agent] attempt to uninstall SUC managed chart [%s] on %s", sucName, cluster.Name)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := h.managedCharts.Cache().Get(cluster.Status.FleetWorkspaceName, sucName)
		if err != nil {
			if errors.IsNotFound(err) {
				logrus.Infof("==== [managed-system-agent] system-upgrade-controller managed chart does not exist")
				return nil
			}
			return err
		}
		if err := h.managedCharts.Delete(app.Namespace, app.Name, &metav1.DeleteOptions{}); err != nil {
			logrus.Errorf("==== [managed-system-agent] hit errors: %v", err)
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		logrus.Infof("==== [managed-system-agent] system-upgrade-controller managed charts deleted")
		return nil
	})
	if err != nil {
		return cluster, fmt.Errorf("failed to delete system-upgrade-controller managed chart: %v", err)
	}

	return cluster, nil
}

func (h *handler) OnChangeUninstallSystemAgentBundle(_ string, cluster *rancherv1.Cluster) (*rancherv1.Cluster, error) {
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

	systemAgent := capr.SafeConcatName(capr.MaxHelmReleaseNameLength, cluster.Name, "managed", "system", "agent")
	logrus.Infof("==== [managed-system-agent] attempt to uninstall system-agent  [%s] on %s", systemAgent, cluster.Name)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := h.bundles.Get(cluster.Status.FleetWorkspaceName, systemAgent, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				logrus.Infof("==== [managed-system-agent] system agent bundle does not exist")
				return nil
			}
			return err
		}
		if err := h.bundles.Delete(app.Namespace, app.Name, &metav1.DeleteOptions{}); err != nil {
			logrus.Errorf("==== [managed-system-agent] hit errors: %v", err)
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		logrus.Infof("==== [managed-system-agent] system-agent bunddles deleted")
		return nil
	})
	if err != nil {
		return cluster, fmt.Errorf("failed to delete system-agent bundle: %v", err)
	}

	return cluster, nil
}

func (h *handler) OnChangeInstallSystemAgent(_ string, cluster *rancherv1.Cluster) (*rancherv1.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return cluster, nil
	}
	if cluster.Spec.RKEConfig == nil || settings.SystemAgentUpgradeImage.Get() == "" {
		return cluster, nil
	}

	logrus.Infof("==== [managed-system-agent] attempt to Install System-Agent on %s", cluster.Name)
	var (
		secretName         = "stv-aggregation"
		result             []runtime.Object
		systemAgentAppName = capr.SafeConcatName(capr.MaxHelmReleaseNameLength, cluster.Name, "managed", "system", "agent")
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

		result = append(result, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespaces.System,
				Annotations: map[string]string{
					"meta.helm.sh/release-name":      systemAgentAppName,
					"meta.helm.sh/release-namespace": namespaces.System,
				},
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "Helm",
				},
			},
			Data: map[string][]byte{
				"CATTLE_SERVER":      []byte(settings.InternalServerURL.Get()),
				"CATTLE_TOKEN":       []byte(token.Status.Token),
				"CATTLE_CA_CHECKSUM": []byte(systemtemplate.InternalCAChecksum()),
			},
		})
	}

	cp, err := h.controlPlanesCache.Get(cluster.Namespace, cluster.Name)
	if err != nil {
		logrus.Errorf("==== [managed-system-agent] Error encountered getting RKE control plane while determining SUC readiness: %v", err)
		return cluster, err

	}

	if !capr.SystemUpgradeControllerReady.IsTrue(cp) {
		// If the SUC is not ready do not create any plans, as those
		// plans may depend on functionality only a newer version of the SUC contains
		logrus.Debugf("==== [managed-system-agent] the SUC is not yet ready, waiting to create system agent upgrade plans (SUC status: %s)", capr.SystemUpgradeControllerReady.GetStatus(cp))
		return cluster, nil
	}

	result = append(result, installer(cluster, secretName, systemAgentAppName)...)

	// construct an Apply object
	kcSecret, err := h.secrets.Cache().Get(cluster.Namespace, cluster.Name+"-kubeconfig")
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
	err = apply.WithSetID("managed-system-agent").WithDynamicLookup().WithSetOwnerReference(false, false).ApplyObjects(result...)
	if err != nil {
		logrus.Infof("==== [managed-system-agent] Apply return errors: %s", err.Error())
		return cluster, err
	}
	return cluster, nil
}

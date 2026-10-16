package planner

import (
	"testing"

	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func registryControlPlane(registryName string, config rkev1.RegistryConfig) *rkev1.RKEControlPlane {
	cp := newControlPlane("", "")
	cp.Namespace = "fleet-default"
	cp.Spec.Registries = &rkev1.Registry{Configs: map[string]rkev1.RegistryConfig{registryName: config}}
	return cp
}

func TestRenderRegistriesTLSFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		registryName string
		wantPath     string
		wantErr      bool
	}{
		{name: "host", registryName: "registry.example.com", wantPath: "/var/lib/rancher/rke2/etc/tls/registries/registry.example.com/ca.crt"},
		{name: "host and port", registryName: "registry.example.com:5000", wantPath: "/var/lib/rancher/rke2/etc/tls/registries/registry.example.com:5000/ca.crt"},
		{name: "path in name stays inside", registryName: "registry.example.com/team", wantPath: "/var/lib/rancher/rke2/etc/tls/registries/registry.example.com/team/ca.crt"},
		{name: "dot dot", registryName: "..", wantErr: true},
		{name: "dot dot in the middle", registryName: "a/../../b", wantErr: true},
		{name: "dot dot to the data directory", registryName: "../../../../../../etc/cron.d/x", wantErr: true},
		{name: "dot", registryName: ".", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mp := newMockPlanner(t, InfoFunctions{})

			got, err := mp.planner.renderRegistries(registryControlPlane(tt.registryName, rkev1.RegistryConfig{CABundle: []byte("ca")}))
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got.certificateFiles)
				return
			}
			require.NoError(t, err)
			require.Len(t, got.certificateFiles, 1)
			assert.Equal(t, tt.wantPath, got.certificateFiles[0].Path)
		})
	}
}

func TestRenderRegistriesTLSSecretHostileName(t *testing.T) {
	t.Parallel()
	mp := newMockPlanner(t, InfoFunctions{})
	mp.secretCache.EXPECT().Get("fleet-default", "tls-secret").Return(&corev1.Secret{
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}, nil).AnyTimes()

	_, err := mp.planner.renderRegistries(registryControlPlane("../../../../x", rkev1.RegistryConfig{TLSSecretName: "tls-secret"}))
	require.Error(t, err)

	got, err := mp.planner.renderRegistries(registryControlPlane("registry.example.com", rkev1.RegistryConfig{TLSSecretName: "tls-secret"}))
	require.NoError(t, err)
	require.Len(t, got.certificateFiles, 2)
}

func TestRenderRegistriesHostileNameWithoutTLSFiles(t *testing.T) {
	t.Parallel()
	mp := newMockPlanner(t, InfoFunctions{})

	// Without a TLS secret or a CA bundle, the name writes no file, so behavior does not change.
	_, err := mp.planner.renderRegistries(registryControlPlane("../x", rkev1.RegistryConfig{InsecureSkipVerify: true}))
	require.NoError(t, err)
}

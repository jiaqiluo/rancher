package kubeconfig

import (
	"strings"
	"testing"

	clientv3 "github.com/rancher/rancher/pkg/client/generated/management/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostileValues are values that must stay plain text in a kubeconfig. Each one tries to end a YAML scalar
// and to add a user with an exec command that runs on the machine of the person who uses the kubeconfig.
var hostileValues = map[string]string{
	"double quote and new user": "x\"\nusers:\n- name: evil\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1beta1\n      command: /bin/touch\n#",
	"new line":                  "a\nb",
	"double quote":              `a"b`,
	"backslash":                 `a\b`,
	"colon and hash":            "a: b #c",
	"leading dash":              "- a",
	"brace":                     "{a: b}",
	"unicode":                   "caf\u00e9 \u2028 x",
}

// requireNoInjection checks that the parsed kubeconfig has no extra user and no exec command except rancher.
func requireNoInjection(t *testing.T, content string, wantUsers int) {
	t.Helper()
	config := loadConfig(t, content)
	require.Len(t, config.AuthInfos, wantUsers, content)
	for name, authInfo := range config.AuthInfos {
		assert.NotEqual(t, "evil", name)
		if authInfo.Exec != nil {
			assert.Equal(t, "rancher", authInfo.Exec.Command)
		}
	}
}

func TestForTokenBasedKeepsPlainOutput(t *testing.T) {
	t.Parallel()

	content, err := ForTokenBased("downstream", "c-abc12", testHost, "", nil)
	require.NoError(t, err)
	assert.Contains(t, content, "- name: \"downstream\"\n  cluster:\n    server: \"https://rancher.example.com/k8s/clusters/c-abc12\"\n")
	assert.Contains(t, content, "        - token\n        - --server=rancher.example.com\n        - --user=downstream\n      command: rancher\n")
	assert.Contains(t, content, "current-context: \"downstream\"\n")
}

func TestForTokenBasedHostileValues(t *testing.T) {
	t.Parallel()

	for name, value := range hostileValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, token := range []string{"", "t-" + value} {
				content, err := ForTokenBased(value, "c-abc12", testHost, token, &ExecUser{ID: value, AuthProvider: value})
				require.NoError(t, err)
				requireNoInjection(t, content, 1)
				config := loadConfig(t, content)
				assert.Equal(t, value, config.CurrentContext)
				require.Contains(t, config.AuthInfos, value)
				require.Contains(t, config.Clusters, value)
				if token != "" {
					assert.Equal(t, token, config.AuthInfos[value].Token)
					continue
				}
				assert.Equal(t, []string{"auth", "get-token", "--server=" + testHost, "--user-id=" + value, "--auth-provider=" + value}, config.AuthInfos[value].Exec.Args)
			}

			content, err := ForTokenBased("downstream", "c-abc12", value, "", nil)
			require.NoError(t, err)
			requireNoInjection(t, content, 1)
			assert.Equal(t, []string{"token", "--server=" + value, "--user=downstream"}, loadConfig(t, content).AuthInfos["downstream"].Exec.Args)
		})
	}
}

func TestForClusterTokenBasedHostileFQDN(t *testing.T) {
	t.Parallel()

	for name, value := range hostileValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cluster := &clientv3.Cluster{Name: value, CACert: "Y2E=", LocalClusterAuthEndpoint: &clientv3.LocalClusterAuthEndpoint{Enabled: true, FQDN: value}}
			content, err := ForClusterTokenBased(cluster, nil, "c-abc12", testHost, "", nil)
			require.NoError(t, err)
			requireNoInjection(t, content, 1)
			config := loadConfig(t, content)
			require.Contains(t, config.Clusters, value+"-fqdn")
			assert.Equal(t, "https://"+value, config.Clusters[value+"-fqdn"].Server)
		})
	}
}

func TestGenerateHostileValues(t *testing.T) {
	t.Parallel()

	for name, value := range hostileValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			content, err := Generate(KubeConfig{
				Clusters:       []Cluster{{Name: value, Server: "https://" + value}},
				Users:          []User{{Name: value, Host: value, ClusterID: value}},
				Contexts:       []Context{{Name: value, User: value, Cluster: value}},
				CurrentContext: value,
			})
			require.NoError(t, err)
			requireNoInjection(t, content, 1)
			config := loadConfig(t, content)
			assert.Equal(t, value, config.CurrentContext)
			assert.Equal(t, "https://"+value, config.Clusters[value].Server)
			assert.Equal(t, []string{"token", "--server=" + value, "--user=" + value, "--cluster=" + value}, config.AuthInfos[value].Exec.Args)
			assert.Equal(t, value, config.Contexts[value].AuthInfo)
			assert.Equal(t, value, config.Contexts[value].Cluster)
		})
	}
}

func TestYAMLArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		value  string
		want   string
	}{
		{name: "plain", prefix: "--server=", value: "rancher.example.com", want: "--server=rancher.example.com"},
		{name: "host and port", prefix: "--server=", value: "rancher.example.com:8443", want: "--server=rancher.example.com:8443"},
		{name: "trailing colon", prefix: "--user=", value: "a:", want: `"--user=a:"`},
		{name: "space", prefix: "--user=", value: "my cluster", want: `"--user=my cluster"`},
		{name: "new line", prefix: "--user=", value: "a\nb", want: `"--user=a\nb"`},
		{name: "quote", prefix: "--user=", value: `a"b`, want: `"--user=a\"b"`},
		{name: "hash", prefix: "--user=", value: "a #b", want: `"--user=a #b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := yamlArg(tt.prefix, tt.value)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.False(t, strings.Contains(got, "\n"))
		})
	}
}

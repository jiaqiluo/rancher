package clusterregistrationtoken

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestAgentEnvVars(t *testing.T) {
	c := &v3.Cluster{
		Spec: v3.ClusterSpec{
			ClusterSpecBase: v3.ClusterSpecBase{
				AgentEnvVars: []corev1.EnvVar{
					{
						Name:  "HTTPS_PROXY",
						Value: "https://0.0.0.0",
					},
					{
						Name:  "HTTP_PROXY",
						Value: "http://0.0.0.0",
					},
				},
			},
		},
	}
	tests := []struct {
		name     string
		cluster  *v3.Cluster
		envType  EnvType
		expected string
	}{
		{
			name:     "Envvars should be an empty string with a nil cluster",
			cluster:  nil,
			envType:  Linux,
			expected: "",
		},
		{
			name:     "Envvars should be an empty string with no vars set on cluster",
			cluster:  &v3.Cluster{},
			envType:  Linux,
			expected: "",
		},
		{
			name:     "Envvars should be formatted correctly for Linux",
			cluster:  c,
			envType:  Linux,
			expected: "HTTPS_PROXY=\"https://0.0.0.0\" HTTP_PROXY=\"http://0.0.0.0\"",
		},
		{
			name:     "Envvars should be formatted correctly for Docker",
			cluster:  c,
			envType:  Docker,
			expected: "-e \"HTTPS_PROXY=https://0.0.0.0\" -e \"HTTP_PROXY=http://0.0.0.0\"",
		},
		{
			name:     "Envvars should be formatted correctly for PowerShell",
			cluster:  c,
			envType:  PowerShell,
			expected: "$env:HTTPS_PROXY=\"https://0.0.0.0\"; $env:HTTP_PROXY=\"http://0.0.0.0\";",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// arrange
			a := assert.New(t)

			// act
			evars := AgentEnvVars(tt.cluster, tt.envType)

			// assert
			a.Equal(tt.expected, evars)
		})
	}
}

func agentVarsCluster(vars ...corev1.EnvVar) *v3.Cluster {
	return &v3.Cluster{Spec: v3.ClusterSpec{ClusterSpecBase: v3.ClusterSpecBase{AgentEnvVars: vars}}}
}

// hostileAgentEnvValues are values that a shell or PowerShell must treat as plain text.
// Values that contain canary write a file if the shell runs a second command.
func hostileAgentEnvValues(canary string) []string {
	return []string{
		"http://proxy:3128",
		"a b",
		"a\nb",
		"it's",
		`a"b`,
		`a\b`,
		`trailing\`,
		"a!b",
		"a\u201Cb\u201D",
		"$HOME",
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		`x"; touch ` + canary + `; "`,
		"x'; touch " + canary + "; '",
	}
}

func TestAgentEnvVarsQuoting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		envVar   corev1.EnvVar
		envType  EnvType
		expected string
	}{
		{name: "linux keeps double quotes for a plain value", envVar: corev1.EnvVar{Name: "A", Value: "a b=c"}, envType: Linux, expected: `A="a b=c"`},
		{name: "linux double quote", envVar: corev1.EnvVar{Name: "A", Value: `a"b`}, envType: Linux, expected: `A='a"b'`},
		{name: "linux dollar", envVar: corev1.EnvVar{Name: "A", Value: "$(id)"}, envType: Linux, expected: `A='$(id)'`},
		{name: "linux backtick", envVar: corev1.EnvVar{Name: "A", Value: "`id`"}, envType: Linux, expected: "A='`id`'"},
		{name: "linux backslash", envVar: corev1.EnvVar{Name: "A", Value: `a\`}, envType: Linux, expected: `A='a\'`},
		{name: "linux exclamation mark", envVar: corev1.EnvVar{Name: "A", Value: "a!b"}, envType: Linux, expected: `A='a!b'`},
		{name: "linux newline", envVar: corev1.EnvVar{Name: "A", Value: "a\nb"}, envType: Linux, expected: "A='a\nb'"},
		{name: "linux single quote stays in double quotes", envVar: corev1.EnvVar{Name: "A", Value: "it's"}, envType: Linux, expected: `A="it's"`},
		{name: "docker dollar", envVar: corev1.EnvVar{Name: "A", Value: "$(id)"}, envType: Docker, expected: `-e 'A=$(id)'`},
		{name: "docker single quote stays in double quotes", envVar: corev1.EnvVar{Name: "A", Value: "it's"}, envType: Docker, expected: `-e "A=it's"`},
		{name: "powershell keeps double quotes for a plain value", envVar: corev1.EnvVar{Name: "A", Value: `a b\c`}, envType: PowerShell, expected: `$env:A="a b\c";`},
		{name: "powershell double quote", envVar: corev1.EnvVar{Name: "A", Value: `a"b`}, envType: PowerShell, expected: `$env:A='a"b';`},
		{name: "powershell subexpression", envVar: corev1.EnvVar{Name: "A", Value: "$(calc)"}, envType: PowerShell, expected: `$env:A='$(calc)';`},
		{name: "powershell backtick", envVar: corev1.EnvVar{Name: "A", Value: "a`nb"}, envType: PowerShell, expected: "$env:A='a`nb';"},
		{name: "powershell curly double quote", envVar: corev1.EnvVar{Name: "A", Value: "a\u201Cb"}, envType: PowerShell, expected: "$env:A='a\u201Cb';"},
		{name: "powershell single quote stays in double quotes", envVar: corev1.EnvVar{Name: "A", Value: "it's"}, envType: PowerShell, expected: `$env:A="it's";`},
		{name: "invalid name is skipped", envVar: corev1.EnvVar{Name: "A=1; calc; B", Value: "x"}, envType: Linux, expected: ""},
		{name: "invalid name is skipped for powershell", envVar: corev1.EnvVar{Name: "A B", Value: "x"}, envType: PowerShell, expected: ""},
		{name: "invalid name is skipped for docker", envVar: corev1.EnvVar{Name: "A\" -v /:/host \"B", Value: "x"}, envType: Docker, expected: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, AgentEnvVars(agentVarsCluster(tt.envVar), tt.envType))
		})
	}
}

// TestAgentEnvVarsRunInShell runs the rendered text in sh and checks that the value arrives unchanged.
func TestAgentEnvVarsRunInShell(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is not available: %v", err)
	}

	canary := filepath.Join(t.TempDir(), "canary")
	for _, v := range hostileAgentEnvValues(canary) {
		linux := AgentEnvVars(agentVarsCluster(corev1.EnvVar{Name: "RANCHER_TEST_VAR", Value: v}), Linux)
		out, err := exec.Command("/bin/sh", "-c", linux+` printenv RANCHER_TEST_VAR`).Output()
		require.NoError(t, err, "linux value %q", v)
		assert.Equal(t, v+"\n", string(out), "linux value %q", v)

		docker := AgentEnvVars(agentVarsCluster(corev1.EnvVar{Name: "RANCHER_TEST_VAR", Value: v}), Docker)
		out, err = exec.Command("/bin/sh", "-c", `printf '%s\0' `+docker).Output()
		require.NoError(t, err, "docker value %q", v)
		assert.Equal(t, "-e\x00RANCHER_TEST_VAR="+v+"\x00", string(out), "docker value %q", v)

		_, err = os.Stat(canary)
		require.True(t, os.IsNotExist(err), "value %q ran a second command", v)
	}
}

// TestAgentEnvVarsRunInPowerShell runs the rendered text in PowerShell when pwsh is installed.
func TestAgentEnvVarsRunInPowerShell(t *testing.T) {
	t.Parallel()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not on PATH")
	}

	canary := filepath.Join(t.TempDir(), "canary")
	values := append(hostileAgentEnvValues(canary),
		"$(New-Item "+canary+")",
		`x"; New-Item `+canary+`; "`,
		"x'; New-Item "+canary+"; '",
		"a`nb",
	)
	for _, v := range values {
		rendered := AgentEnvVars(agentVarsCluster(corev1.EnvVar{Name: "RANCHER_TEST_VAR", Value: v}), PowerShell)
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
			rendered+" [Console]::Out.Write($env:RANCHER_TEST_VAR)").Output()
		require.NoError(t, err, "value %q", v)
		assert.Equal(t, v, string(out), "value %q", v)
		_, err = os.Stat(canary)
		require.True(t, os.IsNotExist(err), "value %q ran a second command", v)
	}
}

package installer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemtemplate"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestInstaller_WindowsInstallScript(t *testing.T) {
	// arrange
	CACert := "uibesrwguiobsdrujigbsduigbuidbg"
	a := assert.New(t)

	// Capture and restore global settings to prevent test pollution
	originalServerURL := settings.ServerURL.Get()
	originalCACerts := settings.CACerts.Get()
	t.Cleanup(func() {
		if err := settings.ServerURL.Set(originalServerURL); err != nil {
			t.Errorf("failed to restore ServerURL setting: %v", err)
		}
		if err := settings.CACerts.Set(originalCACerts); err != nil {
			t.Errorf("failed to restore CACerts setting: %v", err)
		}
	})

	// act
	err := settings.ServerURL.Set("localhost")
	a.Nil(err)

	err = settings.CACerts.Set(CACert)
	a.Nil(err)

	CACertEncoded := systemtemplate.CAChecksum()

	agentVar1 := corev1.EnvVar{
		Name:  "TestEnvVar1",
		Value: "TestEnvVarValue",
	}

	agentVar2 := corev1.EnvVar{
		Name:  "TestEnvVar2",
		Value: "TestEnvVarValue",
	}

	formattedAgentVar1 := capr.FormatWindowsEnvVar(agentVar1, false)
	formattedAgentVar2 := capr.FormatWindowsEnvVar(agentVar2, false)

	script, err := WindowsInstallScript(context.TODO(), "test", []corev1.EnvVar{
		agentVar1,
		agentVar2,
	}, "localhost", "/var/lib/rancher/rke2")

	// assert
	a.Nil(err)
	a.NotNil(script)
	a.Contains(string(script), "$env:CATTLE_TOKEN=\"test\"")
	a.Contains(string(script), "$env:CATTLE_ROLE_NONE=\"true\"")
	a.Contains(string(script), "$env:CATTLE_SERVER=\"localhost\"")
	a.Contains(string(script), fmt.Sprintf("$env:CATTLE_CA_CHECKSUM=\"%s\"", CACertEncoded))
	a.Contains(string(script), "$env:CSI_PROXY_URL")
	a.Contains(string(script), "$env:CSI_PROXY_VERSION")
	a.Contains(string(script), "$env:CSI_PROXY_KUBELET_PATH")
	a.Contains(string(script), "$env:TestEnvVar1")
	a.Contains(string(script), "$env:TestEnvVar2")

	// ensure agent env vars are not on the same line
	a.NotContains(string(script), formattedAgentVar1+formattedAgentVar2)
}

func TestInstaller_LinuxInstallScript(t *testing.T) {
	// arrange
	CACert := "uibesrwguiobsdrujigbsduigbuidbg"
	a := assert.New(t)

	// Capture and restore global settings to prevent test pollution
	originalServerURL := settings.ServerURL.Get()
	originalCACerts := settings.CACerts.Get()
	t.Cleanup(func() {
		if err := settings.ServerURL.Set(originalServerURL); err != nil {
			t.Errorf("failed to restore ServerURL setting: %v", err)
		}
		if err := settings.CACerts.Set(originalCACerts); err != nil {
			t.Errorf("failed to restore CACerts setting: %v", err)
		}
	})

	// act
	err := settings.ServerURL.Set("localhost")
	a.Nil(err)

	err = settings.CACerts.Set(CACert)
	a.Nil(err)

	CACertEncoded := systemtemplate.CAChecksum()

	agentVar1 := corev1.EnvVar{
		Name:  "HTTP_PROXY",
		Value: "http://proxy.example.com:3128",
	}

	agentVar2 := corev1.EnvVar{
		Name:  "HTTPS_PROXY",
		Value: "http://proxy.example.com:3128",
	}

	agentVar3 := corev1.EnvVar{
		Name:  "NO_PROXY",
		Value: "127.0.0.0/8,10.0.0.0/8",
	}

	script, err := LinuxInstallScript(context.TODO(), "test", []corev1.EnvVar{
		agentVar1,
		agentVar2,
		agentVar3,
	}, "localhost", "")

	// assert
	a.Nil(err)
	a.NotNil(script)

	// Verify environment variables are exported with proper shell escaping (single quotes)
	a.Contains(string(script), "export HTTP_PROXY='http://proxy.example.com:3128'")
	a.Contains(string(script), "export HTTPS_PROXY='http://proxy.example.com:3128'")
	a.Contains(string(script), "export NO_PROXY='127.0.0.0/8,10.0.0.0/8'")

	// Verify other required variables
	a.Contains(string(script), "CATTLE_ROLE_NONE=true")
	a.Contains(string(script), "CATTLE_TOKEN=\"test\"")
	a.Contains(string(script), "CATTLE_SERVER=localhost")
	a.Contains(string(script), fmt.Sprintf("CATTLE_CA_CHECKSUM=\"%s\"", CACertEncoded))

	// Verify shebang
	a.Contains(string(script), "#!/usr/bin/env sh")
}

func TestInstaller_LinuxInstallScript_ShellInjectionPrevention(t *testing.T) {
	a := assert.New(t)

	// Capture and restore global settings
	originalServerURL := settings.ServerURL.Get()
	originalCACerts := settings.CACerts.Get()
	t.Cleanup(func() {
		if err := settings.ServerURL.Set(originalServerURL); err != nil {
			t.Errorf("failed to restore ServerURL setting: %v", err)
		}
		if err := settings.CACerts.Set(originalCACerts); err != nil {
			t.Errorf("failed to restore CACerts setting: %v", err)
		}
	})

	err := settings.ServerURL.Set("localhost")
	a.Nil(err)
	err = settings.CACerts.Set("test")
	a.Nil(err)

	tests := []struct {
		name             string
		envVar           corev1.EnvVar
		shouldContain    string
		shouldNotContain []string
		description      string
	}{
		{
			name: "double quotes in value",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: `value with "quotes"`,
			},
			shouldContain:    `export TEST_VAR='value with "quotes"'`,
			shouldNotContain: []string{`export TEST_VAR="value with "quotes""`},
			description:      "Double quotes should be safely contained within single quotes",
		},
		{
			name: "single quote in value",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: `it's a value`,
			},
			shouldContain:    `export TEST_VAR='it'\''s a value'`,
			shouldNotContain: []string{`export TEST_VAR='it's a value'`},
			description:      "Single quotes should be properly escaped",
		},
		{
			name: "backticks in value",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: "`whoami`",
			},
			shouldContain:    "export TEST_VAR='`whoami`'",
			shouldNotContain: []string{`export TEST_VAR="` + "`whoami`" + `"`},
			description:      "Backticks should not execute commands",
		},
		{
			name: "dollar sign in value",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: "$PATH:/custom/path",
			},
			shouldContain:    "export TEST_VAR='$PATH:/custom/path'",
			shouldNotContain: []string{`export TEST_VAR="$PATH:/custom/path"`},
			description:      "Dollar signs should not trigger variable expansion",
		},
		{
			name: "command substitution attempt",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: "$(rm -rf /)",
			},
			shouldContain:    "export TEST_VAR='$(rm -rf /)'",
			shouldNotContain: []string{`export TEST_VAR="$(rm -rf /)"`},
			description:      "Command substitution should not execute",
		},
		{
			name: "newline in value",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: "line1\nline2",
			},
			shouldContain: "export TEST_VAR='line1\nline2'",
			description:   "Newlines should be safely contained",
		},
		{
			name: "semicolon command separator",
			envVar: corev1.EnvVar{
				Name:  "TEST_VAR",
				Value: "value; rm -rf /",
			},
			shouldContain:    "export TEST_VAR='value; rm -rf /'",
			shouldNotContain: []string{`export TEST_VAR="value; rm -rf /"`},
			description:      "Semicolons should not allow command injection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assert.New(t)
			script, err := LinuxInstallScript(context.TODO(), "", []corev1.EnvVar{tt.envVar}, "", "")
			a.Nil(err)
			a.NotNil(script)

			scriptStr := string(script)
			a.Contains(scriptStr, tt.shouldContain, tt.description)

			for _, forbidden := range tt.shouldNotContain {
				a.NotContains(scriptStr, forbidden, "Should not use unsafe escaping")
			}
		})
	}
}

func TestInstaller_WindowsInstallScriptQuotesDataDirectory(t *testing.T) {
	tests := []struct {
		name                string
		dataDir             string
		wantKubeletPath     string
		envVars             []corev1.EnvVar
		wantAgentVarDirLine string
	}{
		{
			name:            "normal path",
			dataDir:         "/var/lib/rancher/rke2",
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/var/lib/rancher/rke2/bin/kubelet.exe'\n",
		},
		{
			name:            "space",
			dataDir:         "/data/my dir",
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/data/my dir/bin/kubelet.exe'\n",
		},
		{
			name:            "newline",
			dataDir:         "/data/a\nb",
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/data/a\nb/bin/kubelet.exe'\n",
		},
		{
			name:            "single quote",
			dataDir:         "/data/it's",
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/data/it''s/bin/kubelet.exe'\n",
		},
		{
			name:            "backslash",
			dataDir:         `/data/a\b`,
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/data/a\\b/bin/kubelet.exe'\n",
		},
		{
			name:            "subexpression",
			dataDir:         "/data/$(Remove-Item x)",
			wantKubeletPath: "$env:CSI_PROXY_KUBELET_PATH = 'C:/data/$(Remove-Item x)/bin/kubelet.exe'\n",
		},
		{
			name:                "system agent directory",
			dataDir:             "/var/lib/rancher/rke2",
			wantKubeletPath:     "$env:CSI_PROXY_KUBELET_PATH = 'C:/var/lib/rancher/rke2/bin/kubelet.exe'\n",
			envVars:             []corev1.EnvVar{{Name: capr.SystemAgentDataDirEnvVar, Value: "/data/it's \"a\"$(x)"}},
			wantAgentVarDirLine: "$env:CATTLE_AGENT_VAR_DIR='/data/it''s \"a\"$(x)'\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, err := WindowsInstallScript(context.TODO(), "", tt.envVars, "localhost", tt.dataDir)
			assert.NoError(t, err)
			assert.Contains(t, string(script), tt.wantKubeletPath)
			if tt.wantAgentVarDirLine != "" {
				assert.Contains(t, string(script), tt.wantAgentVarDirLine)
			}
		})
	}
}

func TestWindowsAgentEnvVarLine(t *testing.T) {
	tests := []struct {
		name   string
		envVar corev1.EnvVar
		want   string
		wantOK bool
	}{
		{name: "string", envVar: corev1.EnvVar{Name: "HTTP_PROXY", Value: "http://proxy:3128"}, want: `$env:HTTP_PROXY="http://proxy:3128"`, wantOK: true},
		{name: "surrounding double quotes are removed", envVar: corev1.EnvVar{Name: "A", Value: `"abc"`}, want: `$env:A="abc"`, wantOK: true},
		{name: "env prefix in name is removed", envVar: corev1.EnvVar{Name: "$env:A", Value: "abc"}, want: `$env:A="abc"`, wantOK: true},
		{name: "bool", envVar: corev1.EnvVar{Name: "FLAG", Value: "true"}, want: "$env:FLAG=$true", wantOK: true},
		{name: "space", envVar: corev1.EnvVar{Name: "A", Value: "a b"}, want: `$env:A="a b"`, wantOK: true},
		{name: "newline", envVar: corev1.EnvVar{Name: "A", Value: "a\nb"}, want: "$env:A='a\nb'", wantOK: true},
		{name: "curly double quote", envVar: corev1.EnvVar{Name: "A", Value: "a\u201Cb"}, want: "$env:A='a\u201Cb'", wantOK: true},
		{name: "single quote stays in double quotes", envVar: corev1.EnvVar{Name: "A", Value: "it's"}, want: `$env:A="it's"`, wantOK: true},
		{name: "double quote", envVar: corev1.EnvVar{Name: "A", Value: `a"b`}, want: `$env:A='a"b'`, wantOK: true},
		{name: "backslash", envVar: corev1.EnvVar{Name: "A", Value: `a\b`}, want: `$env:A="a\b"`, wantOK: true},
		{name: "subexpression", envVar: corev1.EnvVar{Name: "A", Value: "$(calc)"}, want: "$env:A='$(calc)'", wantOK: true},
		{name: "name with statement", envVar: corev1.EnvVar{Name: "A=1; calc; $env:B", Value: "x"}},
		{name: "name with space", envVar: corev1.EnvVar{Name: "A B", Value: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := windowsAgentEnvVarLine(tt.envVar)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestWindowsAgentEnvVarLineRunsAsOneStatement runs the generated line in PowerShell when pwsh is installed.
func TestWindowsAgentEnvVarLineRunsAsOneStatement(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not on PATH")
	}

	canary := filepath.Join(t.TempDir(), "canary")
	values := []string{
		"plain",
		"a b",
		"a\nb",
		"it's",
		`a"b`,
		`a\b`,
		"a\u2018b\u2019c",
		"$(New-Item " + canary + ")",
		`x"; New-Item ` + canary + `; "`,
		"x'; New-Item " + canary + "; '",
	}
	for _, v := range values {
		line, ok := windowsAgentEnvVarLine(corev1.EnvVar{Name: "RANCHER_TEST_VAR", Value: v})
		assert.True(t, ok)
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
			line+"; [Console]::Out.Write($env:RANCHER_TEST_VAR)").Output()
		assert.NoError(t, err, "value %q", v)
		assert.Equal(t, strings.Trim(v, `"`), string(out), "value %q", v)
		_, err = os.Stat(canary)
		assert.True(t, os.IsNotExist(err), "value %q ran a second command", v)
	}
}

// TestWindowsAgentEnvVarLineKeepsExistingOutput pins that a plain value renders as before the quoting change.
func TestWindowsAgentEnvVarLineKeepsExistingOutput(t *testing.T) {
	for _, envVar := range []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: "http://user:pass@proxy:3128"},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1,.svc"},
		{Name: "FLAG", Value: "true"},
		{Name: "FLAG", Value: "$false"},
		{Name: "A", Value: `"quoted"`},
	} {
		got, ok := windowsAgentEnvVarLine(envVar)
		assert.True(t, ok)
		assert.Equal(t, capr.FormatWindowsEnvVar(envVar, false), got)
	}
}

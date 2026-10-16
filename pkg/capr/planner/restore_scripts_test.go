package planner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFakeKubectl writes a kubectl stand-in at <distroDir>/bin/kubectl. It appends its arguments to
// $KUBECTL_LOG and prints $FAKE_GET_OUTPUT for every "get" call.
func writeFakeKubectl(t *testing.T, distroDir string) string {
	t.Helper()
	kubectl := filepath.Join(distroDir, "bin", "kubectl")
	require.NoError(t, os.MkdirAll(filepath.Dir(kubectl), 0o700))
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KUBECTL_LOG\"\nif [ \"$1\" = get ]; then printf '%b' \"$FAKE_GET_OUTPUT\"; fi\n"
	require.NoError(t, os.WriteFile(kubectl, []byte(script), 0o700))
	return kubectl
}

// runRestoreScript writes script to a file and runs it with /bin/sh, as the idempotent wrapper does.
func runRestoreScript(t *testing.T, script, kubectl, log, getOutput string, args ...string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "script.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o700))
	// BSD wc pads its count with spaces, and the script compares the count as a string.
	// This shim keeps the test the same on macOS and Linux.
	shimDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shimDir, "wc"), []byte("#!/bin/sh\n/usr/bin/wc \"$@\" | tr -d ' '\n"), 0o700))
	// The scripts retry for minutes when kubectl fails to run. Fail fast instead.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{scriptPath}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECTL="+kubectl,
		"KUBECONFIG=/dev/null",
		"KUBECTL_LOG="+log,
		"FAKE_GET_OUTPUT="+getOutput,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "output: %s", out)
}

func readLog(t *testing.T, log string) []string {
	t.Helper()
	content, err := os.ReadFile(log)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// TestEtcdRestoreScriptsHostileKubectlPath checks that the restore scripts run kubectl as one word.
// The kubectl path derives from spec.dataDirectories.k8sDistro, so it can contain shell syntax.
func TestEtcdRestoreScriptsHostileKubectlPath(t *testing.T) {
	t.Parallel()
	requireSh(t)

	base := t.TempDir()
	canary := filepath.Join(base, "canary")
	names := hostileDirNames(canary)
	names["normal"] = "rke2"

	for name, dirName := range names {
		t.Run(name, func(t *testing.T) {
			distroDir := filepath.Join(base, strings.ReplaceAll(name, " ", "-"), dirName)
			kubectl := writeFakeKubectl(t, distroDir)

			t.Run("wait for pod list", func(t *testing.T) {
				log := filepath.Join(t.TempDir(), "log")
				runRestoreScript(t, etcdRestorePostRestoreWaitForPodListCleanupScript, kubectl, log, "",
					kubectl, "--kubeconfig", "/etc/kubeconfig", "get", "pods", "--all-namespaces")
				assert.Equal(t, []string{"--kubeconfig /etc/kubeconfig get pods --all-namespaces"}, readLog(t, log))
				requireNoCanary(t, canary)
			})

			t.Run("clean up nodes", func(t *testing.T) {
				dir := t.TempDir()
				log := filepath.Join(dir, "log")
				machineIDs := filepath.Join(dir, "machine-ids")
				nodeNames := filepath.Join(dir, "node-names")
				require.NoError(t, os.WriteFile(machineIDs, nil, 0o600))
				require.NoError(t, os.WriteFile(nodeNames, []byte("keep\n"), 0o600))
				runRestoreScript(t, etcdRestoreNodeCleanUpScript, kubectl, log, "keep\\ngone\\n", machineIDs, nodeNames)
				assert.Equal(t, []string{
					"get nodes --no-headers -o=jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}",
					"delete node gone --wait=false",
				}, readLog(t, log))
				requireNoCanary(t, canary)
			})

			t.Run("wait for ready", func(t *testing.T) {
				log := filepath.Join(t.TempDir(), "log")
				runRestoreScript(t, etcdRestoreNodeWaitForReadyScript, kubectl, log, "True\\n", "machine-1")
				lines := readLog(t, log)
				require.Len(t, lines, 2)
				assert.Contains(t, lines[0], "get node --no-headers")
				assert.Contains(t, lines[1], "get nodes --no-headers")
				requireNoCanary(t, canary)
			})
		})
	}
}

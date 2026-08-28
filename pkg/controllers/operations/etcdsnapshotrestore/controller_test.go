package etcdsnapshotrestore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	rkeplan "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1/plan"
	"github.com/rancher/rancher/pkg/capr"
	ops "github.com/rancher/rancher/pkg/operations"
	planapi "github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	plancontrollers "github.com/rancher/rancher/pkg/plan/generated/controllers/plan.cattle.io/v1alpha1"
	ctrlfake "github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// stubAdapter is a minimal ops.Adapter implementation for testing plan construction.
// Methods unrelated to the test return zero values.
type stubAdapter struct {
	runtimeCommand    string
	dataDir           string
	provisioningDir   string
	kubectlPath       string
	kubeconfigPath    string
	serverUnit        string
	waitForRegisterOK bool
}

func (a *stubAdapter) EtcdSnapshotNamespace() string {
	return "test-namespace"
}

func (a *stubAdapter) ClusterObject() (*unstructured.Unstructured, error) {
	//TODO implement me
	panic("implement me")
}

func (a *stubAdapter) BeaconRef() (string, string)                       { return "test-namespace", "test-cluster" }
func (a *stubAdapter) WaitForRegister() (bool, error)                    { return a.waitForRegisterOK, nil }
func (a *stubAdapter) PauseCluster(_ bool) error                         { return nil }
func (a *stubAdapter) RuntimeCommand() string                            { return a.runtimeCommand }
func (a *stubAdapter) DistroDataDirectory(_ *corev1.Secret) string       { return a.dataDir }
func (a *stubAdapter) ProvisioningDataDirectory(_ *corev1.Secret) string { return a.provisioningDir }
func (a *stubAdapter) ServerUnit() string                                { return a.serverUnit }
func (a *stubAdapter) RenderProbes(_ *corev1.Secret, _ bool) (map[string]rkeplan.Probe, error) {
	return map[string]rkeplan.Probe{}, nil
}
func (a *stubAdapter) KubectlPath(_ *corev1.Secret) string    { return a.kubectlPath }
func (a *stubAdapter) KubeconfigPath(_ *corev1.Secret) string { return a.kubeconfigPath }
func (a *stubAdapter) FindOrElectLeader(_ string, _ ops.Filter) (*corev1.Secret, error) {
	return nil, nil
}

// The six methods below complete the ops.Adapter contract for the stub. They are not exercised
// by the snapshot-restore controller (which only consumes runtime/dataDir/serverUnit/probes/
// kubectl+kubeconfig paths/plans), so each returns a static, runtime-appropriate value.
func (a *stubAdapter) ConfigFile(_ *corev1.Secret) string {
	return "/etc/rancher/" + a.runtimeCommand + "/config.yaml"
}
func (a *stubAdapter) ConfigDirectory(_ *corev1.Secret) string {
	return "/etc/rancher/" + a.runtimeCommand + "/config.yaml.d"
}
func (a *stubAdapter) GetServerURL(_ *corev1.Secret) string      { return "" }
func (a *stubAdapter) GetSupervisorPort(_ *corev1.Secret) string { return "9345" }
func (a *stubAdapter) LoopbackAddress(_ *corev1.Secret) string   { return "127.0.0.1" }
func (a *stubAdapter) ToS3ArgsEnvAndFiles(_ *corev1.Secret) ([]string, []string, []planapi.File) {
	return nil, nil, nil
}

func newTestScope(adapter *stubAdapter, uid types.UID) *scope {
	cluster := &unstructured.Unstructured{}
	cluster.SetName("test-cluster")
	cluster.SetNamespace("fleet-default")
	cluster.SetAPIVersion("provisioning.cattle.io/v1")
	cluster.SetKind("Cluster")

	return &scope{
		op: &opv1alpha1.ETCDSnapshotRestore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "restore-1",
				Namespace: "fleet-default",
				UID:       uid,
			},
		},
		namespace:  "fleet-default",
		clusterObj: cluster,
		adapter:    adapter,
	}
}

func defaultAdapter() *stubAdapter {
	return &stubAdapter{
		runtimeCommand:  "rke2",
		dataDir:         "/var/lib/rancher/rke2",
		provisioningDir: "/var/lib/rancher/capr",
		kubectlPath:     "/var/lib/rancher/rke2/bin/kubectl",
		kubeconfigPath:  "/etc/rancher/rke2/rke2.yaml",
		serverUnit:      "rke2-server",
	}
}

func makePlanSecret(name, nodeName string, labels map[string]string) *corev1.Secret {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[capr.ClusterNameLabel] = "test-cluster"
	if nodeName != "" {
		labels[capr.NodeNameLabel] = nodeName
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "fleet-default",
			Labels:    labels,
			UID:       types.UID(name + "-uid"),
		},
	}
}

func TestCancelPolicyRegistered(t *testing.T) {
	t.Parallel()

	policy := ops.CancelPolicyFor(operationGVK)
	if !policy.RequiresRecovery {
		t.Fatalf("a restore stopped partway can leave nodes with divergent datastores, so it must require recovery")
	}
	if policy.RecoveryMessage == "" {
		t.Fatalf("expected a recovery message telling the user what to do")
	}
}

func TestBuildPostRestoreNodeCleanupPlan(t *testing.T) {
	t.Parallel()

	s := newTestScope(defaultAdapter(), "restore-uid")
	initSecret := makePlanSecret("init", "node-init", map[string]string{
		capr.EtcdRoleLabel: "true",
		capr.InitNodeLabel: "true",
	})
	other := makePlanSecret("worker-1", "node-worker-1", map[string]string{
		capr.WorkerRoleLabel: "true",
	})
	allSecrets := []*corev1.Secret{initSecret, other}

	plan, skipReason := buildPostRestoreNodeCleanupPlan(s, initSecret, allSecrets)
	if skipReason != "" {
		t.Fatalf("unexpected skipReason: %q", skipReason)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}

	// 3 files: idempotent script, cleanup script, node names list.
	if len(plan.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(plan.Files))
	}

	wantIdempotentPath := ops.IdempotentActionScriptPath(s.adapter.ProvisioningDataDirectory(initSecret))
	wantCleanupPath := path.Join(s.adapter.ProvisioningDataDirectory(initSecret), etcdRestoreBinSubdir, nodeCleanupScriptName)
	wantNodeNamesPath := path.Join(s.adapter.ProvisioningDataDirectory(initSecret), etcdRestoreBinSubdir, fmt.Sprintf("node-names-%s", string(s.op.UID)))

	pathsByPath := map[string]planapi.File{}
	for _, f := range plan.Files {
		pathsByPath[f.Path] = f
	}
	for _, p := range []string{wantIdempotentPath, wantCleanupPath, wantNodeNamesPath} {
		if _, ok := pathsByPath[p]; !ok {
			t.Errorf("missing file at path %q", p)
		}
	}

	nodeNamesFile := pathsByPath[wantNodeNamesPath]
	decoded, err := base64.StdEncoding.DecodeString(nodeNamesFile.Content)
	if err != nil {
		t.Fatalf("node names file content not valid base64: %v", err)
	}
	wantNodeNames := "node-init\nnode-worker-1\n"
	if string(decoded) != wantNodeNames {
		t.Errorf("node names content = %q, want %q", string(decoded), wantNodeNames)
	}

	if !nodeNamesFile.Dynamic {
		t.Error("node names file should be Dynamic (one cleanup per restore)")
	}

	cleanupScriptFile := pathsByPath[wantCleanupPath]
	decodedScript, err := base64.StdEncoding.DecodeString(cleanupScriptFile.Content)
	if err != nil {
		t.Fatalf("cleanup script content not valid base64: %v", err)
	}
	if string(decodedScript) != nodeCleanupScript {
		t.Errorf("cleanup script content does not match nodeCleanupScript")
	}

	if len(plan.OneTimeInstructions) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(plan.OneTimeInstructions))
	}
	instr := plan.OneTimeInstructions[0]
	if instr.Command != "/bin/sh" {
		t.Errorf("instruction Command = %q, want /bin/sh", instr.Command)
	}
	// The script invocation must reference the cleanup script path and the node names file path.
	joined := strings.Join(instr.Args, " ")
	if !strings.Contains(joined, wantCleanupPath) {
		t.Errorf("instruction args do not reference cleanup script path %q: %v", wantCleanupPath, instr.Args)
	}
	if !strings.Contains(joined, wantNodeNamesPath) {
		t.Errorf("instruction args do not reference node names path %q: %v", wantNodeNamesPath, instr.Args)
	}

	// The KUBECTL/KUBECONFIG env entries must be set so the cleanup script can find its tools.
	envSet := map[string]bool{}
	for _, e := range instr.Env {
		envSet[e] = true
	}
	if !envSet["KUBECTL="+s.adapter.KubectlPath(initSecret)] {
		t.Errorf("KUBECTL env missing or wrong: %v", instr.Env)
	}
	if !envSet["KUBECONFIG="+s.adapter.KubeconfigPath(initSecret)] {
		t.Errorf("KUBECONFIG env missing or wrong: %v", instr.Env)
	}

	// The instruction must be wrapped in the idempotent script — the script path appears as the
	// second arg (after -x).
	if len(instr.Args) < 2 || instr.Args[1] != wantIdempotentPath {
		t.Errorf("instruction is not idempotent-wrapped, Args[1] = %v", instr.Args)
	}
}

func TestBuildPostRestoreNodeCleanupPlanSkipsWhenNoNodeNames(t *testing.T) {
	t.Parallel()

	s := newTestScope(defaultAdapter(), "restore-uid")
	initSecret := makePlanSecret("init", "", map[string]string{
		capr.EtcdRoleLabel: "true",
		capr.InitNodeLabel: "true",
	})
	// initSecret has no node-name label; allSecrets list has only this secret.
	plan, skipReason := buildPostRestoreNodeCleanupPlan(s, initSecret, []*corev1.Secret{initSecret})
	if plan != nil {
		t.Error("expected nil plan when there are no node names to preserve")
	}
	if skipReason == "" {
		t.Error("expected non-empty skipReason when there are no node names")
	}
}

func TestBuildPostRestoreNodeCleanupPlanSkipsWhenNoKubectl(t *testing.T) {
	t.Parallel()

	a := defaultAdapter()
	a.kubectlPath = ""
	s := newTestScope(a, "restore-uid")
	initSecret := makePlanSecret("init", "node-init", map[string]string{
		capr.EtcdRoleLabel: "true",
		capr.InitNodeLabel: "true",
	})
	plan, skipReason := buildPostRestoreNodeCleanupPlan(s, initSecret, []*corev1.Secret{initSecret})
	if plan != nil {
		t.Error("expected nil plan when kubectl path is missing")
	}
	if skipReason == "" {
		t.Error("expected non-empty skipReason when kubectl path is missing")
	}
}

func TestIdempotencyValueStable(t *testing.T) {
	t.Parallel()

	s := newTestScope(defaultAdapter(), "abc-123")
	if got := s.idempotencyValue(); got != "abc-123" {
		t.Errorf("idempotencyValue = %q, want %q", got, "abc-123")
	}
}

// --- interrupt-cleanup finalizer -------------------------------------------------------------
//
// The fixtures below mirror the ones in etcdsnapshotsave's and encryptionkeyrotation's tests. They
// exist here because the cleanup and lazy-finalizer paths are a third copy of the same logic, and
// a copy nobody exercises is a copy that drifts.

// fakeDynamic satisfies the controller's dynamicResolver interface.
type fakeDynamic struct {
	getObj runtime.Object
	getErr error
}

func (d *fakeDynamic) Get(_ schema.GroupVersionKind, _, _ string) (runtime.Object, error) {
	if d.getErr != nil {
		return nil, d.getErr
	}
	return d.getObj, nil
}

func (d *fakeDynamic) Enqueue(_ schema.GroupVersionKind, _, _ string) error { return nil }

// newMgmtClusterRef builds a ClusterRef/cluster pair pointing at a plain imported mgmt v3 Cluster.
// That GVK is the only adapter ops.NewAdapter can build without a live wrangler context.
// ImportedAdapter.BeaconRef is (name, name), so the cluster's own name doubles as the namespace
// holding its machine-plan Secrets — which is precisely why the operation's namespace must never
// be substituted for it.
func newMgmtClusterRef() (*corev1.ObjectReference, *unstructured.Unstructured) {
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion("management.cattle.io/v3")
	cluster.SetKind("Cluster")
	cluster.SetName("c-m-test")

	return &corev1.ObjectReference{
		APIVersion: "management.cattle.io/v3",
		Kind:       "Cluster",
		Name:       "c-m-test",
	}, cluster
}

func newImportedPlanSecret(name string, state planapi.PlanState) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "c-m-test",
			UID:         types.UID(name + "-uid"),
			Annotations: map[string]string{},
			Labels:      map[string]string{capr.ClusterNameLabel: "c-m-test"},
		},
		Type: planapi.SecretTypeMachinePlan,
		Data: map[string][]byte{
			"plan":               []byte(`{"instructions":[]}`),
			planapi.PlanStateKey: []byte(state),
		},
	}
}

func newRestoreSecretClient(t *testing.T, ctrl *gomock.Controller, updates *[]*corev1.Secret, items ...*corev1.Secret) *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	m := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		if updates != nil {
			*updates = append(*updates, s.DeepCopy())
		}
		return s, nil
	}).AnyTimes()
	m.EXPECT().List(gomock.Any(), gomock.Any()).DoAndReturn(func(ns string, opts metav1.ListOptions) (*corev1.SecretList, error) {
		sel, err := labels.Parse(opts.LabelSelector)
		if err != nil {
			return nil, err
		}
		var out corev1.SecretList
		for _, s := range items {
			if s.Namespace == ns && sel.Matches(labels.Set(s.Labels)) {
				out.Items = append(out.Items, *s)
			}
		}
		return &out, nil
	}).AnyTimes()
	return m
}

func newRestoreOp() *opv1alpha1.ETCDSnapshotRestore {
	clusterRef, _ := newMgmtClusterRef()
	return &opv1alpha1.ETCDSnapshotRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-1",
			Namespace: "fleet-default",
			UID:       types.UID("restore-uid"),
		},
		Spec: opv1alpha1.ETCDSnapshotRestoreSpec{
			OperationSpec: opv1alpha1.OperationSpec{ClusterRef: clusterRef},
		},
	}
}

// captureLogs redirects logrus for the duration of the test. See the identical helper in
// etcdsnapshotsave's tests for why the process-global redirect is safe here.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	t.Cleanup(func() { logrus.SetOutput(previous) })
	return &buf
}

func TestOnChange_DeletionRunsCleanupAndDropsTheFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)

	secret := newImportedPlanSecret("plan-a", planapi.PlanStatePaused)
	secret.Annotations[planapi.PlanPausedAnnotation] = "true"
	secret.Annotations[planapi.PlanCanceledAnnotation] = "true"

	_, cluster := newMgmtClusterRef()
	ts := metav1.NewTime(time.Now())
	op := newRestoreOp()
	op.DeletionTimestamp = &ts
	op.Finalizers = []string{ops.InterruptCleanupFinalizer}

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates, secret)

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotRestore, *opv1alpha1.ETCDSnapshotRestoreList](ctrl)
	var finalFinalizers []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotRestore) (*opv1alpha1.ETCDSnapshotRestore, error) {
			finalFinalizers = o.Finalizers
			return o, nil
		}).Times(1)

	h := &handler{
		etcdsnapshotrestores: opClient,
		secrets:              secrets,
		store:                planapi.NewStore(secrets),
		dynamic:              &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotRestoreStatus{})
	assert.NoError(t, err)
	assert.Empty(t, finalFinalizers, "cleanup succeeded, so the finalizer must be dropped")

	if assert.Len(t, updates, 1, "the leftover annotations must be cleared from the machine-plan secret") {
		assert.NotContains(t, updates[0].Annotations, planapi.PlanPausedAnnotation)
		assert.NotContains(t, updates[0].Annotations, planapi.PlanCanceledAnnotation)
	}
}

func TestOnChange_DeletionForceRemovesTheFinalizerOnceTheBudgetIsSpent(t *testing.T) {
	ctrl := gomock.NewController(t)

	ts := metav1.NewTime(time.Now().Add(-2 * ops.InterruptCleanupBudget))
	op := newRestoreOp()
	op.DeletionTimestamp = &ts
	op.Finalizers = []string{ops.InterruptCleanupFinalizer}

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotRestore, *opv1alpha1.ETCDSnapshotRestoreList](ctrl)
	var finalFinalizers []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotRestore) (*opv1alpha1.ETCDSnapshotRestore, error) {
			finalFinalizers = o.Finalizers
			return o, nil
		}).Times(1)

	secrets := newRestoreSecretClient(t, ctrl, nil)
	h := &handler{
		etcdsnapshotrestores: opClient,
		secrets:              secrets,
		store:                planapi.NewStore(secrets),
		dynamic:              &fakeDynamic{getErr: errors.New("no matches for kind")},
	}

	logs := captureLogs(t)
	status, err := h.OnChange(op, opv1alpha1.ETCDSnapshotRestoreStatus{})
	assert.NoError(t, err,
		"an undeletable CR blocks namespace teardown and cluster deprovisioning; force-remove is "+
			"the lesser failure")
	assert.Empty(t, finalFinalizers)
	assert.Equal(t, opv1alpha1.ETCDSnapshotRestoreStatus{}, status,
		"rule 4 reports to the log only; a condition written here could never be read")
	assert.Contains(t, logs.String(), opv1alpha1.InterruptCleanupIncompleteReason)
}

func TestOnChange_DeletionForceRemovalNamesTheSecretsNamespaceNotTheOperations(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	ts := metav1.NewTime(time.Now().Add(-2 * ops.InterruptCleanupBudget))
	op := newRestoreOp()
	op.DeletionTimestamp = &ts
	op.Finalizers = []string{ops.InterruptCleanupFinalizer}
	require.Equal(t, "fleet-default", op.Namespace)
	require.Empty(t, op.Spec.ClusterRef.Namespace, "the mgmt v3 Cluster is cluster-scoped")

	secrets := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	secrets.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("etcdserver: request timed out")).AnyTimes()

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotRestore, *opv1alpha1.ETCDSnapshotRestoreList](ctrl)
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotRestore) (*opv1alpha1.ETCDSnapshotRestore, error) { return o, nil }).Times(1)

	h := &handler{
		etcdsnapshotrestores: opClient,
		secrets:              secrets,
		store:                planapi.NewStore(secrets),
		dynamic:              &fakeDynamic{getObj: cluster},
	}

	logs := captureLogs(t)
	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotRestoreStatus{})
	assert.NoError(t, err)

	assert.Contains(t, logs.String(),
		"kubectl annotate secret -n c-m-test -l rke.cattle.io/cluster-name=c-m-test "+
			"plan.cattle.io/canceled- plan.cattle.io/paused-")
	assert.NotContains(t, logs.String(), "-n fleet-default")
}

func TestOnChange_DeletionWithNoFinalizerIsANoOp(t *testing.T) {
	ctrl := gomock.NewController(t)

	ts := metav1.NewTime(time.Now())
	op := newRestoreOp()
	op.DeletionTimestamp = &ts

	secrets := newRestoreSecretClient(t, ctrl, nil)
	h := &handler{secrets: secrets, store: planapi.NewStore(secrets)}

	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotRestoreStatus{})
	assert.NoError(t, err, "an operation that never wrote an annotation must never be delayed")
}

func TestOnChange_TheFirstInterruptPersistsTheFinalizerBeforeAnnotatingAnything(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := newRestoreOp()
	op.Spec.Paused = true

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates,
		newImportedPlanSecret("plan-a", planapi.PlanStateInProgress))

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotRestore, *opv1alpha1.ETCDSnapshotRestoreList](ctrl)
	var written []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotRestore) (*opv1alpha1.ETCDSnapshotRestore, error) {
			written = o.Finalizers
			return o, nil
		}).Times(1)

	h := &handler{
		etcdsnapshotrestores: opClient,
		secrets:              secrets,
		store:                planapi.NewStore(secrets),
		dynamic:              &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotRestoreStatus{})
	assert.NoError(t, err)
	assert.Equal(t, []string{ops.InterruptCleanupFinalizer}, written,
		"the finalizer has to be persisted before the first annotation, never after")
	assert.Empty(t, updates,
		"no machine-plan secret may be annotated on the reconcile that writes the finalizer")
}

func TestOnChange_TerminalOperationNeverAcquiresTheFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := newRestoreOp()
	op.Spec.Paused = true

	status := opv1alpha1.ETCDSnapshotRestoreStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseSucceeded)
	op.Status = status

	secrets := newRestoreSecretClient(t, ctrl, nil, newImportedPlanSecret("plan-a", planapi.PlanStateSucceeded))
	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotRestore, *opv1alpha1.ETCDSnapshotRestoreList](ctrl)
	opClient.EXPECT().Update(gomock.Any()).Times(0)
	opClient.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	opClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	h := &handler{
		etcdsnapshotrestores: opClient,
		// A beacon owned by nobody: handleSucceeded finds this op is neither holder nor delegate
		// and is a no-op, keeping the test focused on whether a finalizer was written.
		beacons: &fakeRestoreBeaconClient{getObj: &planv1alpha1.Beacon{
			ObjectMeta: metav1.ObjectMeta{Name: "c-m-test", Namespace: "c-m-test"},
		}},
		secrets: secrets,
		store:   planapi.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnChange(op, status)
	assert.NoError(t, err)
}

// fakeRestoreBeaconClient serves the beacon lookup onChange performs after resolving the adapter.
// An unset getObj yields NotFound, matching a cluster whose beacon has not been created yet.
type fakeRestoreBeaconClient struct {
	plancontrollers.BeaconClient // embed for any unused method; nil panics signal an unexpected call

	getObj *planv1alpha1.Beacon
}

func (f *fakeRestoreBeaconClient) Get(_, name string, _ metav1.GetOptions) (*planv1alpha1.Beacon, error) {
	if f.getObj != nil {
		return f.getObj, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "beacons"}, name)
}

// --- interrupt gate --------------------------------------------------------------------------
//
// This controller's gate was previously covered by inspection only. It is the operation type where
// getting the interrupt wrong is most expensive: a false RecoveryRequired tells the user to run an
// etcd restore, and this is the etcd restore.

// newPlanlessImportedSecret builds a machine-plan Secret that has never been assigned a plan —
// exactly how pkg/controllers/capr/unmanaged creates one, with no Data at all. Every cluster with
// a node the operation's steps do not target has at least one: a worker under an etcd-scoped step,
// or a node registered mid-flight.
func newPlanlessImportedSecret(name string) *corev1.Secret {
	secret := newImportedPlanSecret(name, "")
	secret.Data = nil
	return secret
}

// newInterruptHandler builds the handler the gate tests drive. The beacon it serves is owned by
// nobody: every one of these tests asserts the gate returned before the phase dispatch, and any
// phase handler that reached the beacon would call a method fakeRestoreBeaconClient does not
// implement and panic on the embedded nil interface.
func newInterruptHandler(secrets *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList], cluster *unstructured.Unstructured) *handler {
	return &handler{
		beacons: &fakeRestoreBeaconClient{getObj: &planv1alpha1.Beacon{
			ObjectMeta: metav1.ObjectMeta{Name: "c-m-test", Namespace: "c-m-test"},
		}},
		secrets: secrets,
		store:   planapi.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}
}

func inProgress() opv1alpha1.ETCDSnapshotRestoreStatus {
	status := opv1alpha1.ETCDSnapshotRestoreStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseInProgress)
	return status
}

func TestOnChange_PauseAnnotatesSecretsAndReportsPaused(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := alreadyInterrupted(newRestoreOp())
	op.Spec.Paused = true

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates,
		newImportedPlanSecret("plan-a", planapi.PlanStateInProgress))

	got, err := newInterruptHandler(secrets, cluster).onChange(op, inProgress())
	assert.NoError(t, err)

	assert.True(t, opv1alpha1.PausedCondition.IsTrue(&got))
	assert.Equal(t, opv1alpha1.PauseRequestedReason, opv1alpha1.PausedCondition.GetReason(&got),
		"the node has not reported a paused plan state yet, so the pause is requested, not in effect")
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, got.Phase,
		"a pause holds the operation where it is; it never advances the phase")
	if assert.Len(t, updates, 1, "the pause must be propagated to the machine-plan secret") {
		assert.Equal(t, "true", updates[0].Annotations[planapi.PlanPausedAnnotation])
	}
}

func TestOnChange_CancelAnnotatesSecretsAndHoldsPhase(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := alreadyInterrupted(newRestoreOp())
	op.Spec.Cancel = true

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates,
		newImportedPlanSecret("plan-a", planapi.PlanStateInProgress))

	got, err := newInterruptHandler(secrets, cluster).onChange(op, inProgress())
	assert.NoError(t, err)

	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, got.Phase,
		"the phase must not flip until every node confirms; IsTerminal(Canceled) releases the "+
			"beacon and would let something else act on a cluster mid-restore")
	assert.Equal(t, opv1alpha1.CancelRequestedReason, opv1alpha1.CanceledCondition.GetReason(&got))
	assert.False(t, got.CancelRequestedAt.IsZero())
	if assert.Len(t, updates, 1, "the cancellation must be propagated to the machine-plan secret") {
		assert.Equal(t, "true", updates[0].Annotations[planapi.PlanCanceledAnnotation])
	}
}

func TestOnChange_SuccessfulResumeClearsThePauseViaHandleResume(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Neither paused nor cancelled in the spec, but recorded as paused on the status: the user has
	// withdrawn the pause and the annotation is still on the Secrets.
	_, cluster := newMgmtClusterRef()
	op := alreadyInterrupted(newRestoreOp())
	status := inProgress()
	opv1alpha1.PausedCondition.True(&status)
	opv1alpha1.PausedCondition.Reason(&status, opv1alpha1.PausedReason)

	secret := newImportedPlanSecret("plan-a", planapi.PlanStatePaused)
	secret.Annotations[planapi.PlanPausedAnnotation] = "true"

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates, secret)

	got, err := newInterruptHandler(secrets, cluster).onChange(op, status)
	assert.NoError(t, err)

	assert.True(t, opv1alpha1.PausedCondition.IsFalse(&got),
		"a resume that lands must clear the condition, so the next reconcile's gate lets the "+
			"operation proceed instead of re-listing the Secrets forever")
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(&got))
	if assert.Len(t, updates, 1, "the interrupt must come off the Secret") {
		assert.NotContains(t, updates[0].Annotations, planapi.PlanPausedAnnotation)
	}
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, got.Phase,
		"handleResume skips the phase dispatch for exactly the reconcile that wrote")
}

// TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan pins the read side of the unfiltered
// Secrets contract. The write side is deliberately cluster-wide — different steps target different
// node subsets, so an interrupt has to reach every Secret — but a node the operation never
// dispatched a plan to has no plan-state and never will. Under this operation type's cancel policy
// that node is doubly costly: it burns the full CancelConfirmationTimeout holding the beacon, and
// then reports RecoveryRequired, telling the user to run an etcd restore after cancelling one.
func TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := alreadyInterrupted(newRestoreOp())
	op.Spec.Cancel = true

	confirmed := newImportedPlanSecret("etcd-a", planapi.PlanStateCanceled)
	planless := newPlanlessImportedSecret("worker-b")

	var updates []*corev1.Secret
	secrets := newRestoreSecretClient(t, ctrl, &updates, confirmed, planless)

	got, err := newInterruptHandler(secrets, cluster).onChange(op, inProgress())
	assert.NoError(t, err)

	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, got.Phase,
		"a Secret with no plan has nothing to stop; waiting for it to confirm holds the beacon "+
			"for the full CancelConfirmationTimeout on every cluster with a worker node")
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got),
		"no plan was ever assigned here, which is not the legacy checksum flow and not a slow agent")

	assert.False(t, got.AnyNodeMutationObserved,
		"nothing executed: one node confirmed the cancellation and the other never had a plan")
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(&got),
		"telling a user to run an etcd restore because a worker node has no plan is the single "+
			"most expensive way this feature can be wrong")

	assert.Len(t, updates, 2,
		"the write side stays unfiltered: every machine-plan Secret is annotated, including the "+
			"plan-less one, because a later step may yet target it")
}

// --- updateStatus ----------------------------------------------------------------------------

// An operation canceled from InProgress is now a primary user-facing outcome rather than a
// near-unreachable corner, so the condition set it lands on has to agree with itself.
func TestUpdateStatus_CanceledStopsReportingInProgress(t *testing.T) {
	t.Parallel()

	op := newRestoreOp()
	op.Spec.Cancel = true

	// Exactly what reaches updateStatus after HandleInterrupt finishes a cancellation: the
	// conditions the InProgress phase left behind, plus the Canceled the gate just wrote.
	status := inProgress()
	opv1alpha1.PendingCondition.False(&status)
	opv1alpha1.PendingCondition.Reason(&status, opv1alpha1.InProgressReason)
	opv1alpha1.InProgressCondition.True(&status)
	status.SetPhase(opv1alpha1.OperationPhaseCanceled)
	opv1alpha1.CanceledCondition.True(&status)
	opv1alpha1.CanceledCondition.Reason(&status, opv1alpha1.CanceledReason)

	got := updateStatus(op, status)

	assert.True(t, opv1alpha1.InProgressCondition.IsFalse(&got),
		"a canceled operation reporting InProgress=True contradicts its own phase")
	assert.Equal(t, opv1alpha1.FinishedReason, opv1alpha1.PendingCondition.GetReason(&got),
		"the operation is not waiting to start; it is over")
	assert.True(t, opv1alpha1.SucceededCondition.IsFalse(&got))
	assert.Equal(t, opv1alpha1.NotSuccessfulReason, opv1alpha1.SucceededCondition.GetReason(&got))

	assert.True(t, opv1alpha1.CanceledCondition.IsTrue(&got),
		"CanceledCondition belongs to ops.HandleInterrupt; updateStatus must not re-derive it")
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got))
}

// alreadyInterrupted stamps the cleanup finalizer onto op, standing in for the reconcile that
// persisted it. The first reconcile to observe spec.paused or spec.cancel writes the finalizer and
// returns without touching a Secret, so a test that wants to exercise the interrupt itself has to
// start from the reconcile after that one.
func alreadyInterrupted(op *opv1alpha1.ETCDSnapshotRestore) *opv1alpha1.ETCDSnapshotRestore {
	op.Finalizers = append(op.Finalizers, ops.InterruptCleanupFinalizer)
	return op
}

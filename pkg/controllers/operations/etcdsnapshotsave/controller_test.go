package etcdsnapshotsave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	rkeplan "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1/plan"
	"github.com/rancher/rancher/pkg/capr"
	operationcontrollers "github.com/rancher/rancher/pkg/generated/controllers/operation.cattle.io/v1alpha1"
	ops "github.com/rancher/rancher/pkg/operations"
	planapi "github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	plancontrollers "github.com/rancher/rancher/pkg/plan/generated/controllers/plan.cattle.io/v1alpha1"
	"github.com/rancher/wrangler/v3/pkg/generic"
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

// captureLogs redirects logrus for the duration of the test and returns the buffer it writes to.
//
// logrus is process-global, but Go resumes t.Parallel() tests only once every sequential test in
// the package has finished, and every test that calls this one is sequential — so nothing else in
// this package can be logging while the redirect is installed.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	t.Cleanup(func() { logrus.SetOutput(previous) })
	return &buf
}

// stubAdapter is a minimal ops.Adapter implementation for tests. Each field is what the
// corresponding method returns; RenderProbes always returns the same map regardless of secret/
// supervisor flag to keep produced plans byte-deterministic across calls.
type stubAdapter struct {
	runtimeCommand     string
	dataDir            string
	provisioningDir    string
	kubectlPath        string
	kubeconfigPath     string
	serverUnit         string
	waitForRegisterOK  bool
	waitForRegisterErr error
	probes             map[string]planapi.Probe
}

func (a *stubAdapter) BeaconRef() (string, string)   { return "test-namespace", "test-cluster" }
func (a *stubAdapter) EtcdSnapshotNamespace() string { return "test-namespace" }
func (a *stubAdapter) ClusterObject() (*unstructured.Unstructured, error) {
	return &unstructured.Unstructured{}, nil
}
func (a *stubAdapter) WaitForRegister() (bool, error) {
	return a.waitForRegisterOK, a.waitForRegisterErr
}
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
// by the snapshot-save controller (which only consumes runtime/dataDir/serverUnit/probes/plans),
// so each returns a static, runtime-appropriate value.
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

// fakeDynamic satisfies the controller's dynamicResolver interface for the success-path tests.
// Enqueue records the (gvk, namespace, name) tuple so tests can assert handleSucceeded nudged
// the parent cluster.
type fakeDynamic struct {
	gets       map[string]runtime.Object
	enqueued   []string
	getErr     error
	enqueueErr error
}

func (f *fakeDynamic) Get(gvk schema.GroupVersionKind, ns, name string) (runtime.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if obj, ok := f.gets[gvk.String()+"/"+ns+"/"+name]; ok {
		return obj, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
}

func (f *fakeDynamic) Enqueue(gvk schema.GroupVersionKind, ns, name string) error {
	f.enqueued = append(f.enqueued, gvk.String()+"/"+ns+"/"+name)
	return f.enqueueErr
}

// defaultAdapter returns a fully-populated stubAdapter suitable for most reconcile tests. K3s is
// chosen because the runtime is irrelevant to the controller logic; only the rendered command
// strings matter.
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

// newScope wires together the common per-reconcile context for the tests. The ownerKey mirrors
// what the real controller computes in onChange (plan.ControllerOwnerKey(op, ControllerOwnerKey))
// so beacon fixtures created with `testOwnerKey` will match ownership + delegate checks.
func newScope(op *opv1alpha1.ETCDSnapshotSave, beacon *planv1alpha1.Beacon, adapter *stubAdapter) *scope {
	cluster := &unstructured.Unstructured{}
	cluster.SetName("test")
	cluster.SetNamespace("fleet-default")
	cluster.SetAPIVersion("provisioning.cattle.io/v1")
	cluster.SetKind("Cluster")
	return &scope{
		ownerKey:   planapi.ControllerOwnerKey(op, ControllerOwnerKey),
		op:         op,
		beacon:     beacon,
		namespace:  "fleet-default",
		clusterObj: cluster,
		adapter:    adapter,
	}
}

// testOwnerKey is the fully-qualified beacon owner key for the canonical `newOp()` operation.
// Tests that want to build a beacon owned by "us" pass this to newBeacon, instead of the plain
// ControllerOwnerKey prefix which no longer matches what the handler computes at reconcile time.
var testOwnerKey = planapi.ControllerOwnerKey(newOp(), ControllerOwnerKey)

func newOp() *opv1alpha1.ETCDSnapshotSave {
	return &opv1alpha1.ETCDSnapshotSave{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "save-1",
			Namespace: "fleet-default",
			UID:       "op-uid",
		},
		Spec: opv1alpha1.ETCDSnapshotSaveSpec{
			OperationSpec: opv1alpha1.OperationSpec{},
		},
	}
}

// alreadyInterrupted stamps the cleanup finalizer onto op, standing in for the reconcile that
// persisted it. The first reconcile to observe spec.paused or spec.cancel writes the finalizer and
// returns without touching a Secret, so a test that wants to exercise the interrupt itself has to
// start from the reconcile after that one.
func alreadyInterrupted(op *opv1alpha1.ETCDSnapshotSave) *opv1alpha1.ETCDSnapshotSave {
	op.Finalizers = append(op.Finalizers, ops.InterruptCleanupFinalizer)
	return op
}

func newBeacon(owner string, active bool) *planv1alpha1.Beacon {
	// Beacon ownership lives on Status.Owner (the plan.AcquireBeacon helper writes there).
	// We populate the legacy BeaconOwnerLabel too so any caller that still reads it (e.g.
	// EncryptionKeyRotation's reclaimStaleBeaconOwnerIfNeeded) keeps working.
	return &planv1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "fleet-default",
		},
		Status: planv1alpha1.BeaconStatus{
			Active: active,
			Owner:  owner,
		},
	}
}

// newPlanSecret builds a machine-plan secret carrying the cluster-name + etcd-role labels and
// non-nil Annotations (the plan.Store assumes Annotations is non-nil).
func newPlanSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "fleet-default",
			UID:         types.UID(name + "-uid"),
			Annotations: map[string]string{},
			Labels: map[string]string{
				capr.ClusterNameLabel: "test",
				capr.EtcdRoleLabel:    "true",
			},
		},
		Type: planapi.SecretTypeMachinePlan,
	}
}

// withAppliedPlan returns a copy of the secret pre-populated to look like the system-agent has
// already applied the given plan with healthy probes — used to fast-forward reconcile tests
// through the "wait for plan" branch.
func withAppliedPlan(secret *corev1.Secret, expectedPlan *planapi.Plan) *corev1.Secret {
	out := secret.DeepCopy()
	data, _ := json.Marshal(expectedPlan)
	if out.Data == nil {
		out.Data = map[string][]byte{}
	}
	out.Data["plan"] = data
	out.Data["appliedPlan"] = data
	out.Data["probe-statuses"] = []byte(`{"x":{"healthy":true}}`)
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[planapi.PlanProbesPassedAnnotation] = "applied"
	return out
}

// withFailedPlan marks the secret as having failed its plan past the failure threshold so the
// store reports Failure() == true.
func withFailedPlan(secret *corev1.Secret, expectedPlan *planapi.Plan) *corev1.Secret {
	out := secret.DeepCopy()
	data, _ := json.Marshal(expectedPlan)
	if out.Data == nil {
		out.Data = map[string][]byte{}
	}
	out.Data["plan"] = data
	out.Data["failed-checksum"] = []byte(planapi.PlanHash(data))
	out.Data["failure-count"] = []byte("5")
	out.Data["max-failures"] = []byte("1")
	out.Data["failure-threshold"] = []byte("1")
	return out
}

// newSecretClient mocks the SecretClient used by planapi.Store.AssignPlan. Update echoes the
// passed-in secret back to the caller so the store treats it as the "post-update" state.
func newSecretClient(t *testing.T, ctrl *gomock.Controller, items ...*corev1.Secret) *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	return newRecordingSecretClient(t, ctrl, nil, nil, items...)
}

// newRecordingSecretClient is newSecretClient plus a capture of every secret handed to Update, so
// a test can assert on what the controller wrote back, and an optional updateErr standing in for a
// write that fails against the API server. Attempts are recorded whether or not they succeed.
// Pass a nil recorder to discard the writes.
func newRecordingSecretClient(t *testing.T, ctrl *gomock.Controller, updates *[]*corev1.Secret, updateErr error, items ...*corev1.Secret) *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	m := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	m.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		if updates != nil {
			*updates = append(*updates, s.DeepCopy())
		}
		if updateErr != nil {
			return nil, updateErr
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
			if s.Namespace != ns {
				continue
			}
			if !sel.Matches(labels.Set(s.Labels)) {
				continue
			}
			out.Items = append(out.Items, *s)
		}
		return &out, nil
	}).AnyTimes()
	return m
}

// fakeBeaconClient is a tiny in-memory implementation of plancontrollers.BeaconClient covering
// only Update and UpdateStatus — the only methods AcquireBeacon/ReleaseBeacon/ToggleBeacon call.
// Building a gomock interface for the full ClientInterface would dwarf the controller logic
// under test; this hand-written stub keeps the assertions readable.
type fakeBeaconClient struct {
	plancontrollers.BeaconClient // embed for any unused method; nil panics signal an unexpected call

	getObj        *planv1alpha1.Beacon
	updates       []*planv1alpha1.Beacon
	statusUpdates []*planv1alpha1.Beacon
	updateErr     error
}

// Get serves the beacon lookup onChange performs after resolving the cluster adapter. An unset
// getObj yields NotFound, matching a cluster whose beacon has not been created yet.
func (f *fakeBeaconClient) Get(_, name string, _ metav1.GetOptions) (*planv1alpha1.Beacon, error) {
	if f.getObj != nil {
		return f.getObj, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "beacons"}, name)
}

func (f *fakeBeaconClient) Update(b *planv1alpha1.Beacon) (*planv1alpha1.Beacon, error) {
	f.updates = append(f.updates, b.DeepCopy())
	return b, f.updateErr
}

func (f *fakeBeaconClient) UpdateStatus(b *planv1alpha1.Beacon) (*planv1alpha1.Beacon, error) {
	f.statusUpdates = append(f.statusUpdates, b.DeepCopy())
	return b, nil
}

// --- onChange -------------------------------------------------------------------------------

func TestOnChange_PausedWithMissingClusterFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	op := newOp()
	op.Spec.Paused = true
	op.Spec.ClusterRef = &corev1.ObjectReference{
		APIVersion: "provisioning.cattle.io/v1",
		Kind:       "Cluster",
		Namespace:  "fleet-default",
		Name:       "gone",
	}

	h := &handler{
		beacons: &fakeBeaconClient{},
		secrets: newSecretClient(t, ctrl),
		dynamic: &fakeDynamic{}, // every Get returns NotFound
		store:   planapi.NewStore(newSecretClient(t, ctrl)),
	}

	status, err := h.onChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, status.Phase,
		"cluster resolution now runs before the pause gate, so a paused op against a deleted cluster fails instead of sitting inert")
	assert.Equal(t, opv1alpha1.ClusterNotFoundReason, opv1alpha1.FailedCondition.GetReason(&status))
}

// newMgmtClusterRef builds a ClusterRef/cluster pair pointing at a plain imported mgmt v3
// Cluster. That GVK is the only adapter ops.NewAdapter can build without a live wrangler context,
// which is what lets onChange be driven end-to-end in a unit test. ImportedAdapter.BeaconRef is
// (name, name), so the cluster's own name doubles as the namespace holding its beacon and its
// machine-plan Secrets.
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

// newImportedPlanSecret builds a machine-plan secret in the namespace ImportedAdapter resolves for
// newMgmtClusterRef's cluster, carrying the agent-authored plan state the interrupt machinery
// reads.
func newImportedPlanSecret(name string, state planapi.PlanState) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "c-m-test",
			UID:         types.UID(name + "-uid"),
			Annotations: map[string]string{},
			Labels: map[string]string{
				capr.ClusterNameLabel: "c-m-test",
			},
		},
		Type: planapi.SecretTypeMachinePlan,
		Data: map[string][]byte{
			"plan":               []byte(`{"instructions":[]}`),
			planapi.PlanStateKey: []byte(state),
		},
	}
}

// newPlanlessImportedSecret builds a machine-plan Secret that has never been assigned a plan —
// exactly how pkg/controllers/capr/unmanaged creates one, with no Data at all. Every cluster with
// a node the operation's steps do not target has at least one: a worker under an etcd-scoped step,
// or a node registered mid-flight.
func newPlanlessImportedSecret(name string) *corev1.Secret {
	secret := newImportedPlanSecret(name, "")
	secret.Data = nil
	return secret
}

func TestCancelPolicyRegistered(t *testing.T) {
	t.Parallel()

	policy := ops.CancelPolicyFor(operationGVK)
	assert.False(t, policy.RequiresRecovery,
		"a partial snapshot save leaves nothing for the user to recover from")
	assert.Empty(t, policy.RecoveryMessage)
}

func TestOnChange_CancelAnnotatesSecretsAndHoldsPhase(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Cancel = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	secret := newImportedPlanSecret("plan-a", planapi.PlanStateInProgress)

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, nil, secret)
	h := &handler{
		beacons: &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)},
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseInProgress)

	got, err := h.onChange(op, status)
	assert.NoError(t, err)

	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, got.Phase,
		"the phase must not flip until every node confirms; IsTerminal(Canceled) releases the beacon")
	assert.Equal(t, opv1alpha1.CancelRequestedReason, opv1alpha1.CanceledCondition.GetReason(&got))
	assert.False(t, got.CancelRequestedAt.IsZero())

	if assert.Len(t, updates, 1, "the cancellation must be propagated to the machine-plan secret") {
		assert.Equal(t, "true", updates[0].Annotations[planapi.PlanCanceledAnnotation])
	}
}

// TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan pins the read side of the unfiltered
// Secrets contract. The write side is deliberately cluster-wide — different steps target different
// node subsets, so an interrupt has to reach every Secret — but a node the operation never
// dispatched a plan to has no plan-state and never will, so waiting for it to confirm burns the
// full CancelConfirmationTimeout holding the beacon on every cluster with a dedicated worker.
func TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := alreadyInterrupted(newOp())
	op.Spec.Cancel = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	// The etcd node the save targeted has confirmed. The worker never carried a plan for this
	// operation at all.
	confirmed := newImportedPlanSecret("etcd-a", planapi.PlanStateCanceled)
	planless := newPlanlessImportedSecret("worker-b")

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, nil, confirmed, planless)
	h := &handler{
		beacons: &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)},
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseInProgress)

	got, err := h.onChange(op, status)
	assert.NoError(t, err)

	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, got.Phase,
		"a Secret with no plan has nothing to stop; waiting for it to confirm holds the beacon "+
			"for the full CancelConfirmationTimeout on every cluster with a worker node")
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got),
		"no plan was ever assigned here, which is not the legacy checksum flow and not a slow agent")

	assert.Len(t, updates, 2,
		"the write side stays unfiltered: every machine-plan Secret is annotated, including the "+
			"plan-less one, because a later step may yet target it")
}

// runStatusHandler drives op through the *generated* status handler — the same code path the
// wrangler-registered controller uses in production — and returns the status that was actually
// persisted, or nil when nothing was written.
//
// Calling h.OnChange directly would not do. The whole question on the interrupt error path is
// whether the status survives, and that is decided by generated code this package does not own:
// the handler reverts to the pre-reconcile status on any returned error
// (`etcdsnapshotsave.go:92-100`), and OnChange skips updateStatus on top of that. A hand-rolled
// stand-in would happily assert a guarantee the real one does not give.
func runStatusHandler(t *testing.T, ctrl *gomock.Controller, h *handler, op *opv1alpha1.ETCDSnapshotSave) *opv1alpha1.ETCDSnapshotSaveStatus {
	t.Helper()

	var sync generic.Handler
	saves := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)
	saves.EXPECT().AddGenericHandler(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, _ string, handler generic.Handler) { sync = handler })
	saves.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	var persisted *opv1alpha1.ETCDSnapshotSaveStatus
	saves.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(o *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, error) {
		persisted = o.Status.DeepCopy()
		return o, nil
	}).AnyTimes()

	h.etcdsnapshotsaves = saves
	operationcontrollers.RegisterETCDSnapshotSaveStatusHandler(context.Background(), saves, "", "test", h.OnChange)
	require.NotNil(t, sync, "RegisterETCDSnapshotSaveStatusHandler must have installed a handler")

	_, err := sync(op.Namespace+"/"+op.Name, op)
	assert.NoError(t, err, "the interrupt gate must not propagate its error; the framework would revert the status")

	return persisted
}

func TestOnChange_InterruptFailurePersistsTheLatchedEvidence(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := alreadyInterrupted(newOp())
	op.Spec.Cancel = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	op.Status.SetPhase(opv1alpha1.OperationPhaseInProgress)

	// "a" carries the agent's terminationIncomplete checkpoint. "b" has an unparsable
	// failure-count, which makes Store.Status error on every reconcile forever. DefaultSorter
	// breaks ties by name, so the evidence on "a" is latched before "b" fails the call.
	good := newImportedPlanSecret("a", planapi.PlanStateInProgress)
	good.Data[planapi.PlanCheckpointKey] = []byte(fmt.Sprintf(
		`{"checksum":%q,"terminationIncomplete":true}`, planapi.Checksum(good.Data["plan"])))
	bad := newImportedPlanSecret("b", planapi.PlanStateInProgress)
	bad.Data["failed-checksum"] = []byte(planapi.PlanHash(bad.Data["plan"]))
	bad.Data["failure-count"] = []byte("not-a-number")

	secrets := newSecretClient(t, ctrl, good, bad)
	beacons := &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted, "the failed reconcile must still write a status")
	assert.True(t, persisted.TerminationIncomplete,
		"the agent clears plan-progress on its next completed apply, so evidence dropped on an "+
			"error path never comes back")
	assert.False(t, persisted.CancelRequestedAt.IsZero(),
		"without a persisted stamp the failure budget has nothing to measure and never expires")
	assert.Equal(t, opv1alpha1.CancelEvaluationFailedReason,
		opv1alpha1.CanceledCondition.GetReason(persisted),
		"a stuck interrupt must be visible on the operation, not only in the log")

	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, persisted.Phase,
		"the gate must not fall through to the phase dispatch; handleInProgress would find an "+
			"empty Step and mark the operation Failed")
	assert.Empty(t, beacons.statusUpdates, "no phase handler may touch the beacon")
}

func TestOnChange_InterruptFailureOnAnEmptyCollectGivesUpAtOnce(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := alreadyInterrupted(newOp())
	op.Spec.Cancel = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	op.Status.SetPhase(opv1alpha1.OperationPhaseInProgress)

	// No machine-plan secrets at all. The AtLeast validator turns that into a non-transient
	// CollectorError, which must not be retried for 15 minutes while the beacon is held.
	secrets := newSecretClient(t, ctrl)
	h := &handler{
		beacons: &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)},
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted)
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, persisted.Phase,
		"an empty collect can never become non-empty by retrying, so holding the beacon on it is pure cost")
	assert.Equal(t, opv1alpha1.CancelEvaluationFailedReason, opv1alpha1.CanceledCondition.GetReason(persisted))
}

func TestOnChange_ResumeFailureOnAnEmptyCollectFailsTheOperation(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Neither paused nor cancelled in the spec, but recorded as paused on the status: this is a
	// user withdrawing a pause, which is the case ExpireFailedInterrupt must not leave wedged.
	op := newOp()
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	op.Status.SetPhase(opv1alpha1.OperationPhaseInProgress)
	opv1alpha1.PausedCondition.True(&op.Status)
	opv1alpha1.PausedCondition.Reason(&op.Status, opv1alpha1.PausedReason)

	secrets := newSecretClient(t, ctrl)
	beacons := &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, persisted.Phase,
		"a user who resumes asked the operation to proceed; wedging it forever holding the beacon "+
			"is the opposite, and spec.cancel must not be the only way out")
	assert.Equal(t, opv1alpha1.ResumeFailedReason, opv1alpha1.FailedCondition.GetReason(persisted))
	assert.True(t, opv1alpha1.PausedCondition.IsFalse(persisted))
}

func TestOnChange_ResumeFailurePersistsThePausedConditionSoTheNextReconcileRetries(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Neither paused nor cancelled in the spec, but recorded as paused on the status: the user has
	// withdrawn a pause and Rancher has not managed to lift it from the Secrets yet.
	op := newOp()
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	op.Status.SetPhase(opv1alpha1.OperationPhaseInProgress)
	opv1alpha1.PausedCondition.True(&op.Status)
	opv1alpha1.PausedCondition.Reason(&op.Status, opv1alpha1.PausedReason)

	secret := newImportedPlanSecret("a", planapi.PlanStateInProgress)
	secret.Annotations[planapi.PlanPausedAnnotation] = "true"

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, errors.New("etcdserver: request timed out"), secret)
	beacons := &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted, "the failed resume must write a status")
	assert.True(t, opv1alpha1.PausedCondition.IsTrue(persisted),
		"PausedCondition is the only user-visible evidence that the resume was dropped, and it is "+
			"also handleResume's record that there is still an interrupt to lift; updateStatus "+
			"must not erase it out from under HandleInterrupt")
	assert.Equal(t, opv1alpha1.ResumeFailedReason, opv1alpha1.PausedCondition.GetReason(persisted))
	assert.Equal(t, "true", secret.Annotations[planapi.PlanPausedAnnotation],
		"the agents really are still halted, which is what the condition has to keep saying")
	assert.Len(t, updates, 1, "the resume attempted exactly one write, and it failed")

	// The reconcile after the failure must retry the resume rather than fall through to the phase
	// dispatch. Falling through would run a phase handler against nodes still halted by the paused
	// annotation, with nothing on the status to say so.
	next := op.DeepCopy()
	next.Status = *persisted

	assert.Nil(t, runStatusHandler(t, ctrl, h, next),
		"a retry that reports the same thing must leave the status byte-identical, which is what "+
			"keeps the 5s re-enqueue from becoming a write loop")
	assert.Len(t, updates, 2,
		"handleResume must have run again; one attempt total would mean its PausedCondition gate "+
			"read False and the operation fell through to the phase dispatch")
	assert.Empty(t, beacons.statusUpdates, "no phase handler may touch the beacon")
}

// --- updateStatus ---------------------------------------------------------------------------

func TestUpdateStatusPaused(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Paused = true
	op.Generation = 7

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	// Stand in for what HandleInterrupt wrote earlier in the same reconcile.
	opv1alpha1.PausedCondition.True(&status)
	opv1alpha1.PausedCondition.Reason(&status, opv1alpha1.PausedReason)

	status = updateStatus(op, status)

	assert.Equal(t, int64(7), status.ObservedGeneration, "ObservedGeneration must be copied from the op")
	assert.Equal(t, "True", opv1alpha1.PausedCondition.GetStatus(&status))
	assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(&status))
}

func TestUpdateStatus_DoesNotClobberTheInterruptPausedReason(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Paused = true

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	// Stand in for what HandleInterrupt wrote earlier in the same reconcile.
	opv1alpha1.PausedCondition.True(&status)
	opv1alpha1.PausedCondition.Reason(&status, opv1alpha1.PauseRequestedReason)

	got := updateStatus(op, status)
	assert.Equal(t, opv1alpha1.PauseRequestedReason, opv1alpha1.PausedCondition.GetReason(&got),
		"updateStatus must not re-derive PausedCondition from the spec alone; that loses the "+
			"requested-vs-in-effect distinction HandleInterrupt computes from the agent")
}

func TestUpdateStatus_DoesNotClobberACanceledOperationsClearedPause(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Paused = true
	op.Spec.Cancel = true

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	// HandleInterrupt clears the pause when a cancel lands, because cancel beats pause on the
	// machine-plan Secrets too. Re-deriving from the spec would put it straight back.
	opv1alpha1.PausedCondition.False(&status)
	opv1alpha1.PausedCondition.Reason(&status, opv1alpha1.NotPausedReason)

	got := updateStatus(op, status)
	assert.True(t, opv1alpha1.PausedCondition.IsFalse(&got),
		"a canceled operation must not report Paused alongside Canceled")
}

func TestUpdateStatus_NeverTouchesThePausedCondition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		paused bool
		seed   func(*opv1alpha1.ETCDSnapshotSaveStatus)
		want   string
	}{
		{
			name: "unset stays unset",
			want: "",
		},
		{
			// The case that mattered: !op.Spec.Paused is the resume case, and clearing here erased
			// handleResume's record that there is still an interrupt to lift off the Secrets.
			name: "a failing resume's record survives",
			seed: func(s *opv1alpha1.ETCDSnapshotSaveStatus) {
				opv1alpha1.PausedCondition.True(s)
				opv1alpha1.PausedCondition.Reason(s, opv1alpha1.ResumeFailedReason)
			},
			want: opv1alpha1.ResumeFailedReason,
		},
		{
			name:   "a pause in effect survives",
			paused: true,
			seed: func(s *opv1alpha1.ETCDSnapshotSaveStatus) {
				opv1alpha1.PausedCondition.True(s)
				opv1alpha1.PausedCondition.Reason(s, opv1alpha1.PausedReason)
			},
			want: opv1alpha1.PausedReason,
		},
		{
			name: "a completed resume's clear survives",
			seed: func(s *opv1alpha1.ETCDSnapshotSaveStatus) {
				opv1alpha1.PausedCondition.False(s)
				opv1alpha1.PausedCondition.Reason(s, opv1alpha1.NotPausedReason)
			},
			want: opv1alpha1.NotPausedReason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			op := newOp()
			op.Spec.Paused = tc.paused

			status := opv1alpha1.ETCDSnapshotSaveStatus{}
			if tc.seed != nil {
				tc.seed(&status)
			}
			before := status.DeepCopy()

			got := updateStatus(op, status)

			assert.Equal(t, tc.want, opv1alpha1.PausedCondition.GetReason(&got),
				"ops.HandleInterrupt owns PausedCondition outright; updateStatus deriving it from "+
					"the spec in either direction clobbers handlePause, handleResume and handleCancel")
			assert.Equal(t, opv1alpha1.PausedCondition.GetStatus(before),
				opv1alpha1.PausedCondition.GetStatus(&got))
		})
	}
}

func TestOnChange_SuccessfulResumeClearsThePauseViaHandleResume(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Same setup as the failing-resume test, but the Secret write succeeds. Nothing in
	// updateStatus clears PausedCondition any more, so if it ends up cleared it can only be
	// handleResume that did it.
	op := newOp()
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	op.Status.SetPhase(opv1alpha1.OperationPhaseInProgress)
	opv1alpha1.PausedCondition.True(&op.Status)
	opv1alpha1.PausedCondition.Reason(&op.Status, opv1alpha1.PausedReason)

	secret := newImportedPlanSecret("a", planapi.PlanStateInProgress)
	secret.Annotations[planapi.PlanPausedAnnotation] = "true"

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, nil, secret)
	beacons := &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted)
	assert.True(t, opv1alpha1.PausedCondition.IsFalse(persisted),
		"a resume that lands must clear the condition, so the next reconcile's gate lets the "+
			"operation proceed instead of re-listing the Secrets forever")
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(persisted))
	if assert.Len(t, updates, 1, "the interrupt must come off the Secret") {
		assert.NotContains(t, updates[0].Annotations, planapi.PlanPausedAnnotation)
	}
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, persisted.Phase,
		"handleResume skips the phase dispatch for exactly the reconcile that wrote")
	assert.Empty(t, beacons.statusUpdates)
}

// An operation canceled from InProgress is now a primary user-facing outcome rather than a
// near-unreachable corner, so the condition set it lands on has to agree with itself.
func TestUpdateStatus_CanceledStopsReportingInProgress(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Cancel = true

	// Exactly what reaches updateStatus after HandleInterrupt finishes a cancellation: the
	// conditions the InProgress phase left behind, plus the Canceled the gate just wrote.
	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseInProgress)
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

func TestUpdateStatusByPhase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		phase opv1alpha1.OperationPhase
		check func(t *testing.T, status opv1alpha1.ETCDSnapshotSaveStatus)
	}{
		{
			name:  "pending sets Pending=True",
			phase: opv1alpha1.OperationPhasePending,
			check: func(t *testing.T, s opv1alpha1.ETCDSnapshotSaveStatus) {
				assert.Equal(t, "True", opv1alpha1.PendingCondition.GetStatus(&s))
			},
		},
		{
			name:  "in-progress clears Pending",
			phase: opv1alpha1.OperationPhaseInProgress,
			check: func(t *testing.T, s opv1alpha1.ETCDSnapshotSaveStatus) {
				assert.Equal(t, "False", opv1alpha1.PendingCondition.GetStatus(&s))
				assert.Equal(t, opv1alpha1.InProgressReason, opv1alpha1.PendingCondition.GetReason(&s))
			},
		},
		{
			name:  "succeeded clears Pending+InProgress+Failed",
			phase: opv1alpha1.OperationPhaseSucceeded,
			check: func(t *testing.T, s opv1alpha1.ETCDSnapshotSaveStatus) {
				assert.Equal(t, "False", opv1alpha1.PendingCondition.GetStatus(&s))
				assert.Equal(t, "False", opv1alpha1.InProgressCondition.GetStatus(&s))
				assert.Equal(t, "False", opv1alpha1.FailedCondition.GetStatus(&s))
				assert.Equal(t, opv1alpha1.NotFailedReason, opv1alpha1.FailedCondition.GetReason(&s))
			},
		},
		{
			name:  "failed clears Pending+InProgress+Succeeded",
			phase: opv1alpha1.OperationPhaseFailed,
			check: func(t *testing.T, s opv1alpha1.ETCDSnapshotSaveStatus) {
				assert.Equal(t, "False", opv1alpha1.PendingCondition.GetStatus(&s))
				assert.Equal(t, "False", opv1alpha1.InProgressCondition.GetStatus(&s))
				assert.Equal(t, "False", opv1alpha1.SucceededCondition.GetStatus(&s))
				assert.Equal(t, opv1alpha1.NotSuccessfulReason, opv1alpha1.SucceededCondition.GetReason(&s))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := newOp()
			status := updateStatus(op, opv1alpha1.ETCDSnapshotSaveStatus{
				OperationStatus: opv1alpha1.OperationStatus{Phase: tc.phase},
			})
			tc.check(t, status)
		})
	}
}

// --- handlePending --------------------------------------------------------------------------

func TestHandlePending_NilBeacon(t *testing.T) {
	t.Parallel()

	h := &handler{beacons: &fakeBeaconClient{}}
	s := newScope(newOp(), nil, defaultAdapter())

	got, err := h.handlePending(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// AcquireBeacon returns nil when beacon is nil → handler keeps Pending and reports waiting.
	assert.Empty(t, string(got.Phase), "no phase set yet")
	assert.Equal(t, opv1alpha1.WaitingForBeaconReason, opv1alpha1.PendingCondition.GetReason(&got))
}

func TestHandlePending_BeaconOwnedByOther(t *testing.T) {
	t.Parallel()

	h := &handler{beacons: &fakeBeaconClient{}}
	s := newScope(newOp(), newBeacon("some-other-controller", false), defaultAdapter())

	got, err := h.handlePending(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// AcquireBeacon returns nil when another controller owns it → still waiting.
	assert.Empty(t, string(got.Phase))
	assert.Equal(t, opv1alpha1.WaitingForBeaconReason, opv1alpha1.PendingCondition.GetReason(&got))
}

func TestHandlePending_WaitingForRegistration(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	adapter := defaultAdapter()
	adapter.waitForRegisterOK = false

	h := &handler{beacons: beacons}
	s := newScope(newOp(), newBeacon(testOwnerKey, false), adapter)

	got, err := h.handlePending(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, string(got.Phase), "phase must not advance until all agents have registered")
	assert.Equal(t, opv1alpha1.WaitingForRegistrationReason, opv1alpha1.PendingCondition.GetReason(&got))
}

func TestHandlePending_TransitionsToInProgress(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	h := &handler{beacons: beacons}
	a := defaultAdapter()
	a.waitForRegisterOK = true
	s := newScope(newOp(), newBeacon(testOwnerKey, false), a)

	got, err := h.handlePending(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, got.Phase)
	assert.Equal(t, opv1alpha1.ETCDSnapshotSaveStepPreflight, got.Step)
}

func TestHandlePending_WaitForRegisterErrorBubbles(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("kaboom")
	adapter := defaultAdapter()
	adapter.waitForRegisterErr = sentinel
	adapter.waitForRegisterOK = false

	h := &handler{beacons: &fakeBeaconClient{}}
	_, err := h.handlePending(newScope(newOp(), newBeacon(testOwnerKey, false), adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.ErrorIs(t, err, sentinel)
}

// --- handleInProgress -----------------------------------------------------------------------

func TestHandleInProgress_BeaconLost(t *testing.T) {
	t.Parallel()

	h := &handler{beacons: &fakeBeaconClient{}}
	// Beacon owned by some other controller → handler must fail rather than continue.
	s := newScope(newOp(), newBeacon("other", true), defaultAdapter())

	got, err := h.handleInProgress(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, got.Phase)
	assert.Equal(t, opv1alpha1.BeaconLostReason, opv1alpha1.FailedCondition.GetReason(&got))
}

func TestHandleInProgress_UnknownStep(t *testing.T) {
	t.Parallel()

	h := &handler{beacons: &fakeBeaconClient{}}
	op := newOp()
	op.Status.Step = "Whatever"
	s := newScope(op, newBeacon(testOwnerKey, false), defaultAdapter())

	got, err := h.handleInProgress(s, opv1alpha1.ETCDSnapshotSaveStatus{Step: "Whatever"})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, got.Phase)
	assert.Equal(t, opv1alpha1.UnknownStepReason, opv1alpha1.FailedCondition.GetReason(&got))
}

// --- handleFailed / handleSucceeded --------------------------------------------------------

func TestHandleFailed_HoldingBeaconReleases(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	h := &handler{beacons: beacons}
	s := newScope(newOp(), newBeacon(testOwnerKey, true), defaultAdapter())

	_, err := h.handleFailed(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// ReleaseBeacon on the owner path clears Active + Owner + Delegates in a single UpdateStatus
	// call, so we expect exactly one status update and no main-resource update.
	if assert.Len(t, beacons.statusUpdates, 1, "ReleaseBeacon should update the beacon status") {
		assert.False(t, beacons.statusUpdates[0].Status.Active, "beacon must be toggled inactive on release")
		assert.Equal(t, "", beacons.statusUpdates[0].Status.Owner, "Status.Owner must be cleared on release")
	}
	assert.Empty(t, beacons.updates, "ReleaseBeacon no longer touches the main resource")
}

func TestHandleFailed_NotHoldingNoOp(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	h := &handler{beacons: beacons}
	s := newScope(newOp(), newBeacon("other", true), defaultAdapter())

	_, err := h.handleFailed(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, beacons.updates, "non-owners must not touch the beacon")
	assert.Empty(t, beacons.statusUpdates)
}

func TestHandleSucceeded_NotHoldingNoOp(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	dyn := &fakeDynamic{}
	h := &handler{beacons: beacons, dynamic: dyn}
	s := newScope(newOp(), newBeacon("other", true), defaultAdapter())

	_, err := h.handleSucceeded(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, beacons.updates)
	assert.Empty(t, dyn.enqueued, "non-owners must not enqueue the cluster")
}

func TestHandleSucceeded_HoldingBeaconEnqueuesCluster(t *testing.T) {
	t.Parallel()

	beacons := &fakeBeaconClient{}
	dyn := &fakeDynamic{}
	h := &handler{beacons: beacons, dynamic: dyn}
	s := newScope(newOp(), newBeacon(testOwnerKey, true), defaultAdapter())

	_, err := h.handleSucceeded(s, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// ReleaseBeacon on the owner path clears Active + Owner + Delegates in a single UpdateStatus
	// call.
	if assert.Len(t, beacons.statusUpdates, 1, "ReleaseBeacon should update the beacon status") {
		assert.False(t, beacons.statusUpdates[0].Status.Active, "beacon must be toggled inactive on release")
		assert.Equal(t, "", beacons.statusUpdates[0].Status.Owner, "Status.Owner must be cleared on release")
	}
	assert.Empty(t, beacons.updates, "ReleaseBeacon no longer touches the main resource")
	if assert.Len(t, dyn.enqueued, 1, "parent cluster must be re-enqueued") {
		assert.Equal(t, "provisioning.cattle.io/v1, Kind=Cluster/fleet-default/test", dyn.enqueued[0])
	}
}

// --- reconcileSave --------------------------------------------------------------------------

// expectedSaveInstruction builds the snapshot save instruction the controller will dispatch given
// an op spec and stubAdapter, so tests can predict the exact plan bytes the agent will see.
func expectedSaveInstruction(op *opv1alpha1.ETCDSnapshotSave, runtime string) planapi.OneTimeInstruction {
	args := []string{"etcd-snapshot", "save"}
	if op.Spec.Args.Name != "" {
		args = append(args, "--name", op.Spec.Args.Name)
	}
	return planapi.OneTimeInstruction{
		CommonInstruction: planapi.CommonInstruction{
			Name:    "snapshot",
			Command: runtime,
			Args:    args,
		},
	}
}

func expectedSavePlan(op *opv1alpha1.ETCDSnapshotSave, adapter *stubAdapter) *planapi.Plan {
	return &planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{expectedSaveInstruction(op, adapter.runtimeCommand)},
		Probes:              adapter.probes,
	}
}

func expectedRestartPlan(adapter *stubAdapter) *planapi.Plan {
	return &planapi.Plan{
		OneTimeInstructions: []planapi.OneTimeInstruction{
			{CommonInstruction: planapi.CommonInstruction{
				Name:    "restart",
				Command: "systemctl",
				Args:    []string{"restart", adapter.serverUnit},
			}},
		},
		Probes: adapter.probes,
	}
}

func TestReconcileSave_NoSecrets(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	h := &handler{
		secrets: newSecretClient(t, ctrl),
	}
	h.store = planapi.NewStore(h.secrets)

	status, err := h.reconcileSave(newScope(newOp(), nil, defaultAdapter()), opv1alpha1.ETCDSnapshotSaveStatus{})
	// The Collector validator surfaces the empty-set condition as an error; the outer status
	// handler will requeue (and the op stays in its current phase until the situation resolves).
	assert.NoError(t, err, "terminal errors should not trigger reenqueue")
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, status.Phase, "terminal errors should cause operation to fail")
}

func TestReconcileSave_WaitsForPlanApply(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := newPlanSecret("etcd-1") // no plan applied yet → first dispatch
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileSave(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// Plan was just delivered to the agent — controller must report InProgress and let the next
	// reconcile poll feedback.
	assert.Empty(t, string(got.Phase), "phase must not advance while a plan is pending")
	assert.Equal(t, opv1alpha1.WaitingForPlanAppliedReason, opv1alpha1.InProgressCondition.GetReason(&got))
}

func TestReconcileSave_TransitionsToRestartWhenApplied(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := withAppliedPlan(newPlanSecret("etcd-1"), expectedSavePlan(op, adapter))
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileSave(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, string(got.Phase), "phase must not change on a clean transition")
	assert.Equal(t, opv1alpha1.ETCDSnapshotSaveStepRestart, got.Step)
}

func TestReconcileSave_PlanFailureMarksFailed(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := withFailedPlan(newPlanSecret("etcd-1"), expectedSavePlan(op, adapter))
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileSave(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, got.Phase)
	assert.Equal(t, opv1alpha1.PlanFailedReason, opv1alpha1.FailedCondition.GetReason(&got))
}

func TestReconcileSave_AppliesSnapshotArgs(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	op.Spec.Args.Name = "my-snap"
	adapter := defaultAdapter()

	// Pre-populate so the test traverses the "applied" branch without needing additional poll
	// cycles — we're asserting on the *plan content* not the wait behaviour here.
	expectedPlan := expectedSavePlan(op, adapter)
	secret := withAppliedPlan(newPlanSecret("etcd-1"), expectedPlan)
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	_, err := h.reconcileSave(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)

	wantArgs := []string{"etcd-snapshot", "save", "--name", "my-snap"}
	if !reflect.DeepEqual(expectedPlan.OneTimeInstructions[0].Args, wantArgs) {
		t.Errorf("plan args = %v, want %v — snapshot Args were not threaded through", expectedPlan.OneTimeInstructions[0].Args, wantArgs)
	}
}

// --- reconcileRestart -----------------------------------------------------------------------

func TestReconcileRestart_MarksSucceededWhenApplied(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := withAppliedPlan(newPlanSecret("etcd-1"), expectedRestartPlan(adapter))
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileRestart(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseSucceeded, got.Phase)
	assert.Equal(t, opv1alpha1.FinishedReason, opv1alpha1.SucceededCondition.GetReason(&got))
}

func TestReconcileRestart_WaitsForPlanApply(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := newPlanSecret("etcd-1") // no plan applied yet
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileRestart(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, string(got.Phase), "phase must not advance to Succeeded while restart is pending")
	assert.Equal(t, opv1alpha1.WaitingForPlanAppliedReason, opv1alpha1.InProgressCondition.GetReason(&got))
}

func TestReconcileRestart_PlanFailureMarksFailed(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	secret := withFailedPlan(newPlanSecret("etcd-1"), expectedRestartPlan(adapter))
	h := &handler{
		secrets: newSecretClient(t, ctrl, secret),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileRestart(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, got.Phase)
	assert.Equal(t, opv1alpha1.PlanFailedReason, opv1alpha1.FailedCondition.GetReason(&got))
}

func TestReconcileRestart_FiltersToEtcdSecrets(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	op := newOp()
	adapter := defaultAdapter()

	etcd := withAppliedPlan(newPlanSecret("etcd-1"), expectedRestartPlan(adapter))
	worker := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-1",
			Namespace:   "fleet-default",
			UID:         "worker-uid",
			Annotations: map[string]string{},
			Labels: map[string]string{
				capr.ClusterNameLabel: "test",
				capr.WorkerRoleLabel:  "true",
			},
		},
		Type: planapi.SecretTypeMachinePlan,
	}
	h := &handler{
		secrets: newSecretClient(t, ctrl, etcd, worker),
	}
	h.store = planapi.NewStore(h.secrets)

	got, err := h.reconcileRestart(newScope(op, nil, adapter), opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	// Worker secret must be ignored — only etcd nodes receive the restart plan; success would
	// not be reached if the worker were included (its plan is not in "applied" state).
	assert.Equal(t, opv1alpha1.OperationPhaseSucceeded, got.Phase, "non-etcd secrets must not be in the iteration")
}

// --- interrupt-cleanup finalizer -------------------------------------------------------------

// newDeletingOp returns the canonical operation as a user has just deleted it: terminating,
// carrying the cleanup finalizer it wrote when it first interrupted the cluster, and pointing at
// the imported mgmt v3 cluster whose adapter ops.NewAdapter can build without a live wrangler
// context.
func newDeletingOp(deletedAt time.Time) *opv1alpha1.ETCDSnapshotSave {
	op := newOp()
	ts := metav1.NewTime(deletedAt)
	op.DeletionTimestamp = &ts
	op.Finalizers = []string{ops.InterruptCleanupFinalizer}
	clusterRef, _ := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	return op
}

func TestOnChange_DeletionRunsCleanupAndDropsTheFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Both annotations, not just paused. A cancellation that ran to completion leaves the canceled
	// annotation behind too, and the cleanup path must not gate on PausedCondition — which
	// handleCancel deliberately clears — to decide whether there is anything to clear.
	pausedSecret := newImportedPlanSecret("plan-a", planapi.PlanStatePaused)
	pausedSecret.Annotations[planapi.PlanPausedAnnotation] = "true"
	pausedSecret.Annotations[planapi.PlanCanceledAnnotation] = "true"

	_, cluster := newMgmtClusterRef()
	op := newDeletingOp(time.Now())

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, nil, pausedSecret)
	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)

	var finalFinalizers []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, error) {
			finalFinalizers = o.Finalizers
			return o, nil
		}).Times(1)

	h := &handler{
		etcdsnapshotsaves: opClient,
		beacons:           &fakeBeaconClient{},
		secrets:           secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			cluster.GroupVersionKind().String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Empty(t, finalFinalizers, "cleanup succeeded, so the finalizer must be dropped")

	if assert.Len(t, updates, 1, "the leftover annotations must be cleared from the machine-plan secret") {
		assert.NotContains(t, updates[0].Annotations, planapi.PlanPausedAnnotation,
			"a stranded paused annotation halts every plan on the node with no CR left to explain why")
		assert.NotContains(t, updates[0].Annotations, planapi.PlanCanceledAnnotation)
	}
}

func TestOnChange_DeletionForceRemovesTheFinalizerOnceTheBudgetIsSpent(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newDeletingOp(time.Now().Add(-2 * ops.InterruptCleanupBudget))

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)
	var finalFinalizers []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, error) {
			finalFinalizers = o.Finalizers
			return o, nil
		}).Times(1)

	h := &handler{
		etcdsnapshotsaves: opClient,
		beacons:           &fakeBeaconClient{},
		secrets:           newSecretClient(t, ctrl),
		// getErr is a permanent, non-NotFound failure: the clusterRef GVK no longer resolves.
		dynamic: &fakeDynamic{getErr: errors.New("no matches for kind")},
		store:   planapi.NewStore(newSecretClient(t, ctrl)),
	}

	logs := captureLogs(t)

	status, err := h.OnChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err,
		"an undeletable CR blocks namespace teardown and cluster deprovisioning; a stranded "+
			"annotation is recoverable with one kubectl command. Force-remove is the lesser failure.")
	assert.Empty(t, finalFinalizers)

	assert.Equal(t, opv1alpha1.ETCDSnapshotSaveStatus{}, status,
		"a condition written here can never be read: the Update that drops the finalizer makes the "+
			"framework's UpdateStatus 409, and the next reconcile finds no finalizer and never "+
			"retries. The log is the only honest channel, so nothing may be written to the status")

	assert.Contains(t, logs.String(), opv1alpha1.InterruptCleanupIncompleteReason,
		"the log line needs a stable token for alerting to key on")
	assert.Contains(t, logs.String(),
		"kubectl get secret -A --field-selector type=rke.cattle.io/machine-plan",
		"the cluster never resolved, so the operator gets a discovery command rather than a guessed namespace")
}

// TestOnChange_DeletionForceRemovalNamesTheSecretsNamespaceNotTheOperations covers the one line an
// operator copies and pastes at the moment Rancher has admitted it cannot fix things itself.
//
// The canonical UI-created operation lives in fleet-default and points at the cluster-scoped mgmt
// v3 Cluster (so ClusterRef.Namespace is empty), while ImportedAdapter.BeaconRef puts the
// machine-plan Secrets in a namespace named after the cluster. Deriving the command's namespace
// from the operation would send the operator somewhere with no machine-plan Secrets in it.
func TestOnChange_DeletionForceRemovalNamesTheSecretsNamespaceNotTheOperations(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newDeletingOp(time.Now().Add(-2 * ops.InterruptCleanupBudget))
	require.Equal(t, "fleet-default", op.Namespace)
	require.Empty(t, op.Spec.ClusterRef.Namespace, "the mgmt v3 Cluster is cluster-scoped")

	_, cluster := newMgmtClusterRef()

	// The cluster resolves, so the scope is known; the Secret List is what fails.
	secrets := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	secrets.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("etcdserver: request timed out")).AnyTimes()

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, error) { return o, nil }).Times(1)

	h := &handler{
		etcdsnapshotsaves: opClient,
		beacons:           &fakeBeaconClient{},
		secrets:           secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			cluster.GroupVersionKind().String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	logs := captureLogs(t)
	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)

	assert.Contains(t, logs.String(),
		"kubectl annotate secret -n c-m-test -l rke.cattle.io/cluster-name=c-m-test "+
			"plan.cattle.io/canceled- plan.cattle.io/paused-",
		"the command must select exactly the Secrets the cleanup collector reads")
	assert.NotContains(t, logs.String(), "-n fleet-default",
		"the operation's own namespace holds no machine-plan Secrets")
}

func TestOnChange_DeletionWithNoFinalizerIsANoOp(t *testing.T) {
	ctrl := gomock.NewController(t)
	ts := metav1.NewTime(time.Now())
	op := newOp()
	op.DeletionTimestamp = &ts

	h := &handler{secrets: newSecretClient(t, ctrl), store: planapi.NewStore(nil)}
	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err, "an operation that never wrote an annotation must never be delayed")
}

func TestOnChange_TheFirstInterruptPersistsTheFinalizerBeforeAnnotatingAnything(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Paused = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	var updates []*corev1.Secret
	secrets := newRecordingSecretClient(t, ctrl, &updates, nil,
		newImportedPlanSecret("plan-a", planapi.PlanStateInProgress))

	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)
	var written []string
	opClient.EXPECT().Update(gomock.Any()).DoAndReturn(
		func(o *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, error) {
			written = o.Finalizers
			return o, nil
		}).Times(1)

	h := &handler{
		etcdsnapshotsaves: opClient,
		beacons:           &fakeBeaconClient{getObj: newBeacon(testOwnerKey, true)},
		secrets:           secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind).String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	_, err := h.OnChange(op, opv1alpha1.ETCDSnapshotSaveStatus{})
	assert.NoError(t, err)
	assert.Equal(t, []string{ops.InterruptCleanupFinalizer}, written,
		"the finalizer has to be persisted before the first annotation, never after: the window "+
			"between the two is exactly when a delete strands one")
	assert.Empty(t, updates,
		"no machine-plan secret may be annotated on the reconcile that writes the finalizer")
}

func TestOnChange_TerminalOperationNeverAcquiresTheFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)

	// HandleInterrupt's first act is to return early for a terminal operation, so pausing one
	// writes no annotation anywhere. A finalizer here would guard nothing and cost an Update, a
	// reconcile and a Secret List at deletion time.
	op := newOp()
	op.Spec.Paused = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	status := opv1alpha1.ETCDSnapshotSaveStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseSucceeded)
	op.Status = status

	secrets := newSecretClient(t, ctrl, newImportedPlanSecret("plan-a", planapi.PlanStateSucceeded))
	opClient := ctrlfake.NewMockControllerInterface[*opv1alpha1.ETCDSnapshotSave, *opv1alpha1.ETCDSnapshotSaveList](ctrl)
	opClient.EXPECT().Update(gomock.Any()).Times(0)
	opClient.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	opClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	h := &handler{
		etcdsnapshotsaves: opClient,
		beacons:           &fakeBeaconClient{getObj: newBeacon("", false)},
		secrets:           secrets,
		dynamic: &fakeDynamic{gets: map[string]runtime.Object{
			cluster.GroupVersionKind().String() + "//c-m-test": cluster,
		}},
		store: planapi.NewStore(secrets),
	}

	_, err := h.OnChange(op, status)
	assert.NoError(t, err)
}

// TestRecreateAfterCancel_UnwedgesTheSecret is the regression test for the delete-and-recreate
// path. After a cancel the machine-plan Secret is left holding plan.cattle.io/canceled: "true",
// plan-state: canceled, and a cancellation report under plan-progress. A new operation against
// that cluster has exactly three possible outcomes, and two of them are silent:
//
//	writes new plan content only         -> readInterrupt returns Canceled, handleCancellation sees
//	                                        a terminal state and writes nothing. The plan is never read.
//	clears the annotation only           -> resolveResume returns canceled, decidePlanStateAction
//	                                        returns NeedsApplied: false, monitoring-only. Never runs.
//	clears the annotation AND writes     -> NeedsApplied: true, ResetPlanAttempt: true. The agent
//	plan-state: pending                     commits in-progress, bumps plan-revision, applies from
//	                                        instruction 0. This is the only correct outcome.
//
// Both halves are required and neither is sufficient alone.
func TestRecreateAfterCancel_UnwedgesTheSecret(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)

	// Byte-identity with what the new operation generates is the whole point of this test, so the
	// fixture is derived from the plan rather than hand-written. AssignPlan compares the stored
	// bytes against json.Marshal of the incoming Plan, and every field of Plan is omitempty — a
	// literal such as {"instructions":[]} round-trips to {}, which would take the planChanged
	// path and quietly stop exercising the case under test.
	identical := planapi.Plan{OneTimeInstructions: []planapi.OneTimeInstruction{{
		CommonInstruction: planapi.CommonInstruction{Name: "etcd-snapshot", Command: "/bin/true"},
	}}}
	existingPlan, err := json.Marshal(&identical)
	require.NoError(t, err)

	// A Secret in exactly the state a previous, canceled operation leaves behind. The plan content
	// is byte-identical to what the new operation will generate, which is the case the old
	// checksum-gated write path silently skipped.
	secret := newPlanSecret("plan-a")
	secret.Annotations[planapi.PlanCanceledAnnotation] = "true"
	secret.Data = map[string][]byte{
		"plan":                    existingPlan,
		planapi.PlanStateKey:      []byte(planapi.PlanStateCanceled),
		planapi.PlanCheckpointKey: []byte(`{"checksum":"` + planapi.Checksum(existingPlan) + `","completedInstructions":2}`),
	}

	var updates []*corev1.Secret
	client := newRecordingSecretClient(t, ctrl, &updates, nil, secret)

	store := planapi.NewStore(client)
	got, err := store.AssignPlan(secret, &identical, 1, -1)
	assert.NoError(t, err)

	require.Len(t, updates, 1, "byte-identical content over a canceled plan must still be written, exactly once")
	written := updates[0]

	assert.NotContains(t, written.Annotations, planapi.PlanCanceledAnnotation,
		"outcome 1: leaving the annotation makes handleCancellation short-circuit and the plan is never read")
	assert.Equal(t, string(planapi.PlanStatePending), string(written.Data[planapi.PlanStateKey]),
		"outcome 2: clearing the annotation without writing pending leaves the agent monitoring-only forever")

	progress, ok := written.Data[planapi.PlanCheckpointKey]
	assert.True(t, ok, "plan-progress must be cleared by writing an empty value, not by deleting the key")
	assert.Empty(t, progress,
		"a stale cancellation checkpoint would feed a bogus completedInstructions into recovery "+
			"detection and a bogus terminationIncomplete into the next interrupt write")

	assert.Equal(t, existingPlan, written.Data["plan"],
		"the recreated operation delivers byte-identical content; it must not be rewritten")

	// Outcome 3 is the conjunction, and it is what the controller actually branches on: the
	// snapshot handlers treat !Waiting() as "this node is done" and Failure() as "give up".
	assert.True(t, got.Pending, "the new operation must report Pending, not InProgress")
	assert.True(t, got.Waiting(), "the recreated operation has outstanding work on every node")
	assert.False(t, got.Success(), "nothing has re-run yet, so the operation cannot be complete")
	assert.False(t, got.Failure(), "the canceled run's evidence must not fail the recreated operation")
}

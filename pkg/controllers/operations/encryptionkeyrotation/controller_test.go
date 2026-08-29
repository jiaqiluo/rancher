package encryptionkeyrotation

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	rkeplan "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1/plan"
	"github.com/rancher/rancher/pkg/capr"
	operationcontrollers "github.com/rancher/rancher/pkg/generated/controllers/operation.cattle.io/v1alpha1"
	ops "github.com/rancher/rancher/pkg/operations"
	"github.com/rancher/rancher/pkg/plan"
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

type stubAdapter struct {
	waitForRegisterOK  bool
	waitForRegisterErr error
	pauseCalls         []bool
}

func (a *stubAdapter) BeaconRef() (string, string) { return "test-namespace", "test-cluster" }

func (a *stubAdapter) EtcdSnapshotNamespace() string { return "test-namespace" }

func (a *stubAdapter) ClusterObject() (*unstructured.Unstructured, error) {
	return &unstructured.Unstructured{}, nil
}

func (a *stubAdapter) WaitForRegister() (bool, error) {
	return a.waitForRegisterOK, a.waitForRegisterErr
}

func (a *stubAdapter) RuntimeCommand() string {
	return "rke2"
}

func (a *stubAdapter) DistroDataDirectory(_ *corev1.Secret) string {
	return "/var/lib/rancher/rke2"
}

func (a *stubAdapter) ProvisioningDataDirectory(_ *corev1.Secret) string {
	return "/var/lib/rancher/capr"
}
func (a *stubAdapter) ServerUnit() string {
	return "rke2-server"
}

func (a *stubAdapter) RenderProbes(_ *corev1.Secret, _ bool) (map[string]rkeplan.Probe, error) {
	return map[string]rkeplan.Probe{}, nil
}

func (a *stubAdapter) KubectlPath(_ *corev1.Secret) string {
	return "/var/lib/rancher/rke2/bin/kubectl"
}

func (a *stubAdapter) KubeconfigPath(_ *corev1.Secret) string {
	return "/etc/rancher/rke2/rke2.yaml"
}

func (a *stubAdapter) FindOrElectLeader(_ string, _ ops.Filter) (*corev1.Secret, error) {
	return nil, nil
}

func (a *stubAdapter) PauseCluster(paused bool) error {
	a.pauseCalls = append(a.pauseCalls, paused)
	return nil
}

// The six methods below complete the ops.Adapter contract for the stub. None of them are
// exercised by the encryption-key-rotation controller (which only consumes runtime/dataDir/
// serverUnit/probes/pause/plans), so each returns a static RKE2-shaped value matching what
// CAPRAdapter would produce for an rke2 cluster.
func (a *stubAdapter) ConfigFile(_ *corev1.Secret) string {
	return "/etc/rancher/rke2/config.yaml"
}
func (a *stubAdapter) ConfigDirectory(_ *corev1.Secret) string {
	return "/etc/rancher/rke2/config.yaml.d"
}
func (a *stubAdapter) GetServerURL(_ *corev1.Secret) string      { return "" }
func (a *stubAdapter) GetSupervisorPort(_ *corev1.Secret) string { return "9345" }
func (a *stubAdapter) LoopbackAddress(_ *corev1.Secret) string   { return "127.0.0.1" }
func (a *stubAdapter) ToS3ArgsEnvAndFiles(_ *corev1.Secret) ([]string, []string, []plan.File) {
	return nil, nil, nil
}

type enqueueCall struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

type fakeDynamic struct {
	getObj       runtime.Object
	getErr       error
	enqueueErr   error
	enqueueCalls []enqueueCall
}

func (d *fakeDynamic) Get(_ schema.GroupVersionKind, _, _ string) (runtime.Object, error) {
	if d.getErr != nil {
		return nil, d.getErr
	}
	return d.getObj, nil
}

func (d *fakeDynamic) Enqueue(gvk schema.GroupVersionKind, namespace, name string) error {
	d.enqueueCalls = append(d.enqueueCalls, enqueueCall{
		gvk:       gvk,
		namespace: namespace,
		name:      name,
	})
	return d.enqueueErr
}

func newOp() *opv1alpha1.EncryptionKeyRotation {
	return &opv1alpha1.EncryptionKeyRotation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ekr-1",
			Namespace: "fleet-default",
			UID:       types.UID("ekr-uid"),
		},
	}
}

func newBeacon(owner string, active bool) *planv1alpha1.Beacon {
	// Beacon ownership lives on Status.Owner; we keep the legacy BeaconOwnerLabel populated so
	// reclaimStaleBeaconOwnerIfNeeded (which still reads the label) sees a consistent owner.
	return &planv1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fleet-default",
			Namespace: "fleet-default",
		},
		Status: planv1alpha1.BeaconStatus{
			Active: active,
			Owner:  owner,
		},
	}
}

func newScope(op *opv1alpha1.EncryptionKeyRotation, beacon *planv1alpha1.Beacon, adapter ops.Adapter) *scope {
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion("provisioning.cattle.io/v1")
	cluster.SetKind("Cluster")
	cluster.SetNamespace("fleet-default")
	cluster.SetName("test")
	return &scope{
		op:         op,
		beacon:     beacon,
		namespace:  "fleet-default",
		clusterObj: cluster,
		adapter:    adapter,
	}
}

type fakeEncryptionKeyRotationController struct {
	operationcontrollers.EncryptionKeyRotationController
	getFn func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error)
}

func (f *fakeEncryptionKeyRotationController) Get(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error) {
	if f.getFn == nil {
		return nil, nil
	}
	return f.getFn(namespace, name, opts)
}

type fakeBeaconClient struct {
	plancontrollers.BeaconClient
	getObj          *planv1alpha1.Beacon
	getErr          error
	updateCalls     int
	updates         []*planv1alpha1.Beacon
	statusUpdates   []*planv1alpha1.Beacon
	updateErr       error
	updateStatusErr error
}

// Get serves the beacon lookup onChange performs after resolving the cluster adapter. An unset
// getObj/getErr yields NotFound, matching a cluster whose beacon has not been created yet.
func (f *fakeBeaconClient) Get(_, name string, _ metav1.GetOptions) (*planv1alpha1.Beacon, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getObj != nil {
		return f.getObj, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "beacons"}, name)
}

func (f *fakeBeaconClient) Update(beacon *planv1alpha1.Beacon) (*planv1alpha1.Beacon, error) {
	f.updateCalls++
	f.updates = append(f.updates, beacon.DeepCopy())
	return beacon, f.updateErr
}

func (f *fakeBeaconClient) UpdateStatus(beacon *planv1alpha1.Beacon) (*planv1alpha1.Beacon, error) {
	f.statusUpdates = append(f.statusUpdates, beacon.DeepCopy())
	return beacon, f.updateStatusErr
}

func newPeriodicStatusSecret(secretName, stdout string) *corev1.Secret {
	periodicOutput := map[string]plan.PeriodicInstructionOutput{
		statusPeriodicName: {
			Name:   statusPeriodicName,
			Stdout: []byte(stdout),
		},
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: "fleet-default",
		},
		Data: map[string][]byte{
			"applied-periodic-output": mustGzipJSON(periodicOutput),
		},
	}
}

func mustGzipJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(raw); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}

	return buffer.Bytes()
}

// newImportedPlanSecret builds a machine-plan secret in the namespace ImportedAdapter resolves for
// newMgmtClusterRef's cluster, carrying the agent-authored plan state the interrupt machinery
// reads.
func newImportedPlanSecret(name string, state plan.PlanState) *corev1.Secret {
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
		Type: plan.SecretTypeMachinePlan,
		Data: map[string][]byte{
			"plan":            []byte(`{"instructions":[]}`),
			plan.PlanStateKey: []byte(state),
		},
	}
}

// newSecretClient mocks the SecretClient the interrupt gate lists and writes through. Update
// echoes the passed-in secret back and records it so a test can assert on what was written; a
// non-nil updateErr stands in for a write that fails against the API server, and attempts are
// recorded whether they succeed or not.
func newSecretClient(t *testing.T, ctrl *gomock.Controller, updates *[]*corev1.Secret,
	updateErr error, items ...*corev1.Secret) *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList] {
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

func TestConvergenceWaitMessage(t *testing.T) {
	secretName := "leader-node"
	tests := []struct {
		name             string
		stdout           string
		requireHashMatch bool
		wantWait         bool
		wantErr          bool
	}{
		{
			name:             "returns wait message for non-final stage",
			stdout:           "Current Rotation Stage: start\nServer Encryption Hashes: All hashes match",
			requireHashMatch: false,
			wantWait:         true,
		},
		{
			name:             "returns success at reencrypt finished without hash requirement",
			stdout:           "Current Rotation Stage: reencrypt_finished",
			requireHashMatch: false,
		},
		{
			name:             "returns success at reencrypt finished with hash match",
			stdout:           "Current Rotation Stage: reencrypt_finished\nServer Encryption Hashes: All hashes match",
			requireHashMatch: true,
		},
		{
			name:             "returns wait at reencrypt finished while hashes differ",
			stdout:           "Current Rotation Stage: reencrypt_finished\nServer Encryption Hashes: hash mismatch",
			requireHashMatch: true,
			wantWait:         true,
		},
		{
			name:             "returns error for malformed status output",
			stdout:           "Server Encryption Hashes: All hashes match",
			requireHashMatch: false,
			wantErr:          true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := newPeriodicStatusSecret(secretName, tt.stdout)
			waitMsg, err := convergenceWaitMessage(secret, tt.requireHashMatch)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantWait && waitMsg == "" {
				t.Fatalf("expected wait message, got empty")
			}
			if !tt.wantWait && waitMsg != "" {
				t.Fatalf("expected no wait message, got: %s", waitMsg)
			}
		})
	}
}

func TestReadRotateKeysResult(t *testing.T) {
	tests := []struct {
		name          string
		appliedOutput map[string][]byte
		wantExitCode  int
		wantOutput    string
		wantNotYet    bool
		wantErr       bool
	}{
		{
			name: "parses valid exit code line",
			appliedOutput: map[string][]byte{
				rotateKeysInstructionName: []byte("rotate output\n" + exitCodePrefix + "7\n"),
			},
			wantExitCode: 7,
			wantOutput:   "rotate output\n" + exitCodePrefix + "7\n",
		},
		{
			name:          "returns not yet when key missing",
			appliedOutput: map[string][]byte{},
			wantNotYet:    true,
			wantErr:       true,
		},
		{
			name: "returns not yet when exit code line missing",
			appliedOutput: map[string][]byte{
				rotateKeysInstructionName: []byte("rotate output without code"),
			},
			wantNotYet: true,
			wantErr:    true,
		},
		{
			name: "returns parse error for corrupt exit code",
			appliedOutput: map[string][]byte{
				rotateKeysInstructionName: []byte("rotate output\n" + exitCodePrefix + "NaN\n"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := readRotateKeysResult(tt.appliedOutput)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantNotYet && !errors.Is(err, errRotateKeysOutputNotYet) {
					t.Fatalf("expected errRotateKeysOutputNotYet, got %v", err)
				}
				if !tt.wantNotYet && errors.Is(err, errRotateKeysOutputNotYet) {
					t.Fatalf("expected non-sentinel error, got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.exitCode != tt.wantExitCode {
				t.Fatalf("expected exit code %d, got %d", tt.wantExitCode, result.exitCode)
			}
			if result.output != tt.wantOutput {
				t.Fatalf("expected output %q, got %q", tt.wantOutput, result.output)
			}
		})
	}
}

func TestStatusFromOutput(t *testing.T) {
	tests := []struct {
		name              string
		output            string
		wantStage         string
		wantHashesMatch   bool
		wantHashesPresent bool
		wantTimeoutErr    bool
		wantErr           bool
	}{
		{
			name:      "parses start stage",
			output:    "Current Rotation Stage: start",
			wantStage: "start",
		},
		{
			name:              "parses reencrypt finished with matching hashes",
			output:            "Current Rotation Stage: reencrypt_finished\nServer Encryption Hashes: All hashes match",
			wantStage:         "reencrypt_finished",
			wantHashesMatch:   true,
			wantHashesPresent: true,
		},
		{
			name:              "parses reencrypt finished with non-matching hashes",
			output:            "Current Rotation Stage: reencrypt_finished\nServer Encryption Hashes: hash does not match",
			wantStage:         "reencrypt_finished",
			wantHashesPresent: true,
		},
		{
			name:           "returns timeout error for known timeout output",
			output:         "see server log for details: Get https://127.0.0.1:9345/encrypt/status: context deadline exceeded",
			wantTimeoutErr: true,
			wantErr:        true,
		},
		{
			name:    "returns error when stage line is missing",
			output:  "Server Encryption Hashes: All hashes match",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := statusFromOutput(tt.output)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantTimeoutErr && !errors.Is(err, errStatusTimeout) {
					t.Fatalf("expected errStatusTimeout, got %v", err)
				}
				if !tt.wantTimeoutErr && errors.Is(err, errStatusTimeout) {
					t.Fatalf("expected non-timeout error, got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status.stage != tt.wantStage {
				t.Fatalf("expected stage %q, got %q", tt.wantStage, status.stage)
			}
			if status.hashesMatch != tt.wantHashesMatch {
				t.Fatalf("expected hashesMatch %t, got %t", tt.wantHashesMatch, status.hashesMatch)
			}
			if status.hashesPresent != tt.wantHashesPresent {
				t.Fatalf("expected hashesPresent %t, got %t", tt.wantHashesPresent, status.hashesPresent)
			}
		})
	}
}

func TestUpdateStatus_CanceledStopsReportingInProgress(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Cancel = true

	// Exactly what reaches updateStatus after HandleInterrupt finishes a cancellation: the
	// conditions the InProgress phase left behind, plus the Canceled the gate just wrote.
	status := opv1alpha1.EncryptionKeyRotationStatus{}
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
	tests := []struct {
		name  string
		phase opv1alpha1.OperationPhase
		check func(t *testing.T, status opv1alpha1.EncryptionKeyRotationStatus)
	}{
		{
			name:  "pending sets Pending=true",
			phase: opv1alpha1.OperationPhasePending,
			check: func(t *testing.T, s opv1alpha1.EncryptionKeyRotationStatus) {
				if string(opv1alpha1.PendingCondition.GetStatus(&s)) != "True" {
					t.Fatalf("expected PendingCondition=True")
				}
			},
		},
		{
			name:  "in-progress clears pending with in-progress reason",
			phase: opv1alpha1.OperationPhaseInProgress,
			check: func(t *testing.T, s opv1alpha1.EncryptionKeyRotationStatus) {
				if string(opv1alpha1.PendingCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected PendingCondition=False")
				}
				if opv1alpha1.PendingCondition.GetReason(&s) != opv1alpha1.InProgressReason {
					t.Fatalf("expected PendingCondition reason %q, got %q", opv1alpha1.InProgressReason, opv1alpha1.PendingCondition.GetReason(&s))
				}
			},
		},
		{
			name:  "succeeded clears pending in-progress and failed",
			phase: opv1alpha1.OperationPhaseSucceeded,
			check: func(t *testing.T, s opv1alpha1.EncryptionKeyRotationStatus) {
				if string(opv1alpha1.PendingCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected PendingCondition=False")
				}
				if string(opv1alpha1.InProgressCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected InProgressCondition=False")
				}
				if string(opv1alpha1.FailedCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected FailedCondition=False")
				}
				if opv1alpha1.FailedCondition.GetReason(&s) != opv1alpha1.NotFailedReason {
					t.Fatalf("expected FailedCondition reason %q, got %q", opv1alpha1.NotFailedReason, opv1alpha1.FailedCondition.GetReason(&s))
				}
			},
		},
		{
			name:  "failed clears pending in-progress and succeeded",
			phase: opv1alpha1.OperationPhaseFailed,
			check: func(t *testing.T, s opv1alpha1.EncryptionKeyRotationStatus) {
				if string(opv1alpha1.PendingCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected PendingCondition=False")
				}
				if string(opv1alpha1.InProgressCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected InProgressCondition=False")
				}
				if string(opv1alpha1.SucceededCondition.GetStatus(&s)) != "False" {
					t.Fatalf("expected SucceededCondition=False")
				}
				if opv1alpha1.SucceededCondition.GetReason(&s) != opv1alpha1.NotSuccessfulReason {
					t.Fatalf("expected SucceededCondition reason %q, got %q", opv1alpha1.NotSuccessfulReason, opv1alpha1.SucceededCondition.GetReason(&s))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := newOp()
			op.Generation = 42

			status := updateStatus(op, opv1alpha1.EncryptionKeyRotationStatus{
				OperationStatus: opv1alpha1.OperationStatus{Phase: tt.phase},
			})
			if status.ObservedGeneration != 42 {
				t.Fatalf("expected ObservedGeneration=42, got %d", status.ObservedGeneration)
			}
			tt.check(t, status)
		})
	}
}

// newMgmtClusterRef builds a ClusterRef/cluster pair for a plain imported mgmt v3 Cluster. This
// GVK is the only one ops.NewAdapter can construct without a live wrangler context, allowing
// onChange to be exercised end-to-end in a unit test.
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

func TestCancelPolicyRegistered(t *testing.T) {
	t.Parallel()

	policy := ops.CancelPolicyFor(operationGVK)
	assert.True(t, policy.RequiresRecovery,
		"a rotation stopped partway can leave the cluster in a mixed key state")
	assert.NotEmpty(t, policy.RecoveryMessage)
}

func TestOnChange_PausedStillReportsPausedCondition(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Paused = true

	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, nil, newImportedPlanSecret("plan-a", plan.PlanStateInProgress))

	beacons := &fakeBeaconClient{getObj: newBeacon("", false)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	status, err := h.onChange(op, opv1alpha1.EncryptionKeyRotationStatus{})
	assert.NoError(t, err)
	status = updateStatus(op, status)

	assert.True(t, opv1alpha1.PausedCondition.IsTrue(&status),
		"a paused operation must still report PausedCondition so the user can see the pause took effect")
	assert.Equal(t, opv1alpha1.PauseRequestedReason, opv1alpha1.PausedCondition.GetReason(&status),
		"the node has not reported a paused plan state yet, so the pause is requested, not in effect")
	assert.Equal(t, opv1alpha1.OperationPhasePending, status.Phase,
		"the interrupt handling must run after the empty-phase defaulting, not before it")
	assert.Empty(t, beacons.statusUpdates,
		"the interrupt handling must run before the phase dispatch, so no phase handler may touch the beacon")
	if assert.Len(t, updates, 1, "the pause must be propagated to the machine-plan secret") {
		assert.Equal(t, "true", updates[0].Annotations[plan.PlanPausedAnnotation])
	}
}

// newPlanlessImportedSecret builds a machine-plan Secret that has never been assigned a plan,
// matching how pkg/controllers/capr/unmanaged creates one: with no Data at all. Any cluster with a
// node that the operation's steps do not target can have one, such as a worker under a control-plane-
// scoped step or a node that registered mid-flight.
func newPlanlessImportedSecret(name string) *corev1.Secret {
	secret := newImportedPlanSecret(name, "")
	secret.Data = nil
	return secret
}

// TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan pins the read-side behavior of the
// unfiltered Secrets contract. The write side remains intentionally cluster-wide: different steps
// target different subsets of nodes, so an interrupt must reach every Secret. But a node that the
// operation never assigned a plan has no plan-state and never will.
//
// Under this operation type's cancel policy, counting that node on the read side has two costs: it
// makes cancellation wait the full CancelConfirmationTimeout while holding the beacon, then
// reports RecoveryRequired even though no key was ever rotated on that node.
func TestOnChange_CancelIsNotHeldUpByASecretThatNeverGotAPlan(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Cancel = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	confirmed := newImportedPlanSecret("cp-a", plan.PlanStateCanceled)
	planless := newPlanlessImportedSecret("worker-b")

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, nil, confirmed, planless)

	beacons := &fakeBeaconClient{getObj: newBeacon(beaconOwnerKey(op), true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	status := opv1alpha1.EncryptionKeyRotationStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseInProgress)

	got, err := h.onChange(op, status)
	assert.NoError(t, err)

	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, got.Phase,
		"a Secret with no plan has nothing to stop; waiting for it to confirm holds the beacon "+
			"for the full CancelConfirmationTimeout on every cluster with a worker node")
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got),
		"no plan was ever assigned here, which is not the legacy checksum flow and not a slow agent")

	assert.False(t, got.AnyNodeMutationObserved,
		"nothing executed: one node confirmed the cancellation and the other never had a plan")
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(&got),
		"a worker node with no plan is not evidence that a key rotation ran halfway")

	assert.Len(t, updates, 2,
		"the write side stays unfiltered: every machine-plan Secret is annotated, including the "+
			"plan-less one, because a later step may yet target it")
}

func TestUpdateStatus_DoesNotClobberTheInterruptPausedReason(t *testing.T) {
	t.Parallel()

	op := newOp()
	op.Spec.Paused = true

	status := opv1alpha1.EncryptionKeyRotationStatus{}
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

	status := opv1alpha1.EncryptionKeyRotationStatus{}
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
		seed   func(*opv1alpha1.EncryptionKeyRotationStatus)
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
			seed: func(s *opv1alpha1.EncryptionKeyRotationStatus) {
				opv1alpha1.PausedCondition.True(s)
				opv1alpha1.PausedCondition.Reason(s, opv1alpha1.ResumeFailedReason)
			},
			want: opv1alpha1.ResumeFailedReason,
		},
		{
			name:   "a pause in effect survives",
			paused: true,
			seed: func(s *opv1alpha1.EncryptionKeyRotationStatus) {
				opv1alpha1.PausedCondition.True(s)
				opv1alpha1.PausedCondition.Reason(s, opv1alpha1.PausedReason)
			},
			want: opv1alpha1.PausedReason,
		},
		{
			name: "a completed resume's clear survives",
			seed: func(s *opv1alpha1.EncryptionKeyRotationStatus) {
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

			status := opv1alpha1.EncryptionKeyRotationStatus{}
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

// runStatusHandler sends op through the generated status handler. This is the same path that the
// wrangler-registered controller uses in production.
//
// The helper returns the status that the handler persists. It returns nil when the handler writes
// no status.
//
// See the equivalent helper in etcdsnapshotsave tests. A handwritten stand-in does not test the
// same code path.
func runStatusHandler(t *testing.T, ctrl *gomock.Controller, h *handler, op *opv1alpha1.EncryptionKeyRotation) *opv1alpha1.EncryptionKeyRotationStatus {
	t.Helper()

	var sync generic.Handler
	rotations := ctrlfake.NewMockControllerInterface[*opv1alpha1.EncryptionKeyRotation, *opv1alpha1.EncryptionKeyRotationList](ctrl)
	rotations.EXPECT().AddGenericHandler(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, _ string, handler generic.Handler) { sync = handler })
	rotations.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	var persisted *opv1alpha1.EncryptionKeyRotationStatus
	rotations.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(o *opv1alpha1.EncryptionKeyRotation) (*opv1alpha1.EncryptionKeyRotation, error) {
		persisted = o.Status.DeepCopy()
		return o, nil
	}).AnyTimes()

	h.encryptionkeyrotations = rotations
	operationcontrollers.RegisterEncryptionKeyRotationStatusHandler(context.Background(), rotations, "", "test", h.OnChange)
	require.NotNil(t, sync, "RegisterEncryptionKeyRotationStatusHandler must have installed a handler")

	_, err := sync(op.Namespace+"/"+op.Name, op)
	assert.NoError(t, err, "the interrupt gate must not propagate its error; the framework would revert the status")

	return persisted
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

	secret := newImportedPlanSecret("a", plan.PlanStateInProgress)
	secret.Annotations[plan.PlanPausedAnnotation] = "true"

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, errors.New("etcdserver: request timed out"), secret)
	beacons := &fakeBeaconClient{getObj: newBeacon(beaconOwnerKey(op), true)}
	h := &handler{
		beacons: beacons,
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	persisted := runStatusHandler(t, ctrl, h, op)

	require.NotNil(t, persisted, "the failed resume must write a status")
	assert.True(t, opv1alpha1.PausedCondition.IsTrue(persisted),
		"PausedCondition is the only user-visible evidence that the resume was dropped, and it is "+
			"also handleResume's record that there is still an interrupt to lift; updateStatus "+
			"must not erase it out from under HandleInterrupt")
	assert.Equal(t, opv1alpha1.ResumeFailedReason, opv1alpha1.PausedCondition.GetReason(persisted))
	assert.Equal(t, "true", secret.Annotations[plan.PlanPausedAnnotation],
		"the agents really are still halted, which is what the condition has to keep saying")
	assert.Len(t, updates, 1, "the resume attempted exactly one write, and it failed")

	next := op.DeepCopy()
	next.Status = *persisted

	assert.Nil(t, runStatusHandler(t, ctrl, h, next),
		"a retry that reports the same thing must leave the status byte-identical")
	assert.Len(t, updates, 2,
		"handleResume must have run again; one attempt total would mean its PausedCondition gate "+
			"read False and the operation fell through to the phase dispatch")
	assert.Empty(t, beacons.statusUpdates, "no phase handler may touch the beacon")
}

func TestHandleFailed_HoldingBeaconReleasesAndUnpauses(t *testing.T) {
	op := newOp()
	adapter := &stubAdapter{waitForRegisterOK: true}
	beacons := &fakeBeaconClient{}

	h := &handler{beacons: beacons}
	s := newScope(op, newBeacon(beaconOwnerKey(op), true), adapter)

	_, err := h.handleFailed(s, opv1alpha1.EncryptionKeyRotationStatus{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(adapter.pauseCalls) != 1 || adapter.pauseCalls[0] {
		t.Fatalf("expected PauseCluster(false), got %+v", adapter.pauseCalls)
	}
	// ReleaseBeacon writes to UpdateStatus (Status.Owner is the source of truth for beacon
	// ownership); the legacy main-resource Update is no longer used.
	if len(beacons.statusUpdates) != 1 {
		t.Fatalf("expected one beacon status update (ReleaseBeacon), got %d", len(beacons.statusUpdates))
	}
	if beacons.statusUpdates[0].Status.Owner != "" {
		t.Fatalf("expected Status.Owner to be cleared on release, got %q", beacons.statusUpdates[0].Status.Owner)
	}
	if len(beacons.updates) != 0 {
		t.Fatalf("expected no main-resource updates from ReleaseBeacon, got %d", len(beacons.updates))
	}
}

func TestHandleSucceeded_HoldingBeaconTogglesReleasesAndEnqueues(t *testing.T) {
	op := newOp()
	adapter := &stubAdapter{waitForRegisterOK: true}
	beacons := &fakeBeaconClient{}
	dynamic := &fakeDynamic{}

	h := &handler{
		beacons: beacons,
		dynamic: dynamic,
	}
	s := newScope(op, newBeacon(beaconOwnerKey(op), true), adapter)

	_, err := h.handleSucceeded(s, opv1alpha1.EncryptionKeyRotationStatus{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(adapter.pauseCalls) != 1 || adapter.pauseCalls[0] {
		t.Fatalf("expected PauseCluster(false), got %+v", adapter.pauseCalls)
	}
	// ReleaseBeacon on the owner path clears Active + Owner + Delegates in a single
	// UpdateStatus call — no separate ToggleBeacon is needed.
	if len(beacons.statusUpdates) != 1 {
		t.Fatalf("expected one beacon status update (release), got %d", len(beacons.statusUpdates))
	}
	if beacons.statusUpdates[0].Status.Active {
		t.Fatalf("expected beacon to be toggled inactive on release")
	}
	if beacons.statusUpdates[0].Status.Owner != "" {
		t.Fatalf("expected Status.Owner to be cleared on release, got %q", beacons.statusUpdates[0].Status.Owner)
	}
	if len(beacons.updates) != 0 {
		t.Fatalf("expected no main-resource updates from ReleaseBeacon, got %d", len(beacons.updates))
	}

	if len(dynamic.enqueueCalls) != 1 {
		t.Fatalf("expected one cluster enqueue, got %d", len(dynamic.enqueueCalls))
	}
	expectedGVK := schema.FromAPIVersionAndKind("provisioning.cattle.io/v1", "Cluster")
	if dynamic.enqueueCalls[0].gvk != expectedGVK || dynamic.enqueueCalls[0].namespace != "fleet-default" || dynamic.enqueueCalls[0].name != "test" {
		t.Fatalf("unexpected enqueue call: %#v", dynamic.enqueueCalls[0])
	}
}

func TestHandleSucceeded_NotHoldingOnlyUnpauses(t *testing.T) {
	op := newOp()
	adapter := &stubAdapter{waitForRegisterOK: true}
	beacons := &fakeBeaconClient{}
	dynamic := &fakeDynamic{}

	h := &handler{
		beacons: beacons,
		dynamic: dynamic,
	}
	s := newScope(op, newBeacon("other-controller", true), adapter)

	_, err := h.handleSucceeded(s, opv1alpha1.EncryptionKeyRotationStatus{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(adapter.pauseCalls) != 1 || adapter.pauseCalls[0] {
		t.Fatalf("expected PauseCluster(false), got %+v", adapter.pauseCalls)
	}
	if len(beacons.statusUpdates) != 0 {
		t.Fatalf("expected no beacon status updates, got %d", len(beacons.statusUpdates))
	}
	if len(beacons.updates) != 0 {
		t.Fatalf("expected no beacon release updates, got %d", len(beacons.updates))
	}
	if len(dynamic.enqueueCalls) != 0 {
		t.Fatalf("expected no enqueue calls, got %d", len(dynamic.enqueueCalls))
	}
}

func TestHandleInProgress_BeaconLost(t *testing.T) {
	op := newOp()
	op.Status.Step = opv1alpha1.EncryptionKeyRotationStepRotate

	h := &handler{beacons: &fakeBeaconClient{}}
	s := newScope(op, newBeacon("other-controller", true), &stubAdapter{waitForRegisterOK: true})

	got, err := h.handleInProgress(s, opv1alpha1.EncryptionKeyRotationStatus{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Phase != opv1alpha1.OperationPhaseFailed {
		t.Fatalf("expected phase failed, got %q", got.Phase)
	}
	if opv1alpha1.FailedCondition.GetReason(&got) != opv1alpha1.BeaconLostReason {
		t.Fatalf("expected failed reason %q, got %q", opv1alpha1.BeaconLostReason, opv1alpha1.FailedCondition.GetReason(&got))
	}
}

func TestHandleInProgress_UnknownStep(t *testing.T) {
	op := newOp()
	op.Status.Step = "mystery-step"
	beacons := &fakeBeaconClient{}
	h := &handler{beacons: beacons}

	s := newScope(op, newBeacon(beaconOwnerKey(op), false), nil)
	got, err := h.handleInProgress(s, opv1alpha1.EncryptionKeyRotationStatus{Step: "mystery-step"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Phase != opv1alpha1.OperationPhaseFailed {
		t.Fatalf("expected phase failed, got %q", got.Phase)
	}
	if opv1alpha1.FailedCondition.GetReason(&got) != opv1alpha1.UnknownStepReason {
		t.Fatalf("expected failed reason %q, got %q", opv1alpha1.UnknownStepReason, opv1alpha1.FailedCondition.GetReason(&got))
	}
	if len(beacons.statusUpdates) != 1 {
		t.Fatalf("expected one beacon status update before step handling, got %d", len(beacons.statusUpdates))
	}
	if !beacons.statusUpdates[0].Status.Active {
		t.Fatalf("expected beacon to be toggled active while operation is in progress")
	}
}

func TestReclaimStaleBeaconOwnerIfNeeded(t *testing.T) {
	currentOp := &opv1alpha1.EncryptionKeyRotation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fleet-default",
			Name:      "ekr-current",
			UID:       types.UID("current-uid"),
		},
	}

	tests := []struct {
		name             string
		beacon           *planv1alpha1.Beacon
		getFn            func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error)
		wantUpdate       bool
		wantErr          bool
		wantOwnerCleared bool
		wantRefCleared   bool
	}{
		{
			name: "no owner label does not reclaim",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Labels:    map[string]string{},
				},
			},
		},
		{
			name: "current op owner key does not reclaim",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: beaconOwnerKey(currentOp),
				},
			},
		},
		{
			name: "non matching owner does not reclaim",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "etcd-snapshot-save",
				},
			},
		},
		{
			name: "malformed owner ref reclaims beacon",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Annotations: map[string]string{
						beaconOwnerRefAnnotation: "bad-owner-ref",
					},
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "encryption-key-rotation-old-owner",
				},
			},
			wantUpdate:       true,
			wantOwnerCleared: true,
			wantRefCleared:   true,
		},
		{
			name: "missing owner object reclaims beacon",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Annotations: map[string]string{
						beaconOwnerRefAnnotation: "fleet-default/ekr-old/old-uid",
					},
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "encryption-key-rotation-old-owner",
				},
			},
			getFn: func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error) {
				return nil, apierrors.NewNotFound(schema.GroupResource{Group: "operation.cattle.io", Resource: "encryptionkeyrotations"}, name)
			},
			wantUpdate:       true,
			wantOwnerCleared: true,
			wantRefCleared:   true,
		},
		{
			name: "uid mismatch reclaims beacon",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Annotations: map[string]string{
						beaconOwnerRefAnnotation: "fleet-default/ekr-old/old-uid",
					},
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "encryption-key-rotation-old-owner",
				},
			},
			getFn: func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error) {
				return &opv1alpha1.EncryptionKeyRotation{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      name,
						UID:       "different-uid",
					},
					Status: opv1alpha1.EncryptionKeyRotationStatus{
						OperationStatus: opv1alpha1.OperationStatus{Phase: opv1alpha1.OperationPhaseInProgress},
					},
				}, nil
			},
			wantUpdate:       true,
			wantOwnerCleared: true,
			wantRefCleared:   true,
		},
		{
			name: "terminal owner reclaims beacon",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Annotations: map[string]string{
						beaconOwnerRefAnnotation: "fleet-default/ekr-old/old-uid",
					},
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "encryption-key-rotation-old-owner",
				},
			},
			getFn: func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error) {
				return &opv1alpha1.EncryptionKeyRotation{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      name,
						UID:       "old-uid",
					},
					Status: opv1alpha1.EncryptionKeyRotationStatus{
						OperationStatus: opv1alpha1.OperationStatus{Phase: opv1alpha1.OperationPhaseSucceeded},
					},
				}, nil
			},
			wantUpdate:       true,
			wantOwnerCleared: true,
			wantRefCleared:   true,
		},
		{
			name: "active matching owner does not reclaim",
			beacon: &planv1alpha1.Beacon{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fleet-default",
					Namespace: "fleet-default",
					Annotations: map[string]string{
						beaconOwnerRefAnnotation: "fleet-default/ekr-old/old-uid",
					},
				},
				Status: planv1alpha1.BeaconStatus{
					Owner: "encryption-key-rotation-old-owner",
				},
			},
			getFn: func(namespace, name string, opts metav1.GetOptions) (*opv1alpha1.EncryptionKeyRotation, error) {
				return &opv1alpha1.EncryptionKeyRotation{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      name,
						UID:       "old-uid",
					},
					Status: opv1alpha1.EncryptionKeyRotationStatus{
						OperationStatus: opv1alpha1.OperationStatus{Phase: opv1alpha1.OperationPhaseInProgress},
					},
				}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beaconClient := &fakeBeaconClient{}
			controller := &fakeEncryptionKeyRotationController{getFn: tt.getFn}
			h := &handler{
				beacons:                beaconClient,
				encryptionkeyrotations: controller,
			}
			s := &scope{
				op:     currentOp,
				beacon: tt.beacon.DeepCopy(),
			}

			err := h.reclaimStaleBeaconOwnerIfNeeded(s)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantUpdate && beaconClient.updateCalls == 0 {
				t.Fatalf("expected beacon update")
			}
			if !tt.wantUpdate && beaconClient.updateCalls > 0 {
				t.Fatalf("did not expect beacon update")
			}
			if !tt.wantUpdate {
				return
			}

			owner := s.beacon.Status.Owner
			if tt.wantOwnerCleared && owner != "" {
				t.Fatalf("expected owner label cleared, got %q", owner)
			}
			if s.beacon.Annotations == nil {
				if tt.wantRefCleared {
					return
				}
				t.Fatalf("expected annotations map present")
			}
			if tt.wantRefCleared && s.beacon.Annotations[beaconOwnerRefAnnotation] != "" {
				t.Fatalf("expected owner ref annotation cleared")
			}
		})
	}
}

// --- deletion cleanup (OnRemove) ---------------------------------------------------------------

// newDeletingOp returns the operation as a user has just deleted it: terminating, pointing at the
// imported mgmt v3 cluster. No finalizer is set — wrangler stamps
// wrangler.cattle.io/encryption-key-rotation-cleanup onto every operation and calls OnRemove under
// it, so the handler under test never sees or touches a finalizer.
func newDeletingOp(deletedAt time.Time) *opv1alpha1.EncryptionKeyRotation {
	op := newOp()
	ts := metav1.NewTime(deletedAt)
	op.DeletionTimestamp = &ts
	clusterRef, _ := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef
	return op
}

// ownerRefFor mirrors the owner-ref string handlePending writes onto the beacon.
func ownerRefFor(op *opv1alpha1.EncryptionKeyRotation) string {
	return fmt.Sprintf("%s/%s/%s", op.Namespace, op.Name, op.UID)
}

func TestOnRemove_ClearsAnnotationsAndReleasesTheBeacon(t *testing.T) {
	ctrl := gomock.NewController(t)

	// Both annotations, not just paused. A cancellation that ran to completion leaves the canceled
	// annotation behind too, and cleanup must not gate on PausedCondition — which handleCancel
	// deliberately clears — to decide whether there is anything to clear.
	secret := newImportedPlanSecret("plan-a", plan.PlanStatePaused)
	secret.Annotations[plan.PlanPausedAnnotation] = "true"
	secret.Annotations[plan.PlanCanceledAnnotation] = "true"

	_, cluster := newMgmtClusterRef()
	op := newDeletingOp(time.Now())

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, nil, secret)
	beacons := &fakeBeaconClient{getObj: &planv1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "c-m-test",
			Namespace:   "c-m-test",
			Annotations: map[string]string{beaconOwnerRefAnnotation: ownerRefFor(op)},
		},
		Status: planv1alpha1.BeaconStatus{Active: true, Owner: beaconOwnerKey(op)},
	}}

	h := &handler{
		beacons: beacons,
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	got, err := h.OnRemove("", op)
	assert.NoError(t, err)
	assert.Same(t, op, got, "OnRemove writes Secrets and the Beacon, never the operation")

	if assert.Len(t, updates, 1, "the leftover annotations must be cleared from the machine-plan secret") {
		assert.NotContains(t, updates[0].Annotations, plan.PlanPausedAnnotation,
			"a stranded paused annotation halts every plan on the node with no CR left to explain why")
		assert.NotContains(t, updates[0].Annotations, plan.PlanCanceledAnnotation)
	}

	if assert.Len(t, beacons.statusUpdates, 1, "the beacon this operation owned must be released") {
		assert.Empty(t, beacons.statusUpdates[0].Status.Owner)
		assert.False(t, beacons.statusUpdates[0].Status.Active)
	}

	// Unlike the other operation types this one also records its owner in a beacon annotation.
	// Leaving it points at an operation that no longer exists.
	if assert.Len(t, beacons.updates, 1, "the owner-ref annotation must be cleared too") {
		assert.NotContains(t, beacons.updates[0].Annotations, beaconOwnerRefAnnotation)
	}
}

// An owner-ref naming a different operation belongs to that operation. Clearing it would erase the
// evidence reclaimStaleBeaconOwnerIfNeeded uses to decide whether that owner is still alive.
func TestOnRemove_LeavesAnotherOperationsOwnerRef(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := newDeletingOp(time.Now())
	secrets := newSecretClient(t, ctrl, nil, nil)
	beacons := &fakeBeaconClient{getObj: &planv1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "c-m-test",
			Namespace:   "c-m-test",
			Annotations: map[string]string{beaconOwnerRefAnnotation: "fleet-default/other/other-uid"},
		},
		Status: planv1alpha1.BeaconStatus{Active: true, Owner: "encryption-key-rotation-other-uid"},
	}}

	h := &handler{
		beacons: beacons,
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnRemove("", op)
	assert.NoError(t, err)
	assert.Empty(t, beacons.statusUpdates, "another operation is legitimately driving this cluster")
	assert.Empty(t, beacons.updates, "the other operation's owner-ref must survive")
}

// wrangler drops its finalizer only on a nil return, so a retryable failure has to surface as an
// error or the leftover state is abandoned on the first attempt.
func TestOnRemove_ReturnsTheErrorWhileTheBudgetRemains(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	secrets := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	secrets.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("etcdserver: request timed out")).AnyTimes()

	h := &handler{
		beacons: &fakeBeaconClient{},
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnRemove("", newDeletingOp(time.Now()))
	assert.Error(t, err, "the deletion must be requeued rather than give up on the first failure")
}

func TestOnRemove_GivesUpOnceTheBudgetIsSpent(t *testing.T) {
	ctrl := gomock.NewController(t)

	secrets := newSecretClient(t, ctrl, nil, nil)
	h := &handler{
		beacons: &fakeBeaconClient{},
		secrets: secrets,
		store:   plan.NewStore(secrets),
		// getErr is a permanent, non-NotFound failure: the clusterRef GVK no longer resolves.
		dynamic: &fakeDynamic{getErr: errors.New("no matches for kind")},
	}

	logs := captureLogs(t)

	_, err := h.OnRemove("", newDeletingOp(time.Now().Add(-2*ops.CleanupBudget)))
	assert.NoError(t, err,
		"an undeletable CR blocks namespace teardown and cluster deprovisioning; a stranded "+
			"annotation is recoverable with one kubectl command. Giving up is the lesser failure.")

	assert.Contains(t, logs.String(), opv1alpha1.InterruptCleanupIncompleteReason,
		"the log line needs a stable token for alerting to key on")
	assert.Contains(t, logs.String(),
		"kubectl get secret -A --field-selector type=rke.cattle.io/machine-plan",
		"the cluster never resolved, so the operator gets a discovery command rather than a guessed namespace")
}

// captureLogs redirects logrus for the duration of the test and returns the buffer it writes to.
// See the identical helper in etcdsnapshotsave's tests for why the process-global redirect is safe
// here.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	t.Cleanup(func() { logrus.SetOutput(previous) })
	return &buf
}

// The canonical UI-created operation lives in fleet-default and points at the cluster-scoped mgmt
// v3 Cluster, while ImportedAdapter.BeaconRef puts the machine-plan Secrets in a namespace named
// after the cluster. Deriving the command's namespace from the operation would send the operator
// somewhere with no machine-plan Secrets in it.
func TestOnRemove_FailureNamesTheSecretsNamespaceNotTheOperations(t *testing.T) {
	ctrl := gomock.NewController(t)

	_, cluster := newMgmtClusterRef()
	op := newDeletingOp(time.Now().Add(-2 * ops.CleanupBudget))
	require.Equal(t, "fleet-default", op.Namespace)
	require.Empty(t, op.Spec.ClusterRef.Namespace, "the mgmt v3 Cluster is cluster-scoped")

	// The cluster resolves, so the scope is known; the Secret List is what fails.
	secrets := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	secrets.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, errors.New("etcdserver: request timed out")).AnyTimes()

	h := &handler{
		beacons: &fakeBeaconClient{},
		secrets: secrets,
		store:   plan.NewStore(secrets),
		dynamic: &fakeDynamic{getObj: cluster},
	}

	logs := captureLogs(t)
	_, err := h.OnRemove("", op)
	assert.NoError(t, err)

	assert.Contains(t, logs.String(),
		"kubectl annotate secret -n c-m-test -l rke.cattle.io/cluster-name=c-m-test "+
			"plan.cattle.io/canceled- plan.cattle.io/paused-",
		"the command must select exactly the Secrets the cleanup collector reads")
	assert.Contains(t, logs.String(), "kubectl patch beacon -n c-m-test c-m-test",
		"a beacon left held wedges the cluster and must be as discoverable as the annotations")
	assert.NotContains(t, logs.String(), "-n fleet-default",
		"the operation's own namespace holds no machine-plan Secrets")
}

// OnChange must not drive status for a terminating operation. Cleanup belongs to OnRemove, which
// runs under wrangler's finalizer.
func TestOnChange_TerminatingOperationDrivesNoStatus(t *testing.T) {
	ctrl := gomock.NewController(t)

	secrets := newSecretClient(t, ctrl, nil, nil)
	h := &handler{secrets: secrets, store: plan.NewStore(secrets)}

	status, err := h.OnChange(newDeletingOp(time.Now()), opv1alpha1.EncryptionKeyRotationStatus{})
	assert.NoError(t, err)
	assert.Equal(t, opv1alpha1.EncryptionKeyRotationStatus{}, status)
}

// The first reconcile that observes an interrupt must annotate immediately. Rancher used to spend
// this reconcile persisting a finalizer of its own; wrangler's is already in place by now.
func TestOnChange_TheFirstInterruptAnnotatesImmediately(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Paused = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, nil, newImportedPlanSecret("plan-a", plan.PlanStateInProgress))

	rotations := ctrlfake.NewMockControllerInterface[*opv1alpha1.EncryptionKeyRotation, *opv1alpha1.EncryptionKeyRotationList](ctrl)
	rotations.EXPECT().Update(gomock.Any()).Times(0)
	rotations.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	h := &handler{
		encryptionkeyrotations: rotations,
		beacons:                &fakeBeaconClient{getObj: newBeacon(beaconOwnerKey(op), true)},
		secrets:                secrets,
		store:                  plan.NewStore(secrets),
		dynamic:                &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnChange(op, opv1alpha1.EncryptionKeyRotationStatus{})
	assert.NoError(t, err)

	if assert.Len(t, updates, 1, "the pause must reach the machine-plan secret on this reconcile") {
		assert.Equal(t, "true", updates[0].Annotations[plan.PlanPausedAnnotation])
	}
}

// HandleInterrupt returns early for a terminal operation, so pausing one writes no annotation
// anywhere and its deletion has nothing to undo.
func TestOnChange_TerminalOperationIsNotInterrupted(t *testing.T) {
	ctrl := gomock.NewController(t)

	op := newOp()
	op.Spec.Paused = true
	clusterRef, cluster := newMgmtClusterRef()
	op.Spec.ClusterRef = clusterRef

	status := opv1alpha1.EncryptionKeyRotationStatus{}
	status.SetPhase(opv1alpha1.OperationPhaseSucceeded)
	op.Status = status

	var updates []*corev1.Secret
	secrets := newSecretClient(t, ctrl, &updates, nil, newImportedPlanSecret("plan-a", plan.PlanStateSucceeded))
	rotations := ctrlfake.NewMockControllerInterface[*opv1alpha1.EncryptionKeyRotation, *opv1alpha1.EncryptionKeyRotationList](ctrl)
	rotations.EXPECT().Update(gomock.Any()).Times(0)
	rotations.EXPECT().EnqueueAfter(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	rotations.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	h := &handler{
		encryptionkeyrotations: rotations,
		beacons:                &fakeBeaconClient{getObj: newBeacon("", false)},
		secrets:                secrets,
		store:                  plan.NewStore(secrets),
		dynamic:                &fakeDynamic{getObj: cluster},
	}

	_, err := h.OnChange(op, status)
	assert.NoError(t, err)
	assert.Empty(t, updates, "a terminal operation must not annotate anything")
}

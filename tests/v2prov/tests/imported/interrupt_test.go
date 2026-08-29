package imported

import (
	"fmt"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/controllers/operations/etcdsnapshotsave"
	planapi "github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// Interrupts are only meaningful once a plan exists on the machine-plan Secrets. Imported Secrets
// are created empty — pkg/controllers/capr/unmanaged sets no Data at all — and only an operation's
// own steps call AssignPlan. ops.HandleInterrupt runs before phase dispatch, so an interrupt that
// lands while the operation is still Pending freezes it there and no plan is ever written: the
// node has nothing to pause, nothing to cancel, and reports no plan-state.
//
// Every test here therefore gates on the Restart-step lifecycle hook rather than racing the
// controller. At that checkpoint the Save plan has been assigned and applied, so the interrupt
// lands on a node with real plan content and real agent-authored state.
const (
	interruptHookName     = "v2prov-interrupt-test"
	interruptDelegateName = "v2prov-interrupt-test-delegate"
)

var restartHookKey = etcdsnapshotsave.RestartStepHookLabelPrefix + interruptHookName

// withTTL overrides the operation's TTL, which CreateETCDSnapshotSaveOp otherwise hardcodes to 60
// seconds. A negative TTL disables garbage collection entirely (ops.IsExpired returns false for
// TTL < 0).
//
// Only needed by a test that leaves an operation parked in a terminal phase, because that is the
// only place OnChange consults the TTL at all — see the ops.IsTerminal guard in
// etcdsnapshotsave/controller.go. A merely paused or in-progress operation is never collected no
// matter how long it is held.
func withTTL(ttl int64) SnapshotSaveOption {
	return func(op *opv1alpha1.ETCDSnapshotSave) {
		op.Spec.TTL = ttl
	}
}

// gateAtRestartStep creates a snapshot-save operation held at the Restart step and returns it.
//
// Callers get an operation whose Save plan is assigned and applied on every etcd node, whose
// controller is parked on a delegate, and which has not yet dispatched the restart plan. That is
// the only point in the state machine a test can interrupt deterministically.
func gateAtRestartStep(t *testing.T, cs *clients.Clients, fx *importedClusterFixture, opts ...SnapshotSaveOption) *opv1alpha1.ETCDSnapshotSave {
	t.Helper()

	opts = append(opts, WithSaveLabels(map[string]string{restartHookKey: interruptDelegateName}))
	op := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef, opts...)

	cp := WaitForSnapshotSaveHookPause(t, cs, op, fx.mgmtCluster.Name, fx.mgmtCluster.Name,
		restartHookKey, interruptDelegateName,
		opv1alpha1.OperationPhaseInProgress, opv1alpha1.ETCDSnapshotSaveStepRestart)
	t.Logf("gated at Restart step: phase=%s step=%s delegates=%v",
		cp.Op.Status.Phase, cp.Op.Status.Step, cp.Beacon.Status.Delegates)

	// Guard the premise rather than trusting it. If a future change lets the Restart step be
	// reached without a dispatched plan, every interrupt assertion below would pass vacuously
	// against a Secret the agent has never looked at.
	require.True(t, everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
		if !secretHasPlan(s) {
			return true
		}
		return len(s.Data[planapi.PlanStateKey]) > 0
	}), "at the Restart step the save plan must be on the etcd secrets with agent-authored state")

	return op
}

// releaseRestartGate clears the hook label and pops the delegate, letting the controller proceed.
func releaseRestartGate(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave, fx *importedClusterFixture) {
	t.Helper()
	AdvancePastSnapshotSaveHook(t, cs, op, fx.mgmtCluster.Name, fx.mgmtCluster.Name,
		restartHookKey, interruptDelegateName)
}

// patchOperationSpec re-reads the operation, applies mutate to its spec and writes it back.
//
// Conflicts are retried: the controller re-reconciles every 5 seconds and the generated status
// handler writes the whole object, so a 409 here is ordinary and must not fail a test. Every other
// API error is returned unmodified, because the cancel test asserts that the CEL transition rule
// rejects un-cancelling and wrapping that error would hide it.
func patchOperationSpec(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave, mutate func(spec *opv1alpha1.OperationSpec)) error {
	t.Helper()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		latest = latest.DeepCopy()
		mutate(&latest.Spec.OperationSpec)
		_, err = cs.Operation.ETCDSnapshotSave().Update(latest)
		return err
	})
}

// getOp re-reads the operation. Every interrupt assertion is about status the controller wrote
// after the test's own write, so the object handed back by Create is always stale.
//
// Only call this from the test goroutine. Inside a polling closure use tryGetOp.
func getOp(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave) *opv1alpha1.ETCDSnapshotSave {
	t.Helper()

	got, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// tryGetOp re-reads the operation for use inside an Eventually or Never closure, reporting failure
// rather than raising it.
//
// testify runs those closures on their own goroutine, where require's FailNow calls
// runtime.Goexit: the closure would vanish mid-poll instead of failing the test, and the poll would
// run to its timeout reporting nothing useful. A read error is a transient to retry, not a verdict.
func tryGetOp(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave) (*opv1alpha1.ETCDSnapshotSave, bool) {
	t.Helper()

	got, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
	if err != nil {
		t.Logf("getting operation %s/%s: %v", op.Namespace, op.Name, err)
		return nil, false
	}
	return got, true
}

// opReachedPhase reports whether the operation is in phase, for use inside a polling closure.
func opReachedPhase(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave, phase opv1alpha1.OperationPhase) bool {
	t.Helper()

	got, ok := tryGetOp(t, cs, op)
	return ok && got.Status.Phase == phase
}

// listPlanSecrets returns every machine-plan Secret belonging to the fixture's cluster. They live
// in the namespace named after the management cluster, which is also where handleError collects
// them for the failure bundle.
func listPlanSecrets(t *testing.T, cs *clients.Clients, fx *importedClusterFixture) ([]corev1.Secret, error) {
	t.Helper()

	secrets, err := cs.Core.Secret().List(fx.mgmtCluster.Name, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", capr.ClusterNameLabel, fx.mgmtCluster.Name),
		FieldSelector: fmt.Sprintf("type=%s", planapi.SecretTypeMachinePlan),
	})
	if err != nil {
		return nil, err
	}
	return secrets.Items, nil
}

// everyPlanSecret applies pred to every machine-plan Secret belonging to the fixture's cluster and
// reports whether all of them satisfy it.
//
// minSecrets is the floor the caller expects to inspect. Fewer than that is reported as false,
// never as vacuously true: an interrupt that reached no node at all is precisely the failure these
// tests exist to catch, and "every node agrees" over zero nodes would report it as success. A
// multi-node test passes its node count so it cannot pass while looking at one Secret.
func everyPlanSecret(t *testing.T, cs *clients.Clients, fx *importedClusterFixture, minSecrets int, pred func(secret *corev1.Secret) bool) bool {
	t.Helper()

	items, err := listPlanSecrets(t, cs, fx)
	if err != nil {
		t.Logf("listing machine-plan secrets in %s: %v", fx.mgmtCluster.Name, err)
		return false
	}
	if len(items) < minSecrets {
		return false
	}
	for i := range items {
		if !pred(&items[i]) {
			return false
		}
	}
	return true
}

// secretHasPlan mirrors ops.hasPlan. A machine-plan Secret with no plan carries no evidence about
// the operation: the interrupt annotations are written to it, but the agent reports no state for
// it. Predicates about agent-authored state must skip these, or a cluster with a dedicated worker
// under an etcd-scoped step can never satisfy them.
func secretHasPlan(secret *corev1.Secret) bool {
	return len(secret.Data["plan"]) > 0
}

// nodeStopped reports whether the agent has demonstrably stopped working on this node, using the
// same definition Rancher uses in ops.unstoppedNodes: paused, or any terminal state.
//
// Asserting plan-state == paused specifically would be wrong. At the Restart-step gate the save
// plan has already completed, so the honest agent-authored state is "succeeded"; and even mid-plan
// an agent that finishes its last instruction as the annotation lands reports "succeeded" and is
// correctly counted as stopped.
func nodeStopped(secret *corev1.Secret) bool {
	if !secretHasPlan(secret) {
		return true
	}
	state := planapi.PlanState(secret.Data[planapi.PlanStateKey])
	return state == planapi.PlanStatePaused || state.IsTerminal()
}

func hasAnnotation(secret *corev1.Secret, key string) bool {
	_, ok := secret.Annotations[key]
	return ok
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResume exercises pause and resume
// against a real system-agent. Pausing must reach every node and stop Rancher from dispatching the
// next step's plan; clearing the pause must let the operation finish.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResume(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-pause", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	// The default TTL is fine here. TTL is only consulted under ops.IsTerminal, and this test never
	// leaves the operation sitting in a terminal phase.
	op := gateAtRestartStep(t, cs, fx)

	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = true }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return s.Annotations[planapi.PlanPausedAnnotation] == "true" && nodeStopped(s)
		})
	}, 5*time.Minute, 5*time.Second, "the pause must reach every node and every node must have stopped")

	got := getOp(t, cs, op)
	assert.True(t, opv1alpha1.PausedCondition.IsTrue(&got.Status))
	assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(&got.Status),
		"once every node reports stopped the condition must report in-effect, not requested")

	// Release the hook while still paused. The controller is now free to run reconcileRestart, and
	// the pause is the only thing that may stop it.
	planBefore := etcdPlanBytes(t, cs, fx)
	releaseRestartGate(t, cs, op, fx)

	// This is what pause means to a user: forward progress stops. HandleInterrupt returns before
	// phase dispatch, so the restart plan must not be assigned while spec.paused is set.
	require.Never(t, func() bool {
		latest, ok := tryGetOp(t, cs, op)
		if !ok {
			// A failed read is not evidence that the operation moved. Reporting true here would
			// fail the test on a transient API error.
			return false
		}
		if latest.Status.Step != opv1alpha1.ETCDSnapshotSaveStepRestart {
			t.Logf("operation advanced to step %q while paused", latest.Status.Step)
			return true
		}
		return !equalPlanBytes(planBefore, etcdPlanBytes(t, cs, fx))
	}, 30*time.Second, 5*time.Second, "a paused operation must not dispatch the next step's plan")

	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = false }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return !hasAnnotation(s, planapi.PlanPausedAnnotation)
		})
	}, 2*time.Minute, 5*time.Second, "Rancher owns the annotation; the agent never removes one")

	WaitForSnapshotSaveSucceeded(t, cs, op, beaconNS, beaconName)
}

// etcdPlanBytes snapshots the plan content of every machine-plan Secret that has one, keyed by
// Secret name, so a test can prove Rancher did or did not dispatch new content.
func etcdPlanBytes(t *testing.T, cs *clients.Clients, fx *importedClusterFixture) map[string]string {
	t.Helper()

	out := map[string]string{}
	items, err := listPlanSecrets(t, cs, fx)
	if err != nil {
		t.Logf("listing machine-plan secrets in %s: %v", fx.mgmtCluster.Name, err)
		return out
	}
	for i := range items {
		if secretHasPlan(&items[i]) {
			out[items[i].Name] = string(items[i].Data["plan"])
		}
	}
	return out
}

func equalPlanBytes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancel exercises cancellation and the
// delete-and-recreate recovery path, which is the failure mode that is otherwise silent.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancel(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-cancel", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	// This operation does need withTTL(-1): unlike the pause test it sits in a terminal phase
	// (Canceled) while the test reads its conditions, tries the CEL-rejected un-cancel, and then
	// deletes it by hand. TTL is consulted exactly there, so the default 60s could garbage-collect
	// the operation out from under all three steps.
	op := gateAtRestartStep(t, cs, fx, withTTL(-1))
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Cancel = true }))

	require.Eventually(t, func() bool {
		return opReachedPhase(t, cs, op, opv1alpha1.OperationPhaseCanceled)
	}, 10*time.Minute, 5*time.Second, "the operation must reach Canceled once every node confirms")

	got := getOp(t, cs, op)
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got.Status),
		"every node's plan-state is terminal at this gate, so the cancellation is confirmed rather than timed out")

	// IsFalse, not !IsTrue: the point is that the operation *affirmatively* records that nothing
	// needs recovering. An absent condition would satisfy !IsTrue while telling the user nothing.
	//
	// Deterministic at this gate: ETCDSnapshotSave declares RequiresRecovery: false, and no
	// instruction was interrupted part-way, so the agent cannot report terminationIncomplete —
	// which is the one signal that raises this condition regardless of the CancelPolicy.
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(&got.Status),
		"a snapshot save registers RequiresRecovery: false")
	assert.Equal(t, opv1alpha1.NotRecoveryRequiredReason,
		opv1alpha1.RecoveryRequiredCondition.GetReason(&got.Status))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return s.Annotations[planapi.PlanCanceledAnnotation] == "true"
		})
	}, 2*time.Minute, 5*time.Second, "the cancellation must be recorded on every node's plan secret")

	// spec.cancel is terminal: the API server must reject un-cancelling it. Assert the specific
	// rejection, not merely "an error" — a conflict or a network blip would otherwise pass for a
	// CEL rejection and the rule could be deleted without this test noticing.
	err = patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Cancel = false })
	require.Error(t, err, "the CEL transition rule must reject true -> false")
	assert.True(t, apierrors.IsInvalid(err),
		"the rejection must come from the CEL rule as a 422, not from a conflict: %v", err)

	// Delete and re-create: the new operation must actually run rather than wedging on the
	// leftover canceled annotation and terminal plan-state.
	require.NoError(t, cs.Operation.ETCDSnapshotSave().Delete(op.Namespace, op.Name, &metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return !hasAnnotation(s, planapi.PlanCanceledAnnotation)
		})
	}, 2*time.Minute, 5*time.Second, "the OnRemove cleanup handler must clear the annotation on deletion")

	// No withTTL(-1) on the retry: nothing inspects it after it goes terminal, and
	// WaitForSnapshotSaveSucceeded polls every 2s, so it observes Succeeded long before the
	// default 60s TTL can collect it — the same arrangement every other snapshot test here uses.
	retryOp := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef)
	WaitForSnapshotSaveSucceeded(t, cs, retryOp, beaconNS, beaconName)
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveDeleteWhilePaused deletes a paused operation
// and proves the cluster is left usable.
//
// Two things are stranded by a delete that does no cleanup, and both are silent. A leftover
// plan.cattle.io/paused annotation halts every future plan on the node with no CR left to explain
// why. A leftover beacon owner key blocks every future day-2 operation in AcquireBeacon, because
// the terminal-phase handlers are the only other release path and a deleted operation never
// reaches one. The final snapshot save is the assertion that covers both.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveDeleteWhilePaused(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-delete", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})

	op := gateAtRestartStep(t, cs, fx)
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = true }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return s.Annotations[planapi.PlanPausedAnnotation] == "true"
		})
	}, 5*time.Minute, 5*time.Second, "the pause must reach the machine-plan secrets before the delete")

	// Delete while paused and mid-flight. The operation is non-terminal, so it still owns the
	// beacon and still has the hook delegate on the chain.
	require.NoError(t, cs.Operation.ETCDSnapshotSave().Delete(op.Namespace, op.Name, &metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 5*time.Minute, 5*time.Second,
		"the cleanup handler must finish and let wrangler drop its finalizer; a stuck object here "+
			"would block namespace teardown")

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return !hasAnnotation(s, planapi.PlanPausedAnnotation) &&
				!hasAnnotation(s, planapi.PlanCanceledAnnotation)
		})
	}, 2*time.Minute, 5*time.Second, "no interrupt annotation may outlive the operation that wrote it")

	// ReleaseBeacon clears the whole delegate chain along with the owner, so the hook delegate this
	// test pushed is gone too and nothing needs popping by hand.
	require.Eventually(t, func() bool {
		beacon, err := cs.Plan.Beacon().Get(fx.mgmtCluster.Name, fx.mgmtCluster.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("getting beacon %s: %v", fx.mgmtCluster.Name, err)
			return false
		}
		return beacon.Status.Owner == "" && len(beacon.Status.Delegates) == 0
	}, 2*time.Minute, 5*time.Second, "a beacon still owned by a deleted operation wedges the cluster")

	// The real proof: a plain operation must run to completion on this cluster.
	RunETCDSnapshotSaveOperationTest(t, cs, fx.ns.Name, fx.clusterRef)
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancelBeforeDispatch covers the shortcut in
// ops.HandleInterrupt for an operation canceled before any plan reached a node.
//
// The Pending-phase hook gates the operation after it acquires the beacon but before it can wait
// for agent registration or assign anything, which is the only deterministic way to hold it in
// Pending. Nothing has been written to a machine-plan Secret at that point, so cancellation is
// immediate and has nothing to undo.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancelBeforeDispatch(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-pending", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	pendingHookKey := planv1alpha1.PendingPhaseHookLabelPrefix + interruptHookName
	op := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef, withTTL(-1),
		WithSaveLabels(map[string]string{pendingHookKey: interruptDelegateName}))

	WaitForSnapshotSaveHookPause(t, cs, op, beaconNS, beaconName, pendingHookKey, interruptDelegateName,
		opv1alpha1.OperationPhasePending, "")

	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Cancel = true }))

	require.Eventually(t, func() bool {
		return opReachedPhase(t, cs, op, opv1alpha1.OperationPhaseCanceled)
	}, 5*time.Minute, 5*time.Second, "cancelling before dispatch must be immediate; there is nothing to confirm")

	got := getOp(t, cs, op)
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got.Status))
	assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(&got.Status), "canceled before any plan was dispatched")

	// No annotation may be written. SyncInterruptAnnotations is only reached on the non-Pending
	// path, and writing plan.cattle.io/canceled to a node that never received a plan would leave
	// the agent refusing content it has no reason to refuse.
	assert.True(t, everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
		return !hasAnnotation(s, planapi.PlanCanceledAnnotation) &&
			!hasAnnotation(s, planapi.PlanPausedAnnotation)
	}), "cancelling before dispatch must not annotate any machine-plan secret")

	// Documenting current behaviour, not endorsing it: the Pending branch of handleCancel returns
	// before setRecoveryCondition, so RecoveryRequired is never written at all. A user therefore
	// cannot tell "nothing to recover" from "not evaluated". If that branch is ever changed to
	// report explicitly, this assertion is the one to update.
	assert.False(t, opv1alpha1.RecoveryRequiredCondition.IsTrue(&got.Status))
	assert.False(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(&got.Status),
		"the Pending cancel path writes no RecoveryRequired condition at all")

	// The beacon was acquired in handlePending, so handleCanceled has to give it back.
	AdvancePastSnapshotSaveHook(t, cs, op, beaconNS, beaconName, pendingHookKey, interruptDelegateName)
	require.Eventually(t, func() bool {
		beacon, err := cs.Plan.Beacon().Get(beaconNS, beaconName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return beacon.Status.Owner == "" && len(beacon.Status.Delegates) == 0
	}, 5*time.Minute, 5*time.Second, "a canceled operation must release the beacon it acquired")
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancelBeatsPause sets both interrupts in a
// single write and proves cancel wins on the wire, not just in the status.
//
// The agent has one source of truth per Secret. Leaving both annotations set would tell it to hold
// at an instruction boundary and to abort, and only one of those is what the user asked for.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSaveCancelBeatsPause(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-precedence", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})

	op := gateAtRestartStep(t, cs, fx, withTTL(-1))

	// One write, both flags. Setting them in two writes would let the controller observe the pause
	// alone first, which is a different scenario.
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) {
		spec.Paused = true
		spec.Cancel = true
	}))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
			return s.Annotations[planapi.PlanCanceledAnnotation] == "true" &&
				!hasAnnotation(s, planapi.PlanPausedAnnotation)
		})
	}, 5*time.Minute, 5*time.Second, "cancel must be written and pause cleared, never both at once")

	require.Eventually(t, func() bool {
		return opReachedPhase(t, cs, op, opv1alpha1.OperationPhaseCanceled)
	}, 10*time.Minute, 5*time.Second, "the operation must reach Canceled despite spec.paused also being set")

	got := getOp(t, cs, op)
	assert.False(t, opv1alpha1.PausedCondition.IsTrue(&got.Status),
		"an operation reporting Canceled and Paused at once contradicts itself")
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(&got.Status))
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResumeCycles pauses and resumes twice
// before letting the operation finish.
//
// Pause is not one-shot, and the machinery that clears the annotation has two owners: handleResume
// clears it on the resume reconcile, and AssignPlan clears it unconditionally on every plan write.
// Cycling proves they converge rather than fight, and that a second pause is still honoured after
// a plan has been dispatched in between.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResumeCycles(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-cycles", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	op := gateAtRestartStep(t, cs, fx)

	for cycle := 1; cycle <= 2; cycle++ {
		t.Logf("pause/resume cycle %d", cycle)

		require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = true }))
		require.Eventually(t, func() bool {
			return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
				return s.Annotations[planapi.PlanPausedAnnotation] == "true" && nodeStopped(s)
			})
		}, 5*time.Minute, 5*time.Second, "cycle %d: the pause must reach every node", cycle)

		require.Equal(t, opv1alpha1.PausedReason,
			opv1alpha1.PausedCondition.GetReason(&getOp(t, cs, op).Status),
			"cycle %d: a repeated pause must report in-effect just like the first", cycle)

		require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = false }))
		require.Eventually(t, func() bool {
			return everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
				return !hasAnnotation(s, planapi.PlanPausedAnnotation)
			})
		}, 2*time.Minute, 5*time.Second, "cycle %d: the resume must clear the annotation", cycle)

		require.False(t, opv1alpha1.PausedCondition.IsTrue(&getOp(t, cs, op).Status),
			"cycle %d: PausedCondition must be cleared or the next pause has nothing to toggle", cycle)
	}

	releaseRestartGate(t, cs, op, fx)
	WaitForSnapshotSaveSucceeded(t, cs, op, beaconNS, beaconName)
}

// Test_Imported_Operation_SetE_ImportedETCDSnapshotSaveInterruptFansOut runs the pause against a
// three-node cluster.
//
// Every interrupt assertion in this package is phrased as "every node", and on a single-node
// cluster that quantifier is untested: one Secret satisfies it trivially. Here everyPlanSecret is
// given a floor of three, so a pause that reaches only the node the controller happened to write
// first fails instead of passing.
//
// The transitional PauseRequested reason, which names the nodes that have not stopped yet, is not
// deterministically observable: at the Restart-step gate every node's plan-state is already
// terminal, so the condition goes straight to Paused. Covering that window belongs in
// pkg/operations/interrupt_test.go, where node states can be fixed.
func Test_Imported_Operation_SetE_ImportedETCDSnapshotSaveInterruptFansOut(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	const nodes = 3

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-fanout", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: nodes},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	op := gateAtRestartStep(t, cs, fx)

	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = true }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, nodes, func(s *corev1.Secret) bool {
			return s.Annotations[planapi.PlanPausedAnnotation] == "true" && nodeStopped(s)
		})
	}, 5*time.Minute, 5*time.Second, "the pause must reach all %d nodes, not just the first", nodes)

	// Sanity-check the floor actually bit: if the cluster came up with fewer plan secrets than
	// nodes, the assertion above could only have passed by the collector returning them all.
	items, err := listPlanSecrets(t, cs, fx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(items), nodes,
		"the fan-out assertion is only meaningful over more than one machine-plan secret")

	got := getOp(t, cs, op)
	assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(&got.Status),
		"the condition must not report in-effect until every one of the nodes has stopped")

	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = false }))
	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, nodes, func(s *corev1.Secret) bool {
			return !hasAnnotation(s, planapi.PlanPausedAnnotation)
		})
	}, 2*time.Minute, 5*time.Second, "the resume must clear the annotation from all %d nodes", nodes)

	releaseRestartGate(t, cs, op, fx)
	WaitForSnapshotSaveSucceeded(t, cs, op, beaconNS, beaconName)
}

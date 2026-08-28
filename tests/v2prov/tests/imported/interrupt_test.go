package imported

import (
	"fmt"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/capr"
	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// patchOperationSpec re-reads the operation, applies mutate to its spec and writes it back. The
// API error is returned unmodified: the cancel test asserts that the CEL transition rule rejects
// un-cancelling, and wrapping or fataling on the error would hide exactly that.
func patchOperationSpec(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave, mutate func(spec *opv1alpha1.OperationSpec)) error {
	t.Helper()

	latest, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	latest = latest.DeepCopy()
	mutate(&latest.Spec.OperationSpec)
	_, err = cs.Operation.ETCDSnapshotSave().Update(latest)
	return err
}

// getOp re-reads the operation. Every interrupt assertion is about status the controller wrote
// after the test's own write, so the object handed back by Create is always stale.
func getOp(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotSave) *opv1alpha1.ETCDSnapshotSave {
	t.Helper()

	got, err := cs.Operation.ETCDSnapshotSave().Get(op.Namespace, op.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// everyPlanSecret applies pred to every machine-plan Secret belonging to the fixture's cluster and
// reports whether all of them satisfy it.
//
// An empty list is reported as false, never as vacuously true: an interrupt that reached no node
// at all is precisely the failure these tests exist to catch, and "every node agrees" over zero
// nodes would report it as success.
//
// The Secrets live in the namespace named after the management cluster, which is also where
// handleError collects them for the failure bundle.
func everyPlanSecret(t *testing.T, cs *clients.Clients, fx *importedClusterFixture, pred func(data map[string][]byte, annotations map[string]string) bool) bool {
	t.Helper()

	secrets, err := cs.Core.Secret().List(fx.mgmtCluster.Name, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", capr.ClusterNameLabel, fx.mgmtCluster.Name),
		FieldSelector: fmt.Sprintf("type=%s", planapi.SecretTypeMachinePlan),
	})
	if err != nil {
		t.Logf("listing machine-plan secrets in %s: %v", fx.mgmtCluster.Name, err)
		return false
	}
	if len(secrets.Items) == 0 {
		return false
	}
	for i := range secrets.Items {
		if !pred(secrets.Items[i].Data, secrets.Items[i].Annotations) {
			return false
		}
	}
	return true
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResume exercises pause and resume
// against a real system-agent: pausing must stop the plan at an instruction boundary and leave a
// resume checkpoint, and clearing the pause must let it finish rather than restart.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotSavePauseResume(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-interrupt-pause", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	// The default TTL is fine here. TTL is only consulted under ops.IsTerminal, and this test
	// never leaves the operation sitting in a terminal phase: it holds it paused (InProgress
	// throughout) and then polls it to Succeeded on a 2s interval, exactly as the other snapshot
	// tests in this directory do.
	op := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef)

	// Pause, then assert the annotation reaches the machine-plan Secret and the agent records it.
	// The patch lands within a round trip or two of the Create, long before the operation can walk
	// Preflight -> Save -> Restart, each of which waits on a downstream plan application.
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = true }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, func(data map[string][]byte, ann map[string]string) bool {
			return ann[planapi.PlanPausedAnnotation] == "true" &&
				planapi.PlanState(data[planapi.PlanStateKey]) == planapi.PlanStatePaused
		})
	}, 5*time.Minute, 5*time.Second, "the agent must record plan-state: paused")

	got := getOp(t, cs, op)
	assert.True(t, opv1alpha1.PausedCondition.IsTrue(&got.Status))
	assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(&got.Status),
		"once every node reports paused the condition must report in-effect, not requested")

	// Resume and let it finish.
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Paused = false }))

	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, func(_ map[string][]byte, ann map[string]string) bool {
			_, present := ann[planapi.PlanPausedAnnotation]
			return !present
		})
	}, 2*time.Minute, 5*time.Second, "Rancher owns the annotation; the agent never removes one")

	WaitForSnapshotSaveSucceeded(t, cs, op, beaconNS, beaconName)
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
	op := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef, withTTL(-1))
	require.NoError(t, patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Cancel = true }))

	require.Eventually(t, func() bool {
		return getOp(t, cs, op).Status.Phase == opv1alpha1.OperationPhaseCanceled
	}, 10*time.Minute, 5*time.Second, "the operation must reach Canceled once every node confirms")

	// IsFalse, not !IsTrue: the point is that the operation *affirmatively* records that nothing
	// needs recovering. An absent condition would satisfy !IsTrue while telling the user nothing.
	got := getOp(t, cs, op)
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(&got.Status),
		"a snapshot save registers RequiresRecovery: false")

	// spec.cancel is terminal: the API server must reject un-cancelling it.
	err = patchOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) { spec.Cancel = false })
	require.Error(t, err, "the CEL transition rule must reject true -> false")

	// Delete and re-create: the new operation must actually run rather than wedging on the
	// leftover canceled annotation and terminal plan-state.
	require.NoError(t, cs.Operation.ETCDSnapshotSave().Delete(op.Namespace, op.Name, &metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		return everyPlanSecret(t, cs, fx, func(_ map[string][]byte, ann map[string]string) bool {
			_, canceled := ann[planapi.PlanCanceledAnnotation]
			return !canceled
		})
	}, 2*time.Minute, 5*time.Second, "the cleanup finalizer must clear the annotation on deletion")

	// No withTTL(-1) on the retry: nothing inspects it after it goes terminal, and
	// WaitForSnapshotSaveSucceeded polls every 2s, so it observes Succeeded long before the
	// default 60s TTL can collect it — the same arrangement every other snapshot test here uses.
	retry := CreateETCDSnapshotSaveOp(t, cs, fx.ns.Name, fx.clusterRef)
	WaitForSnapshotSaveSucceeded(t, cs, retry, beaconNS, beaconName)
}

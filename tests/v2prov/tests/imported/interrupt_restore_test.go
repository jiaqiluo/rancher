package imported

import (
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/controllers/operations/etcdsnapshotrestore"
	planapi "github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/rancher/tests/v2prov/clients"
	"github.com/rancher/rancher/tests/v2prov/cluster"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// patchRestoreOperationSpec is patchOperationSpec for the restore operation type. Conflicts are
// retried because the controller reconciles on a timer and rewrites the whole object.
func patchRestoreOperationSpec(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotRestore,
	mutate func(spec *opv1alpha1.OperationSpec)) error {
	t.Helper()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := cs.Operation.ETCDSnapshotRestore().Get(op.Namespace, op.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		latest = latest.DeepCopy()
		mutate(&latest.Spec.OperationSpec)
		_, err = cs.Operation.ETCDSnapshotRestore().Update(latest)
		return err
	})
}

// getRestoreOp re-reads the operation. Only call it from the test goroutine.
func getRestoreOp(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotRestore) *opv1alpha1.ETCDSnapshotRestore {
	t.Helper()

	got, err := cs.Operation.ETCDSnapshotRestore().Get(op.Namespace, op.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// restoreReachedPhase reports whether the operation is in phase, for use inside a polling closure.
// See tryGetOp for why require must not be called from one.
func restoreReachedPhase(t *testing.T, cs *clients.Clients, op *opv1alpha1.ETCDSnapshotRestore,
	phase opv1alpha1.OperationPhase) bool {
	t.Helper()

	got, err := cs.Operation.ETCDSnapshotRestore().Get(op.Namespace, op.Name, metav1.GetOptions{})
	if err != nil {
		t.Logf("getting operation %s/%s: %v", op.Namespace, op.Name, err)
		return false
	}
	return got.Status.Phase == phase
}

// Test_Imported_Operation_SetD_ImportedETCDSnapshotRestoreCancelRequiresRecovery is the other half
// of the CancelPolicy contract.
//
// ETCDSnapshotSave declares RequiresRecovery: false, so every cancel test against it can only ever
// observe the negative case. ETCDSnapshotRestore declares RequiresRecovery: true, and it is the
// operation where getting this wrong is most expensive: a cancelled restore can leave the datastore
// divergent across nodes, and the user has to be told so in a way they cannot miss.
//
// The Restore-step hook is the deterministic gate. By then the Shutdown plan has been assigned and
// applied on every node — the cluster's server units are down — so latchMutationEvidence records
// AnyNodeMutationObserved from a terminal plan-state and the policy applies.
//
// The cluster is deliberately left broken. This test ends at the cancellation; proving the recovery
// advice works is the restore test's job.
func Test_Imported_Operation_SetD_ImportedETCDSnapshotRestoreCancelRequiresRecovery(t *testing.T) {
	cs, err := clients.New()
	require.NoError(t, err)
	defer cs.Close()

	fx := setUpImportedCluster(t, cs, "test-imported-restore-cancel", []cluster.ImportedNodePool{
		{ControlPlane: true, ETCD: true, Worker: true, Quantity: 1},
	})
	beaconNS, beaconName := fx.mgmtCluster.Name, fx.mgmtCluster.Name

	// A restore needs something to restore from. Allow for clock skew between the test runner and
	// the in-cluster controller when deciding which snapshots are ours.
	snapshotsValidAfter := time.Now().Add(-30 * time.Second)
	RunETCDSnapshotSaveOperationTest(t, cs, fx.ns.Name, fx.clusterRef)
	waitForSnapshots(t, cs, fx.mgmtCluster.Name, fx.mgmtCluster.Name, snapshotsValidAfter, 1)
	snapshot := waitForBackpopulatedSnapshot(t, cs, fx.mgmtCluster.Name, fx.mgmtCluster.Name,
		"imported-init-0", snapshotsValidAfter)
	require.NotEmpty(t, snapshot.SnapshotFile.Name, "back-populated snapshot has no file name")

	restoreHookKey := etcdsnapshotrestore.RestoreStepHookLabelPrefix + interruptHookName

	// TTL -1: the operation sits in Canceled while the test reads its conditions, and the default
	// 60s would collect it out from under them.
	op := CreateETCDSnapshotRestoreOp(t, cs, fx.ns.Name, snapshot.SnapshotFile.Name, fx.clusterRef,
		WithRestoreLabels(map[string]string{restoreHookKey: interruptDelegateName}),
		func(op *opv1alpha1.ETCDSnapshotRestore) { op.Spec.TTL = -1 })

	cp := WaitForSnapshotRestoreHookPause(t, cs, op, beaconNS, beaconName,
		restoreHookKey, interruptDelegateName,
		opv1alpha1.OperationPhaseInProgress, opv1alpha1.ETCDSnapshotRestoreStepRestore)
	t.Logf("gated at Restore step: phase=%s step=%s delegates=%v",
		cp.Op.Status.Phase, cp.Op.Status.Step, cp.Beacon.Status.Delegates)

	// Guard the premise: the Shutdown plan must actually be on the nodes, or the mutation evidence
	// this test depends on would never be latched and the assertion below would pass vacuously.
	require.True(t, everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
		if !secretHasPlan(s) {
			return true
		}
		return len(s.Data[planapi.PlanStateKey]) > 0
	}), "at the Restore step the shutdown plan must be on the secrets with agent-authored state")

	require.NoError(t, patchRestoreOperationSpec(t, cs, op, func(spec *opv1alpha1.OperationSpec) {
		spec.Cancel = true
	}))

	require.Eventually(t, func() bool {
		return restoreReachedPhase(t, cs, op, opv1alpha1.OperationPhaseCanceled)
	}, 10*time.Minute, 5*time.Second, "the operation must reach Canceled once every node confirms")

	got := getRestoreOp(t, cs, op)

	// The whole point. A restore cancelled after shutdown has stopped etcd on every node, and
	// AnyNodeMutationObserved plus RequiresRecovery must turn that into an unmissable statement.
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsTrue(&got.Status),
		"a cancelled restore that already shut the cluster down needs manual recovery")
	assert.Equal(t, opv1alpha1.RecoveryRequiredReason,
		opv1alpha1.RecoveryRequiredCondition.GetReason(&got.Status),
		"the reason must name the policy, not the weaker TerminationIncomplete signal")
	assert.Contains(t, opv1alpha1.RecoveryRequiredCondition.GetMessage(&got.Status),
		"the restore was interrupted partway",
		"the message is the operator's only instruction; it must be the restore-specific one")

	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(&got.Status))

	require.True(t, everyPlanSecret(t, cs, fx, 1, func(s *corev1.Secret) bool {
		return s.Annotations[planapi.PlanCanceledAnnotation] == "true"
	}), "the cancellation must be recorded on every node's plan secret")

	// A cancelled restore must still hand the cluster back, or the recovery it just told the user
	// to perform could never be started.
	AdvancePastSnapshotRestoreHook(t, cs, op, beaconNS, beaconName, restoreHookKey, interruptDelegateName)
	require.Eventually(t, func() bool {
		beacon, err := cs.Plan.Beacon().Get(beaconNS, beaconName, metav1.GetOptions{})
		if err != nil {
			t.Logf("getting beacon %s: %v", beaconName, err)
			return false
		}
		return beacon.Status.Owner == "" && len(beacon.Status.Delegates) == 0
	}, 5*time.Minute, 5*time.Second,
		"a cancelled restore must release the beacon so a recovery restore can acquire it")
}

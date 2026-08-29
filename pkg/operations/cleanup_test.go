package operations

import (
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	plancontrollers "github.com/rancher/rancher/pkg/plan/generated/controllers/plan.cattle.io/v1alpha1"
	ctrlfake "github.com/rancher/wrangler/v3/pkg/generic/fake"
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
)

// cleanupOwnerKey is what plan.ControllerOwnerKey produces for the operation these tests delete.
const cleanupOwnerKey = "etcd-snapshot-save/fleet-default/op-1"

// cleanupClusterName is the imported mgmt v3 cluster the tests act on. ImportedAdapter.BeaconRef
// returns (name, name), so this doubles as the namespace holding the beacon and the machine-plan
// Secrets.
const cleanupClusterName = "c-m-test"

// --- fixtures ---------------------------------------------------------------------------------

// cleanupDynamic resolves exactly one ClusterRef, or fails every lookup when getErr is set.
type cleanupDynamic struct {
	obj    runtime.Object
	getErr error
}

func (d *cleanupDynamic) Get(_ schema.GroupVersionKind, _, _ string) (runtime.Object, error) {
	if d.getErr != nil {
		return nil, d.getErr
	}
	return d.obj, nil
}

// cleanupBeacons is an in-memory BeaconClient covering the two methods releaseClusterBeacon calls.
// Embedding the interface makes any other call panic, which is the signal we want.
type cleanupBeacons struct {
	plancontrollers.BeaconClient

	obj           *planv1alpha1.Beacon
	getErr        error
	updateErr     error
	statusUpdates []*planv1alpha1.Beacon
}

func (b *cleanupBeacons) Get(_, name string, _ metav1.GetOptions) (*planv1alpha1.Beacon, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	if b.obj == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "beacons"}, name)
	}
	return b.obj, nil
}

func (b *cleanupBeacons) UpdateStatus(beacon *planv1alpha1.Beacon) (*planv1alpha1.Beacon, error) {
	b.statusUpdates = append(b.statusUpdates, beacon.DeepCopy())
	if b.updateErr != nil {
		return nil, b.updateErr
	}
	return beacon, nil
}

func cleanupBeacon(owner string, delegates ...string) *planv1alpha1.Beacon {
	return &planv1alpha1.Beacon{
		ObjectMeta: metav1.ObjectMeta{Name: cleanupClusterName, Namespace: cleanupClusterName},
		Status: planv1alpha1.BeaconStatus{
			Active:    true,
			Owner:     owner,
			Delegates: delegates,
		},
	}
}

// cleanupSecret is a machine-plan Secret in the namespace ImportedAdapter resolves, carrying both
// interrupt annotations so every test can prove they were cleared.
func cleanupSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cleanupClusterName,
			Labels:    map[string]string{capr.ClusterNameLabel: cleanupClusterName},
			Annotations: map[string]string{
				plan.PlanPausedAnnotation:   "true",
				plan.PlanCanceledAnnotation: "true",
			},
		},
		Type: plan.SecretTypeMachinePlan,
	}
}

// deletingOp is the operation as the API server presents it during termination: only metadata
// matters to CleanupOperation, and DeletionTimestamp is what the budget is measured from.
func deletingOp(deletedAt time.Time) metav1.Object {
	ts := metav1.NewTime(deletedAt)
	return &metav1.ObjectMeta{Namespace: "fleet-default", Name: "op-1", DeletionTimestamp: &ts}
}

func cleanupClusterRef() (*corev1.ObjectReference, *unstructured.Unstructured) {
	cluster := &unstructured.Unstructured{}
	cluster.SetAPIVersion("management.cattle.io/v3")
	cluster.SetKind("Cluster")
	cluster.SetName(cleanupClusterName)

	return &corev1.ObjectReference{
		APIVersion: "management.cattle.io/v3",
		Kind:       "Cluster",
		Name:       cleanupClusterName,
	}, cluster
}

// newCleanupScope wires a scope over an in-memory Secret client, returning the recorder for the
// Secrets written so ordering and content can be asserted.
func newCleanupScope(t *testing.T, beacons *cleanupBeacons, secrets ...*corev1.Secret) (CleanupScope, *[]*corev1.Secret) {
	t.Helper()

	ctrl := gomock.NewController(t)
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	written := &[]*corev1.Secret{}
	client.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		*written = append(*written, s.DeepCopy())
		return s, nil
	}).AnyTimes()
	client.EXPECT().List(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ns string, opts metav1.ListOptions) (*corev1.SecretList, error) {
			sel, err := labels.Parse(opts.LabelSelector)
			if err != nil {
				return nil, err
			}
			var out corev1.SecretList
			for _, s := range secrets {
				if s.Namespace == ns && sel.Matches(labels.Set(s.Labels)) {
					out.Items = append(out.Items, *s)
				}
			}
			return &out, nil
		}).AnyTimes()

	ref, cluster := cleanupClusterRef()

	return CleanupScope{
		LogPrefix:  "test",
		Object:     deletingOp(time.Now()),
		ClusterRef: ref,
		Dynamic:    &cleanupDynamic{obj: cluster},
		Secrets:    client,
		Beacons:    beacons,
		OwnerKey:   cleanupOwnerKey,
	}, written
}

// --- beacon release ---------------------------------------------------------------------------

// An operation deleted while it still owns the beacon is the whole reason this cleanup exists.
// Nothing else reclaims a foreign owner key, so without this every later day-2 operation on the
// cluster waits forever in AcquireBeacon.
func TestCleanupOperation_ReleasesAnOwnedBeacon(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	require.NoError(t, CleanupOperation(scope))

	require.Len(t, beacons.statusUpdates, 1, "the owned beacon must be released exactly once")
	released := beacons.statusUpdates[0]
	assert.Empty(t, released.Status.Owner, "a dead owner key blocks every later operation")
	assert.False(t, released.Status.Active)
	assert.Nil(t, released.Status.Delegates)
}

// A mid-chain slot must be surrendered without disturbing the owner: some other operation is
// legitimately driving the cluster and must keep it.
func TestCleanupOperation_RemovesOnlyItsOwnDelegateSlot(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon("someone-else", cleanupOwnerKey, "third-party")}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	require.NoError(t, CleanupOperation(scope))

	require.Len(t, beacons.statusUpdates, 1)
	released := beacons.statusUpdates[0]
	assert.Equal(t, "someone-else", released.Status.Owner, "the live owner must be left alone")
	assert.Equal(t, []string{"third-party"}, released.Status.Delegates)
}

// Cleaning up an operation that never acquired the beacon must cost one read and no write.
// Writing here would race whichever operation does hold it.
func TestCleanupOperation_LeavesABeaconItDoesNotHold(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon("someone-else", "third-party")}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	require.NoError(t, CleanupOperation(scope))
	assert.Empty(t, beacons.statusUpdates, "an operation that holds nothing must not write")
}

// A beacon that is already gone is nothing to clean, not a failure to retry.
func TestCleanupOperation_TreatsAMissingBeaconAsDone(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{}
	scope, written := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	require.NoError(t, CleanupOperation(scope))
	assert.Empty(t, beacons.statusUpdates)
	assert.Len(t, *written, 1, "the annotations must still be cleared")
}

// --- ordering ---------------------------------------------------------------------------------

// Releasing the beacon lets the next operation acquire the cluster. Doing that while a paused
// annotation is still on the Secrets would hand it a halted cluster, so the annotations must be
// cleared first and the release must not happen at all if clearing them failed.
func TestCleanupOperation_ClearsAnnotationsBeforeReleasingTheBeacon(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	secret := cleanupSecret("etcd-a")

	client.EXPECT().List(gomock.Any(), gomock.Any()).Return(
		&corev1.SecretList{Items: []corev1.Secret{*secret}}, nil).AnyTimes()
	client.EXPECT().Update(gomock.Any()).Return(nil, errors.New("etcdserver: request timed out")).AnyTimes()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	ref, cluster := cleanupClusterRef()

	err := CleanupOperation(CleanupScope{
		LogPrefix:  "test",
		Object:     deletingOp(time.Now()),
		ClusterRef: ref,
		Dynamic:    &cleanupDynamic{obj: cluster},
		Secrets:    client,
		Beacons:    beacons,
		OwnerKey:   cleanupOwnerKey,
	})

	require.Error(t, err, "a failed annotation clear must requeue while the budget remains")
	assert.Empty(t, beacons.statusUpdates,
		"releasing the beacon here would hand a halted cluster to the next operation")
}

func TestCleanupOperation_ClearsBothInterruptAnnotations(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, written := newCleanupScope(t, beacons, cleanupSecret("etcd-a"), cleanupSecret("etcd-b"))

	require.NoError(t, CleanupOperation(scope))

	require.Len(t, *written, 2, "every machine-plan secret in the cluster must be cleared")
	for _, s := range *written {
		assert.NotContains(t, s.Annotations, plan.PlanPausedAnnotation,
			"a stranded paused annotation halts every plan on the node with no CR left to explain why")
		assert.NotContains(t, s.Annotations, plan.PlanCanceledAnnotation)
	}
}

// --- budget -----------------------------------------------------------------------------------

// wrangler drops its finalizer only on a nil return, so a retryable failure must surface as an
// error for the deletion to be requeued.
func TestCleanupOperation_ReturnsTheErrorWhileTheBudgetRemains(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey), updateErr: errors.New("conflict")}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	assert.Error(t, CleanupOperation(scope))
}

// Past the budget the operation must be allowed to go. A deleting object that can never finish
// cleanup is a deletion trap, which is worse than the leftover state it is protecting.
func TestCleanupOperation_GivesUpOnceTheBudgetIsSpent(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey), updateErr: errors.New("conflict")}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))
	scope.Object = deletingOp(time.Now().Add(-2 * CleanupBudget))

	assert.NoError(t, CleanupOperation(scope),
		"holding a finalizer forever traps the object; the failure is logged instead")
}

// The budget is measured from DeletionTimestamp, not from when cleanup happened to start.
func TestCleanupBudgetExhausted(t *testing.T) {
	t.Parallel()

	assert.False(t, CleanupBudgetExhausted(&metav1.ObjectMeta{}),
		"an object that is not deleting has no budget to spend")
	assert.False(t, CleanupBudgetExhausted(deletingOp(time.Now())))
	assert.True(t, CleanupBudgetExhausted(deletingOp(time.Now().Add(-2*CleanupBudget))))
}

// --- nothing to clean -------------------------------------------------------------------------

// A cluster that no longer exists cannot hold a beacon or carry Secrets, so its operation must be
// allowed to delete immediately rather than retry to the end of the budget.
func TestCleanupOperation_TreatsAMissingClusterAsDone(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, written := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))
	scope.Dynamic = &cleanupDynamic{
		getErr: apierrors.NewNotFound(schema.GroupResource{Resource: "clusters"}, cleanupClusterName),
	}

	require.NoError(t, CleanupOperation(scope))
	assert.Empty(t, *written)
	assert.Empty(t, beacons.statusUpdates)
}

func TestCleanupOperation_NilClusterRefIsDone(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, written := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))
	scope.ClusterRef = nil

	require.NoError(t, CleanupOperation(scope))
	assert.Empty(t, *written)
	assert.Empty(t, beacons.statusUpdates)
}

// Beacon release is opt-in so a caller that has no beacon to release can leave the fields unset.
func TestCleanupOperation_SkipsBeaconReleaseWhenUnconfigured(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, written := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))
	scope.OwnerKey = ""

	require.NoError(t, CleanupOperation(scope))
	assert.Len(t, *written, 1, "annotation cleanup is unconditional")
	assert.Empty(t, beacons.statusUpdates)
}

// --- AfterRelease hook ------------------------------------------------------------------------

// The hook exists for beacon state a single operation type owns, such as EncryptionKeyRotation's
// owner-ref annotation. It must run on the same beacon and inside the same budget as the release,
// or that state escapes the retry and force-give-up rules the rest of cleanup obeys.
func TestCleanupOperation_AfterReleaseRunsOnTheReleasedBeacon(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	var seen *planv1alpha1.Beacon
	scope.AfterRelease = func(b *planv1alpha1.Beacon) error {
		seen = b
		return nil
	}

	require.NoError(t, CleanupOperation(scope))
	require.NotNil(t, seen, "the hook must run once the beacon has been released")
	assert.Equal(t, cleanupClusterName, seen.Name)
}

// A beacon that never existed carries no operation-type state either, so the hook must not run.
func TestCleanupOperation_AfterReleaseIsSkippedWithoutABeacon(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	called := false
	scope.AfterRelease = func(*planv1alpha1.Beacon) error {
		called = true
		return nil
	}

	require.NoError(t, CleanupOperation(scope))
	assert.False(t, called)
}

// Even when this operation holds nothing, the hook still runs: it decides for itself whether the
// state on the beacon is its own.
func TestCleanupOperation_AfterReleaseRunsWhenTheBeaconIsHeldByAnother(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon("someone-else")}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))

	called := false
	scope.AfterRelease = func(*planv1alpha1.Beacon) error {
		called = true
		return nil
	}

	require.NoError(t, CleanupOperation(scope))
	assert.True(t, called)
	assert.Empty(t, beacons.statusUpdates, "the live owner must still be left alone")
}

func TestCleanupOperation_AfterReleaseFailureObeysTheBudget(t *testing.T) {
	t.Parallel()

	beacons := &cleanupBeacons{obj: cleanupBeacon(cleanupOwnerKey)}
	scope, _ := newCleanupScope(t, beacons, cleanupSecret("etcd-a"))
	scope.AfterRelease = func(*planv1alpha1.Beacon) error { return errors.New("conflict") }

	assert.Error(t, CleanupOperation(scope), "a retryable failure must requeue the deletion")

	scope.Object = deletingOp(time.Now().Add(-2 * CleanupBudget))
	assert.NoError(t, CleanupOperation(scope), "past the budget the operation must be let go")
}

// --- operator message -------------------------------------------------------------------------

func TestCleanupFailureMessage(t *testing.T) {
	t.Parallel()

	cause := errors.New("boom")

	resolved := CleanupFailureMessage(cleanupClusterName, cleanupClusterName, cause)
	assert.Contains(t, resolved, CleanupBudget.String())
	assert.Contains(t, resolved, "boom")
	assert.Contains(t, resolved, plan.PlanPausedAnnotation)
	assert.Contains(t, resolved, plan.PlanCanceledAnnotation)
	assert.Contains(t, resolved, "kubectl patch beacon",
		"a held beacon is as damaging as a stranded annotation and must be discoverable too")

	// With no cluster the namespace is unknown, so the message has to fall back to a search.
	unresolved := CleanupFailureMessage("", "", cause)
	assert.Contains(t, unresolved, plan.SecretTypeMachinePlan)
	assert.Contains(t, unresolved, "kubectl get beacon -A")
}

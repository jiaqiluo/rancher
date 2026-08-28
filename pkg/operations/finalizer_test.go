package operations

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInterruptFinalizerHelpers(t *testing.T) {
	t.Parallel()

	t.Run("add is idempotent and reports whether it changed anything", func(t *testing.T) {
		obj := &corev1.Secret{}
		assert.True(t, AddInterruptFinalizer(obj))
		assert.Equal(t, []string{InterruptCleanupFinalizer}, obj.Finalizers)
		assert.False(t, AddInterruptFinalizer(obj))
		assert.Len(t, obj.Finalizers, 1)
	})

	t.Run("remove preserves other finalizers", func(t *testing.T) {
		obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Finalizers: []string{"other/one", InterruptCleanupFinalizer, "other/two"},
		}}
		assert.True(t, RemoveInterruptFinalizer(obj))
		assert.Equal(t, []string{"other/one", "other/two"}, obj.Finalizers)
		assert.False(t, RemoveInterruptFinalizer(obj))
	})

	t.Run("has", func(t *testing.T) {
		assert.False(t, HasInterruptFinalizer(&corev1.Secret{}))
		obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{InterruptCleanupFinalizer}}}
		assert.True(t, HasInterruptFinalizer(obj))
	})
}

func TestInterruptCleanupBudgetExhausted(t *testing.T) {
	t.Parallel()

	t.Run("an object that is not being deleted has spent nothing", func(t *testing.T) {
		assert.False(t, InterruptCleanupBudgetExhausted(&corev1.Secret{}))
	})

	t.Run("within budget", func(t *testing.T) {
		ts := metav1.NewTime(time.Now())
		assert.False(t, InterruptCleanupBudgetExhausted(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &ts},
		}))
	})

	t.Run("budget measured from DeletionTimestamp, not from now", func(t *testing.T) {
		ts := metav1.NewTime(time.Now().Add(-2 * InterruptCleanupBudget))
		assert.True(t, InterruptCleanupBudgetExhausted(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &ts},
		}))
	})
}

func TestInterruptCleanupFailureMessage(t *testing.T) {
	t.Parallel()

	cause := errors.New("no matches for kind")

	t.Run("names the namespace the collector actually reads, not the operation's", func(t *testing.T) {
		// The canonical UI-created operation lives in fleet-default and points at the
		// cluster-scoped mgmt v3 Cluster, but ImportedAdapter.BeaconRef puts its machine-plan
		// Secrets in c-m-xxxxx. Printing fleet-default would send the operator to an empty
		// namespace at exactly the moment Rancher has given up.
		got := InterruptCleanupFailureMessage("c-m-abcde", "c-m-abcde", cause)

		assert.Contains(t, got,
			"kubectl annotate secret -n c-m-abcde -l rke.cattle.io/cluster-name=c-m-abcde "+
				"plan.cattle.io/canceled- plan.cattle.io/paused-")
		assert.NotContains(t, got, "fleet-default")
		assert.Contains(t, got, cause.Error())
		assert.Contains(t, got, InterruptCleanupBudget.String())
	})

	t.Run("falls back to discovery when the cluster never resolved", func(t *testing.T) {
		for _, tc := range []struct{ namespace, clusterName string }{
			{"", ""},
			{"c-m-abcde", ""},
			{"", "c-m-abcde"},
		} {
			got := InterruptCleanupFailureMessage(tc.namespace, tc.clusterName, cause)
			assert.Contains(t, got,
				"kubectl get secret -A --field-selector type=rke.cattle.io/machine-plan",
				"a half-resolved scope is not a scope; guessing one sends the operator somewhere wrong")
			assert.NotContains(t, got, "-l rke.cattle.io/cluster-name=",
				"the selector must not be emitted with a hole in it")
		}
	})
}

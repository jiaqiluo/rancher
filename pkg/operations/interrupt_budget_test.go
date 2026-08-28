package operations

import (
	"errors"
	"fmt"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/plan"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// errTransient stands in for a read that failed and might succeed next time. plan.IsTransient
// classifies anything that is not a plan.CollectorError this way.
var errTransient = errors.New("etcdserver: request timed out")

// errPermanent is what an empty machine-plan Secret set arrives as: the AtLeast validator's
// non-transient plan.CollectorError. No amount of retrying turns it into a set.
var errPermanent = plan.CollectorError{
	Err:       fmt.Errorf("plan: expected at least 1 secrets matching any label, got 0"),
	Transient: false,
}

// interruptBudgetGVK returns a GVK unique to the calling test. The CancelPolicy registry is
// process-global, so tests that register a policy must not share a key or they race when run in
// parallel.
func interruptBudgetGVK(t *testing.T) schema.GroupVersionKind {
	t.Helper()
	return schema.GroupVersionKind{Group: "test.cattle.io", Version: "v1", Kind: t.Name()}
}

// pausedScope wires a scope whose operation is recorded as paused, which is the state
// handleResume runs in. Passing paused == false leaves the spec clean, which is what a resume is.
func pausedScope(t *testing.T, paused bool, secrets ...*corev1.Secret) (
	InterruptScope, *opv1alpha1.OperationSpec, *opv1alpha1.OperationStatus, *[]*corev1.Secret,
) {
	t.Helper()
	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress, secrets...)
	spec.Paused = paused
	opv1alpha1.PausedCondition.True(status)
	opv1alpha1.PausedCondition.Reason(status, opv1alpha1.PausedReason)
	return s, spec, status, written
}

func TestExpireFailedInterrupt_NoErrorOrTerminalIsIgnored(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		phase opv1alpha1.OperationPhase
		err   error
	}{
		{name: "no error", phase: opv1alpha1.OperationPhaseInProgress, err: nil},
		{name: "already terminal", phase: opv1alpha1.OperationPhaseFailed, err: errPermanent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, spec, status, written := newInterruptScope(t, tc.phase)
			spec.Cancel = true
			status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-time.Hour))
			before := status.DeepCopy()

			assert.False(t, ExpireFailedInterrupt(s, tc.err))
			assert.Equal(t, before, status, "nothing may be touched")
			assert.Empty(t, *written)
		})
	}
}

// --- pause ------------------------------------------------------------------------------------

func TestExpireFailedInterrupt_PauseIsReportedButNeverTerminated(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "transient", err: errTransient},
		{name: "permanent", err: errPermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, _, status, _ := pausedScope(t, true)

			assert.False(t, ExpireFailedInterrupt(s, tc.err),
				"a paused operation holds the cluster for as long as the user leaves it paused, "+
					"so a failing pause must not be overridden on a timer")
			assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase)
			assert.True(t, opv1alpha1.PausedCondition.IsTrue(status))
			assert.Equal(t, opv1alpha1.PauseFailedReason, opv1alpha1.PausedCondition.GetReason(status),
				"the failure must be on the status, not only in the log")
			assert.Contains(t, opv1alpha1.PausedCondition.GetMessage(status), tc.err.Error())
		})
	}
}

// --- resume -----------------------------------------------------------------------------------

func TestExpireFailedInterrupt_ResumeRetriesATransientFailure(t *testing.T) {
	t.Parallel()

	s, _, status, _ := pausedScope(t, false)

	assert.False(t, ExpireFailedInterrupt(s, errTransient),
		"every phase handler in these controllers retries a transient read forever; giving resume "+
			"a shorter fuse would fail operations during an API blip that nothing else fails on")
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase)
	assert.True(t, opv1alpha1.PausedCondition.IsTrue(status),
		"the operation really is still halted, and must keep saying so")
	assert.Equal(t, opv1alpha1.ResumeFailedReason, opv1alpha1.PausedCondition.GetReason(status))
}

func TestExpireFailedInterrupt_ResumeFailsTheOperationOnAPermanentFailure(t *testing.T) {
	t.Parallel()

	// Paused annotation already on the Secret from the pause that is now being withdrawn.
	secret := planStateSecret("a", plan.PlanStateInProgress, "")
	secret.Annotations[plan.PlanPausedAnnotation] = "true"

	s, _, status, written := pausedScope(t, false, secret)

	assert.True(t, ExpireFailedInterrupt(s, errPermanent),
		"a user who resumes is asking the operation to proceed; halting forever on a failure that "+
			"cannot resolve is the opposite of that")
	assert.Equal(t, opv1alpha1.OperationPhaseFailed, status.Phase,
		"a terminal phase is what lets the caller's phase handler release the beacon")
	assert.Equal(t, opv1alpha1.ResumeFailedReason, opv1alpha1.FailedCondition.GetReason(status))
	assert.True(t, opv1alpha1.PausedCondition.IsFalse(status),
		"an operation reporting Failed and Paused at once contradicts itself")

	if assert.Len(t, *written, 1, "the interrupt must be lifted before the beacon is released") {
		assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation)
		assert.NotContains(t, (*written)[0].Annotations, plan.PlanCanceledAnnotation)
	}
}

// --- cancel -----------------------------------------------------------------------------------

func TestExpireFailedInterrupt_StartsTheClockOnTheFirstFailure(t *testing.T) {
	t.Parallel()

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	spec.Cancel = true

	assert.False(t, ExpireFailedInterrupt(s, errTransient),
		"the budget cannot already be spent on the very first failure")
	assert.False(t, status.CancelRequestedAt.IsZero(),
		"a failure before HandleInterrupt reaches its own stamp must still start the clock, "+
			"otherwise the budget has nothing to measure and never expires")
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase)
	assert.Equal(t, opv1alpha1.CancelEvaluationFailedReason,
		opv1alpha1.CanceledCondition.GetReason(status))
}

func TestExpireFailedInterrupt_HoldsWhileTheBudgetLasts(t *testing.T) {
	t.Parallel()

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	spec.Cancel = true
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget / 2))

	assert.False(t, ExpireFailedInterrupt(s, errTransient))
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase)
	assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(status), "retrying")
}

func TestExpireFailedInterrupt_CancelGivesUpImmediatelyOnAPermanentFailure(t *testing.T) {
	t.Parallel()

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	spec.Cancel = true

	assert.True(t, ExpireFailedInterrupt(s, errPermanent),
		"waiting out a 15m budget for an answer that will never change only holds the beacon longer")
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
	assert.NotContains(t, opv1alpha1.CanceledCondition.GetMessage(status), "within",
		"the message must not claim a budget elapsed when the failure was decided immediately")
}

func TestExpireFailedInterrupt_DrivesTerminalOnceSpent(t *testing.T) {
	t.Parallel()

	// A machine-plan Secret whose failure-count cannot be parsed makes Store.Status error on every
	// reconcile forever, so retrying alone never releases the beacon. It is classified transient —
	// it is not a CollectorError — which is exactly why the budget has to exist.
	secret := planStateSecret("a", plan.PlanStateInProgress, "")
	secret.Data["failed-checksum"] = []byte(plan.PlanHash(secret.Data["plan"]))
	secret.Data["failure-count"] = []byte("not-a-number")

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress, secret)
	spec.Cancel = true

	_, err := HandleInterrupt(s)
	require.Error(t, err, "the corrupt Secret must make HandleInterrupt fail")
	require.True(t, plan.IsTransient(err), "and it must be classified as worth retrying")
	assert.False(t, status.CancelRequestedAt.IsZero())

	// Backdate past the budget to stand in for the failure repeating for its whole duration.
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget - time.Second))

	assert.True(t, ExpireFailedInterrupt(s, err))
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase,
		"a terminal phase is what lets the caller's phase handler release the beacon")
	assert.Equal(t, opv1alpha1.CancelEvaluationFailedReason, opv1alpha1.CanceledCondition.GetReason(status))
	assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(status), "not-a-number",
		"the message must carry the underlying failure so the stall is diagnosable")
}

// --- lifting the interrupt before the beacon goes -----------------------------------------------

func TestExpireFailedInterrupt_LiftsThePauseButKeepsTheCancelOnTheSecrets(t *testing.T) {
	t.Parallel()

	// The operation was paused first, so the agents are halted by the paused annotation. Releasing
	// the beacon without lifting it strands every agent on the cluster.
	secret := planStateSecret("a", plan.PlanStateInProgress, "")
	secret.Annotations[plan.PlanPausedAnnotation] = "true"

	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress, secret)
	spec.Cancel = true
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget - time.Second))

	require.True(t, ExpireFailedInterrupt(s, errTransient))
	if assert.Len(t, *written, 1) {
		assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation,
			"leaving this behind halts every agent on the cluster until an unrelated AssignPlan clears it")
		assert.Equal(t, "true", (*written)[0].Annotations[plan.PlanCanceledAnnotation],
			"the canceled annotation is what the ordinary cancel path writes; diverging from it here "+
				"would re-arm a plan Rancher has just abandoned")
	}
	assert.NotContains(t, opv1alpha1.CanceledCondition.GetMessage(status), "may still be halted")
}

func TestExpireFailedInterrupt_SaysSoWhenItCannotLiftTheInterrupt(t *testing.T) {
	t.Parallel()

	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	s.Secrets = func() ([]*corev1.Secret, error) { return nil, errTransient }
	spec.Cancel = true
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget - time.Second))

	require.True(t, ExpireFailedInterrupt(s, errTransient))
	assert.Empty(t, *written)
	assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(status), "may still be halted",
		"leftover interrupt state must be discoverable rather than silent")
}

// --- recovery reporting -------------------------------------------------------------------------

func TestExpireFailedInterrupt_ReportsRecoveryConservatively(t *testing.T) {
	t.Parallel()

	gvk := interruptBudgetGVK(t)
	RegisterCancelPolicy(gvk, CancelPolicy{RequiresRecovery: true, RecoveryMessage: "recover me"})

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	s.GVK = gvk
	spec.Cancel = true
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget - time.Second))

	assert.True(t, ExpireFailedInterrupt(s, errTransient))
	assert.True(t, status.AnyNodeMutationObserved,
		"Rancher never found out what the nodes did, and under a RequiresRecovery policy that has "+
			"to resolve towards telling the user to check")
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsTrue(status))
	assert.Contains(t, opv1alpha1.RecoveryRequiredCondition.GetMessage(status), "recover me")
}

func TestExpireFailedInterrupt_NoRecoveryForAHarmlessType(t *testing.T) {
	t.Parallel()

	gvk := interruptBudgetGVK(t)
	RegisterCancelPolicy(gvk, CancelPolicy{RequiresRecovery: false})

	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	s.GVK = gvk
	spec.Cancel = true
	status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-InterruptFailureBudget - time.Second))

	assert.True(t, ExpireFailedInterrupt(s, errTransient))
	assert.False(t, status.AnyNodeMutationObserved,
		"the flag is evidence, not a placeholder; a type with nothing to recover from must not "+
			"have it fabricated")
	assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(status))
}

// --- no write storm ------------------------------------------------------------------------------

func TestExpireFailedInterrupt_RepeatingTheSameFailureLeavesTheStatusAlone(t *testing.T) {
	t.Parallel()

	s, _, status, _ := pausedScope(t, true)

	require.False(t, ExpireFailedInterrupt(s, errTransient))
	first := status.DeepCopy()

	require.False(t, ExpireFailedInterrupt(s, errTransient))
	assert.True(t, equality.Semantic.DeepEqual(first, status),
		"the controllers re-enqueue every 5s; a report that rewrote the condition each time would "+
			"turn a stuck interrupt into an endless stream of status writes, and is also what the "+
			"log gate keys off")
}

package operations

import (
	"fmt"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/plan"
	"github.com/rancher/wrangler/v3/pkg/condition"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InterruptFailureBudget limits how long cancellation evaluation may fail before giving up.
// It is longer than CancelConfirmationTimeout because this budget covers hard failures.
// The controller requeues every 5s, so the budget allows many retries before expiry.
// Make it a var so tests may shorten it.
var InterruptFailureBudget = 15 * time.Minute

// ExpireFailedInterrupt records a HandleInterrupt failure in status and decides if the operation
// must become terminal instead of retrying. Return true when it transitions the operation.
//
// Call this for every HandleInterrupt error. HandleInterrupt returns handled==true with the
// error so the caller skips normal phase dispatch. Without recording failures, an operation may
// hold the cluster beacon indefinitely while the controller silently retries.
//
// Behavior varies by requested interrupt:
// - Cancel: bounded. If the budget is exhausted, move to Canceled to release the beacon.
// - Pause: unbounded. Respect the user's pause; do not override it on a timer.
// - Resume: bounded only if the failure is permanent. Transient read failures are retried.
//
// "Cannot resolve" means plan.IsTransient returns false. Non-transient errors should not be retried.
func ExpireFailedInterrupt(s InterruptScope, err error) bool {
	if err == nil || IsTerminal(s.Status.Phase) {
		return false
	}

	permanent := !plan.IsTransient(err)

	switch {
	case s.Spec.Cancel:
		return s.cancelFailed(err, permanent)

	case s.Spec.Paused:
		s.report(opv1alpha1.PausedCondition, true, opv1alpha1.PauseFailedReason,
			fmt.Sprintf("pause could not be evaluated: %v", err))
		return false

	case opv1alpha1.PausedCondition.IsTrue(s.Status):
		return s.resumeFailed(err, permanent)
	}

	// Unreachable: HandleInterrupt only returns an error from one of the three branches above.
	// Logged rather than dropped so a future branch that forgets to report is not silent.
	logrus.Errorf("[%s] %s/%s: handling interrupt: %v", s.LogPrefix, s.Namespace, s.Name, err)
	return false
}

func (s InterruptScope) cancelFailed(err error, permanent bool) bool {
	if s.Status.CancelRequestedAt.IsZero() {
		// HandleInterrupt stamps this itself, but only once it gets past listing the machine-plan
		// Secrets — which is exactly the step that can fail. Stamp it here too so a failure on that
		// very first step still starts the clock instead of leaving the budget with nothing to
		// measure.
		s.Status.CancelRequestedAt = metav1.Now()
	}

	if !permanent && time.Since(s.Status.CancelRequestedAt.Time) < InterruptFailureBudget {
		s.report(opv1alpha1.CanceledCondition, true, opv1alpha1.CancelEvaluationFailedReason,
			fmt.Sprintf("cancellation could not be evaluated, retrying: %v", err))
		return false
	}

	within := ""
	if !permanent {
		within = fmt.Sprintf(" within %s", InterruptFailureBudget)
	}
	message := fmt.Sprintf("cancellation could not be evaluated%s, so it was abandoned rather than "+
		"hold the cluster indefinitely; last error: %v", within, err)
	// keepCanceled: matches what the ordinary cancel path writes, so this does not diverge from
	// finishCancel. Only the paused annotation is dangerous to leave behind.
	message += s.releaseInterrupt(true, err)

	policy := CancelPolicyFor(s.GVK)
	if policy.RequiresRecovery {
		// Mirrors the legacy-plan-flow branch in latchMutationEvidence: under a policy that
		// requires recovery, "could not tell" has to resolve to "assume it happened".
		s.Status.AnyNodeMutationObserved = true
	}

	s.SetPhase(opv1alpha1.OperationPhaseCanceled)
	s.report(opv1alpha1.CanceledCondition, true, opv1alpha1.CancelEvaluationFailedReason, message)
	s.setRecoveryCondition(policy, nil)
	return true
}

func (s InterruptScope) resumeFailed(err error, permanent bool) bool {
	if !permanent {
		s.report(opv1alpha1.PausedCondition, true, opv1alpha1.ResumeFailedReason,
			fmt.Sprintf("resume could not be applied, retrying; the operation is still halted: %v", err))
		return false
	}

	message := fmt.Sprintf("resume could not be applied and the failure will not resolve on retry, "+
		"so the operation was failed rather than left halted holding the cluster; last error: %v", err)
	message += s.releaseInterrupt(false, err)

	s.SetPhase(opv1alpha1.OperationPhaseFailed)
	s.report(opv1alpha1.FailedCondition, true, opv1alpha1.ResumeFailedReason, message)
	// An operation reporting Failed and Paused at once contradicts itself, and nothing is holding
	// it any more.
	opv1alpha1.PausedCondition.False(s.Status)
	opv1alpha1.PausedCondition.Reason(s.Status, opv1alpha1.NotPausedReason)
	opv1alpha1.PausedCondition.Message(s.Status, "")
	return true
}

// Without this cleanup, giving up on a cancellation that followed a pause could release the beacon
// while every agent in the cluster remains halted by a leftover paused annotation, waiting for an
// unrelated AssignPlan to clear it. This is the same hazard guarded against by handleCancel's
// Pending branch, reached from the opposite direction.
//
// keepCanceled preserves the canceled annotation, matching what the normal cancel path writes.
// Clearing it here would make this path inconsistent with finishCancel without providing any
// benefit. Re-arming a node whose plan Rancher has abandoned is not a risk worth taking merely for
// cleanup.
//
// Returns a suffix for the condition message when cleanup fails, making leftover interrupt state
// visible rather than silent. Listing the Secrets may itself be the failure, which is expected for
// a best-effort cleanup. For that reason, the cause is omitted from the suffix when it merely
// repeats the original failure.
func (s InterruptScope) releaseInterrupt(keepCanceled bool, cause error) string {
	secrets, err := s.Secrets()
	if err == nil {
		_, err = SyncInterruptAnnotations(s.SecretClient, secrets, false, keepCanceled)
	}
	if err == nil {
		return ""
	}

	suffix := "; the interrupt annotations could not be lifted from the machine-plan secrets, so " +
		"agents on this cluster may still be halted"
	if cause == nil || err.Error() != cause.Error() {
		suffix += fmt.Sprintf(" (%v)", err)
	}
	return suffix
}

// report writes a condition and logs the message only when it is not already the one on the status.
func (s InterruptScope) report(cond condition.Cond, status bool, reason, message string) {
	if cond.GetMessage(s.Status) != message || cond.GetReason(s.Status) != reason {
		logrus.Errorf("[%s] %s/%s: %s", s.LogPrefix, s.Namespace, s.Name, message)
	}

	// Writing an unchanged condition is free: wrangler's setters only stamp LastUpdateTime when the
	// value actually changes, so a repeated report leaves the status byte-identical and the caller's
	// no-op re-enqueue path intact.
	cond.SetStatusBool(s.Status, status)
	cond.Reason(s.Status, reason)
	cond.Message(s.Status, message)
}

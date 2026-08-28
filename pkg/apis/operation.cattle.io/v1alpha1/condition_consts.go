package v1alpha1

import (
	"strings"

	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	"github.com/rancher/wrangler/v3/pkg/condition"
)

var (
	// PendingCondition represents the condition state for a task or process that is awaiting execution or resolution.
	PendingCondition = condition.Cond("Pending")

	// InProgressCondition represents the condition state for a task or process that is currently in progress or being executed.
	InProgressCondition = condition.Cond("InProgress")

	// SucceededCondition represents the condition state for a task or process that completed successfully.
	SucceededCondition = condition.Cond("Succeeded")

	// FailedCondition represents the condition state for a task or process that has failed to complete successfully.
	FailedCondition = condition.Cond("Failed")

	// CanceledCondition represents the condition state for a task or process that has been canceled or terminated.
	CanceledCondition = condition.Cond("Canceled")

	// PausedCondition represents the condition state for a task or process that has been paused.
	PausedCondition = condition.Cond("Paused")

	// RecoveryRequiredCondition reports that a canceled operation may have left the cluster in a
	// state requiring manual recovery. Set it when the CancelPolicy requires recovery and mutation
	// was observed, or when a node reported processes might still be running.
	RecoveryRequiredCondition = condition.Cond("RecoveryRequired")
)

const (
	// ClusterNotFoundReason surfaces when an operation fails because the cluster is not found.
	ClusterNotFoundReason = "ClusterNotFound"

	// BeaconLostReason surfaces when an operation fails because the beacon is lost.
	BeaconLostReason = "BeaconLost"

	// UnknownStepReason surfaces when an operation fails because the step is unknown.
	UnknownStepReason = "UnknownStep"

	// UnknownPhaseReason surfaces when an operation fails because the phase is unknown.
	UnknownPhaseReason = "UnknownPhase"

	// WaitingForRegistrationReason surfaces when an operation is waiting for registration.
	WaitingForRegistrationReason = "WaitingForRegistration"

	// WaitingForBeaconReason surfaces when an operation is waiting to acquire the beacon.
	WaitingForBeaconReason = "WaitingForBeacon"

	// WaitingForPlanAppliedReason surfaces when an operation is waiting for a node plan to be applied.
	WaitingForPlanAppliedReason = "WaitingForPlanApplied"

	WaitingForDelegateReason = "WaitingForDelegate"

	PlanFailedReason = "PlanFailed"

	// FinishedReason surfaces when an operation has reached a terminal state (success/failure).
	FinishedReason = "Finished"

	// NotFailedReason surfaces when an operation has not failed.
	NotFailedReason = "NotFailed"

	// NotSuccessfulReason surfaces when an operation has not completed successfully.
	NotSuccessfulReason = "NotSuccessful"

	// InProgressReason surfaces when an operation is currently in progress.
	InProgressReason = "InProgress"

	// PausedReason surfaces when an operation is paused.
	PausedReason = "Paused"

	// NotPausedReason surfaces when an operation is not paused.
	NotPausedReason = "NotPaused"

	// WaitingForSuitableLeaderReason surfaces when no suitable control-plane leader can be
	// elected for encryption key rotation yet. The operation will retry automatically.
	WaitingForSuitableLeaderReason = "WaitingForSuitableLeader"

	// WaitingForEncryptionKeyRotationReason surfaces when the rotate-keys plan has been applied
	// but the runtime secrets-encrypt status has not yet confirmed reencrypt_finished.
	WaitingForEncryptionKeyRotationReason = "WaitingForEncryptionKeyRotation"

	PreflightCheckFailedReason = "PreflightCheckFailed"

	// CancelRequestedReason surfaces after cancellation has been propagated to the machine-plan
	// Secrets but before every node has confirmed it. The operation remains InProgress during this
	// period because marking it Canceled would release the beacon (and, for EncryptionKeyRotation, the CAPI
	// cluster pause) while a node could still be executing an instruction.
	CancelRequestedReason = "CancelRequested"

	// CanceledReason surfaces when every node confirmed the cancellation, or when the operation was
	// canceled before any plan was dispatched.
	CanceledReason = "Canceled"

	// AgentConfirmationTimeoutReason surfaces when the operation moved to Canceled because the
	// confirmation window elapsed rather than because every node confirmed. The message names the
	// nodes that never confirmed.
	AgentConfirmationTimeoutReason = "AgentConfirmationTimeout"

	// CancelEvaluationFailedReason surfaces when Rancher cannot evaluate the cancellation at all,
	// for example because a machine-plan Secret is corrupt or Secret reads continue to fail. While the
	// interrupt failure budget remains, it is a progress signal for the retrying cancellation. Once
	// the budget is exhausted, or immediately when the failure is known to be unrecoverable, it explains
	// why the operation transitions to Canceled instead of holding the cluster's beacon indefinitely.
	// In the terminal case, recovery is reported conservatively because Rancher never established what
	// the nodes actually did.
	CancelEvaluationFailedReason = "CancelEvaluationFailed"

	// PauseFailedReason surfaces when Rancher cannot determine whether the pause succeeded. The
	// operation continues retrying. A paused operation holds the cluster for as long as the user leaves
	// it paused, so a failed pause is no worse than a successful one.
	PauseFailedReason = "PauseFailed"

	// ResumeFailedReason surfaces when Rancher cannot lift an interrupt the user has withdrawn.
	// On PausedCondition it means the operation is still halted and Rancher is retrying;
	// on FailedCondition it means the failure cannot resolve on retry and the operation was failed
	// rather than left halted holding the cluster.
	ResumeFailedReason = "ResumeFailed"

	// LegacyPlanFlowReason surfaces when one or more nodes could not confirm the cancellation at
	// all because their machine-plan Secret has no plan-state key. Those agents are on the legacy
	// checksum flow and ignore the interrupt annotations entirely, so Rancher asked and has no way
	// to know whether anything listened.
	LegacyPlanFlowReason = "LegacyPlanFlow"

	// PauseRequestedReason surfaces while the pause annotation has been written but at least one
	// node has not yet reported a paused or terminal plan state. PausedReason replaces it once
	// every node has demonstrably stopped.
	PauseRequestedReason = "PauseRequested"

	// RecoveryRequiredReason surfaces when a canceled operation of a RequiresRecovery type observed
	// real mutation before the cancel landed.
	RecoveryRequiredReason = "RecoveryRequired"

	// NotRecoveryRequiredReason surfaces when a canceled operation left nothing to recover from.
	NotRecoveryRequiredReason = "NotRecoveryRequired"

	// TerminationIncompleteReason surfaces when a node reported that processes from an interrupted
	// instruction's process tree were still running when the agent gave up on them, meaning the
	// node is not necessarily quiescent. Worth reporting even for an operation type whose
	// CancelPolicy does not require recovery.
	TerminationIncompleteReason = "TerminationIncomplete"

	// InterruptCleanupIncompleteReason surfaces when the deletion finalizer exhausted its retry
	// budget and dropped itself anyway, leaving interrupt annotations on machine-plan Secrets. The
	// message names the namespace and Secrets so the leftover state is discoverable.
	InterruptCleanupIncompleteReason = "InterruptCleanupIncomplete"
)

func WaitingForDelegateMessage(beacon *planv1alpha1.Beacon) string {
	if beacon == nil {
		return ""
	}

	if len(beacon.Status.Delegates) == 0 {
		return ""
	}

	return strings.Join(beacon.Status.Delegates, ",")
}

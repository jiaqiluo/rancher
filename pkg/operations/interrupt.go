package operations

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SyncInterruptAnnotations converges the two interrupt annotations on machine-plan Secrets.
// It returns true when it wrote any Secret.
// Cancel wins over pause. Cancel clears pause so the agent sees one source of truth.
// Clearing removes the annotation key rather than writing "false".
// The agent treats absent and "false" the same and never modifies these annotations.
// Any non-boolean value is overwritten to avoid agent confusion.
// The function is idempotent per Secret. The caller may retry on error.
// The function DeepCopies Secrets before writing them.
func SyncInterruptAnnotations(client corecontrollers.SecretClient, secrets []*corev1.Secret, paused, canceled bool) (bool, error) {
	wantPaused := paused && !canceled

	var (
		changed bool
		errs    []error
	)

	for _, secret := range secrets {
		if secret == nil {
			continue
		}

		updated := secret.DeepCopy()
		dirty := setInterruptAnnotation(updated, plan.PlanPausedAnnotation, wantPaused)
		dirty = setInterruptAnnotation(updated, plan.PlanCanceledAnnotation, canceled) || dirty
		if !dirty {
			continue
		}

		if _, err := client.Update(updated); err != nil {
			errs = append(errs, fmt.Errorf("syncing interrupt annotations on secret %s/%s: %w",
				secret.Namespace, secret.Name, err))
			continue
		}
		changed = true
	}

	return changed, errors.Join(errs...)
}

// setInterruptAnnotation converges a single annotation and reports whether it changed.
func setInterruptAnnotation(secret *corev1.Secret, key string, want bool) bool {
	current, present := secret.Annotations[key]
	switch {
	case want && current != "true":
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[key] = "true"
		return true
	case !want && present:
		delete(secret.Annotations, key)
		return true
	}
	return false
}

// CancelPolicy declares what cancellation means for an operation type.
// It describes the policy that applies when a user cancels an in-flight operation.
type CancelPolicy struct {
	// RequiresRecovery is true when a partial run can leave the cluster needing manual recovery.
	// It is false for operation types whose partial runs have no lasting effect.
	RequiresRecovery bool

	// RecoveryMessage appears on RecoveryRequiredCondition when recovery is required.
	RecoveryMessage string
}

// defaultCancelPolicy applies to unregistered operation types.
// The default requires recovery to force operator attention.
var defaultCancelPolicy = CancelPolicy{
	RequiresRecovery: true,
	RecoveryMessage: "this operation type did not declare a cancel policy; " +
		"inspect the cluster before running further day-2 operations",
}

var (
	// cancelPolicies maps GVK.String() to a CancelPolicy.
	// Controllers register their policy at init time.
	cancelPolicies   = map[string]CancelPolicy{}
	cancelPoliciesMu sync.RWMutex
)

// RegisterCancelPolicy registers a CancelPolicy for a GroupVersionKind.
// Call it from the controller package's init function.
func RegisterCancelPolicy(gvk schema.GroupVersionKind, policy CancelPolicy) {
	cancelPoliciesMu.Lock()
	defer cancelPoliciesMu.Unlock()
	cancelPolicies[gvk.String()] = policy
}

// CancelPolicyFor returns the registered policy or the default if none is set.
func CancelPolicyFor(gvk schema.GroupVersionKind) CancelPolicy {
	cancelPoliciesMu.RLock()
	defer cancelPoliciesMu.RUnlock()
	if policy, ok := cancelPolicies[gvk.String()]; ok {
		return policy
	}
	return defaultCancelPolicy
}

// CancelConfirmationTimeout limits how long the controller waits for node confirmations.
// Make it a var so tests may shorten it.
var CancelConfirmationTimeout = 5 * time.Minute

// InterruptScope holds the data HandleInterrupt needs from each controller.
type InterruptScope struct {
	// LogPrefix is the controller log tag, for example "etcdsnapshotsave".
	LogPrefix string

	// Namespace and Name identify the operation.
	Namespace string
	Name      string

	// GVK is the operation type's own GroupVersionKind. It keys the CancelPolicy registry.
	GVK schema.GroupVersionKind

	// Spec and Status are the shared operation structs. Status is mutated in place.
	Spec   *opv1alpha1.OperationSpec
	Status *opv1alpha1.OperationStatus

	// SetPhase sets the concrete status phase and updates LastUpdated.
	SetPhase func(opv1alpha1.OperationPhase)

	// Secrets returns all machine-plan Secrets for the cluster, freshly listed.
	// The list must include every machine-plan Secret for the cluster.
	Secrets func() ([]*corev1.Secret, error)

	// SecretClient writes interrupt annotations.
	SecretClient corecontrollers.SecretClient

	// Store reads per-node plan status without writing.
	Store *plan.Store
}

// HandleInterrupt enforces and reports Spec.Paused and Spec.Cancel.
// It returns true when the caller must return without running the phase handler.
// Cancel takes precedence over pause. Cancel writes canceled and clears paused.
func HandleInterrupt(s InterruptScope) (bool, error) {
	// A terminal operation has nothing left to interrupt. Its phase handler releases the beacon
	// and unpauses the CAPI cluster when applicable.
	if IsTerminal(s.Status.Phase) {
		return false, nil
	}

	switch {
	case s.Spec.Cancel:
		return s.handleCancel()
	case s.Spec.Paused:
		return s.handlePause()
	default:
		return s.handleResume()
	}
}

func (s InterruptScope) handleCancel() (bool, error) {
	wasPaused := opv1alpha1.PausedCondition.IsTrue(s.Status)
	// Cancel takes precedence over pause. Clear the paused record.
	opv1alpha1.PausedCondition.False(s.Status)
	opv1alpha1.PausedCondition.Reason(s.Status, opv1alpha1.NotPausedReason)
	opv1alpha1.PausedCondition.Message(s.Status, "")

	// If Cancel occurs while Pending, no machine-plan Secret carries a plan yet.
	// Move the operation straight to Cancel. RecoveryRequired cannot apply.
	if s.Status.Phase == opv1alpha1.OperationPhasePending {
		if wasPaused {
			secrets, err := s.Secrets()
			if err != nil {
				return true, err
			}
			if _, err := SyncInterruptAnnotations(s.SecretClient, secrets, false, false); err != nil {
				return true, err
			}
		}

		logrus.Infof("[%s] %s/%s: canceled before any plan was dispatched", s.LogPrefix, s.Namespace, s.Name)
		s.SetPhase(opv1alpha1.OperationPhaseCanceled)
		opv1alpha1.CanceledCondition.True(s.Status)
		opv1alpha1.CanceledCondition.Reason(s.Status, opv1alpha1.CanceledReason)
		opv1alpha1.CanceledCondition.Message(s.Status, "canceled before any plan was dispatched")
		return true, nil
	}

	secrets, err := s.Secrets()
	if err != nil {
		return true, err
	}

	if s.Status.CancelRequestedAt.IsZero() {
		s.Status.CancelRequestedAt = metav1.Now()
	}

	if _, err := SyncInterruptAnnotations(s.SecretClient, secrets, false, true); err != nil {
		return true, err
	}

	policy := CancelPolicyFor(s.GVK)
	legacy, err := s.latchMutationEvidence(secrets, policy)
	if err != nil {
		return true, err
	}

	unconfirmed, err := s.unconfirmedNodes(secrets)
	if err != nil {
		return true, err
	}

	switch {
	case len(unconfirmed) == 0:
		s.finishCancel(opv1alpha1.CanceledReason, "cancellation confirmed by every node", policy, legacy)

	case time.Since(s.Status.CancelRequestedAt.Time) >= CancelConfirmationTimeout:
		// A timed-out confirmation still lands in Canceled, not Failed: the user asked to cancel
		// and nothing about the operation itself failed, only the confirmation didn't arrive.
		reason := opv1alpha1.AgentConfirmationTimeoutReason
		message := fmt.Sprintf("cancellation not confirmed within %s by: %s",
			CancelConfirmationTimeout, strings.Join(unconfirmed, ", "))
		if len(legacy) > 0 && len(legacy) == len(unconfirmed) {
			// Every node that failed to confirm did so because it reports no plan-state at all,
			// which means the agent is on the legacy checksum flow and ignores the interrupt
			// annotations entirely. That is a different statement from "the agent was slow", and
			// the honest report is that Rancher asked and cannot know whether anything listened.
			reason = opv1alpha1.LegacyPlanFlowReason
			message = fmt.Sprintf("cancellation could not be enforced on %s: no plan-state reported, "+
				"so the agent is on the legacy checksum flow and ignores the interrupt annotations",
				strings.Join(legacy, ", "))
		}
		logrus.Warnf("[%s] %s/%s: %s", s.LogPrefix, s.Namespace, s.Name, message)
		s.finishCancel(reason, message, policy, legacy)

	default:
		// Stay InProgress. IsTerminal(Canceled) is what releases the beacon and the
		// Adapter.PauseCluster lock; releasing either while a node might still be mid-instruction
		// would let something else start acting on the cluster concurrently.
		opv1alpha1.CanceledCondition.True(s.Status)
		opv1alpha1.CanceledCondition.Reason(s.Status, opv1alpha1.CancelRequestedReason)
		opv1alpha1.CanceledCondition.Message(s.Status,
			fmt.Sprintf("waiting for cancellation to be confirmed by: %s", strings.Join(unconfirmed, ", ")))
	}

	return true, nil
}

func (s InterruptScope) finishCancel(reason, message string, policy CancelPolicy, legacy []string) {
	s.SetPhase(opv1alpha1.OperationPhaseCanceled)
	opv1alpha1.CanceledCondition.True(s.Status)
	opv1alpha1.CanceledCondition.Reason(s.Status, reason)
	opv1alpha1.CanceledCondition.Message(s.Status, message)
	s.setRecoveryCondition(policy, legacy)
}

func (s InterruptScope) handlePause() (bool, error) {
	secrets, err := s.Secrets()
	if err != nil {
		return true, err
	}

	if _, err := SyncInterruptAnnotations(s.SecretClient, secrets, true, false); err != nil {
		return true, err
	}

	// Latch mutation evidence during a pause. Flags are sticky and persist observed progress.
	if _, err := s.latchMutationEvidence(secrets, CancelPolicyFor(s.GVK)); err != nil {
		return true, err
	}

	unstopped, err := s.unstoppedNodes(secrets)
	if err != nil {
		return true, err
	}

	opv1alpha1.PausedCondition.True(s.Status)
	if len(unstopped) == 0 {
		opv1alpha1.PausedCondition.Reason(s.Status, opv1alpha1.PausedReason)
		opv1alpha1.PausedCondition.Message(s.Status, "operation is paused and every node has stopped")
		return true, nil
	}

	// Unlike cancel, pause only reports. It does not time out waiting for confirmation.
	opv1alpha1.PausedCondition.Reason(s.Status, opv1alpha1.PauseRequestedReason)
	opv1alpha1.PausedCondition.Message(s.Status,
		fmt.Sprintf("pause requested; waiting for: %s", strings.Join(unstopped, ", ")))
	return true, nil
}

func (s InterruptScope) handleResume() (bool, error) {
	if !opv1alpha1.PausedCondition.IsTrue(s.Status) {
		return false, nil
	}

	secrets, err := s.Secrets()
	if err != nil {
		return true, err
	}

	// Clear annotations on resume rather than via AssignPlan. AssignPlan touches a subset of
	// Secrets and would leave others paused.
	changed, err := SyncInterruptAnnotations(s.SecretClient, secrets, false, false)
	if err != nil {
		return true, err
	}

	opv1alpha1.PausedCondition.False(s.Status)
	opv1alpha1.PausedCondition.Reason(s.Status, opv1alpha1.NotPausedReason)
	opv1alpha1.PausedCondition.Message(s.Status, "")

	if changed {
		logrus.Infof("[%s] %s/%s: resumed; cleared interrupt annotations", s.LogPrefix, s.Namespace, s.Name)
	}
	return changed, nil
}

// latchMutationEvidence sets sticky status flags from agent plan-progress reports.
// It returns node names that report no state due to the legacy checksum flow.
// Once set, the sticky flags are never cleared.
// The agent reports completed instruction counts and terminationIncomplete. Rancher lacks
// equivalent signals, so the agent is the source of truth.
func (s InterruptScope) latchMutationEvidence(secrets []*corev1.Secret, policy CancelPolicy) ([]string, error) {
	var legacy []string

	for _, secret := range secrets {
		if secret == nil {
			continue
		}

		// A Secret with no plan carries no evidence about the operation.
		if !hasPlan(secret) {
			continue
		}

		st, err := s.Store.Status(secret)
		if err != nil {
			return nil, err
		}

		if st.State == "" {
			legacy = append(legacy, nodeName(secret))
			if policy.RequiresRecovery {
				s.Status.AnyNodeMutationObserved = true
			}
			continue
		}
		if st.State == plan.PlanStateSucceeded || st.State == plan.PlanStateFailed {
			// A terminal plan state means mutation happened or failed during mutation.
			s.Status.AnyNodeMutationObserved = true
		}

		if st.Checkpoint != nil {
			if st.Checkpoint.Completed > 0 {
				s.Status.AnyNodeMutationObserved = true
			}
			if st.Checkpoint.TerminationIncomplete {
				s.Status.TerminationIncomplete = true
			}
		}
	}

	sort.Strings(legacy)
	return legacy, nil
}

func (s InterruptScope) setRecoveryCondition(policy CancelPolicy, legacy []string) {
	var messages []string

	policyRequires := policy.RequiresRecovery && s.Status.AnyNodeMutationObserved
	if policyRequires {
		message := policy.RecoveryMessage
		if len(legacy) > 0 {
			message = fmt.Sprintf("%s (determined conservatively: %s predate plan-state reporting, "+
				"so whether they executed the plan cannot be known)", message, strings.Join(legacy, ", "))
		}
		messages = append(messages, message)
	}

	if s.Status.TerminationIncomplete {
		// Surface TerminationIncomplete regardless of CancelPolicy. It warns that processes may still run.
		messages = append(messages,
			"a node reported that processes from an interrupted instruction may still be running")
	}

	if len(messages) == 0 {
		opv1alpha1.RecoveryRequiredCondition.False(s.Status)
		opv1alpha1.RecoveryRequiredCondition.Reason(s.Status, opv1alpha1.NotRecoveryRequiredReason)
		opv1alpha1.RecoveryRequiredCondition.Message(s.Status, "")
		return
	}

	reason := opv1alpha1.RecoveryRequiredReason
	if !policyRequires {
		reason = opv1alpha1.TerminationIncompleteReason
	}
	opv1alpha1.RecoveryRequiredCondition.True(s.Status)
	opv1alpha1.RecoveryRequiredCondition.Reason(s.Status, reason)
	opv1alpha1.RecoveryRequiredCondition.Message(s.Status, strings.Join(messages, "; "))
}

// unconfirmedNodes returns names of nodes that did not confirm cancellation.
// A node is confirmed when its plan state is terminal. Legacy Secrets without state are unconfirmable.
func (s InterruptScope) unconfirmedNodes(secrets []*corev1.Secret) ([]string, error) {
	return s.nodesNotIn(secrets, func(st *plan.PlanStatus) bool {
		return st.State.IsTerminal()
	})
}

// unstoppedNodes returns the sorted names of nodes that have not demonstrably stopped for a pause.
// A node has stopped when its plan state is paused or terminal.
func (s InterruptScope) unstoppedNodes(secrets []*corev1.Secret) ([]string, error) {
	return s.nodesNotIn(secrets, func(st *plan.PlanStatus) bool {
		return st.State == plan.PlanStatePaused || st.State.IsTerminal()
	})
}

func (s InterruptScope) nodesNotIn(secrets []*corev1.Secret, satisfied func(*plan.PlanStatus) bool) ([]string, error) {
	var out []string
	for _, secret := range secrets {
		if secret == nil {
			continue
		}
		if !hasPlan(secret) {
			continue
		}

		st, err := s.Store.Status(secret)
		if err != nil {
			return nil, err
		}
		if satisfied(st) {
			continue
		}
		out = append(out, nodeName(secret))
	}
	sort.Strings(out)
	return out, nil
}

// hasPlan reports whether a machine-plan Secret has a plan.
//
// Machine-plan Secrets start empty. pkg/controllers/capr/unmanaged does not populate Data. A
// controller adds a plan later with AssignPlan. InterruptScope.Secrets is the unfiltered cluster-wide
// set, so it includes nodes that the operation does not target. Examples include workers under an
// etcd- or control-plane-scoped step and nodes that register during the operation.
//
// Both read paths skip Secrets without plans. This separates "no plan was assigned" from the legacy
// checksum flow. In the legacy flow, a Secret has a plan but no state. LegacyPlanFlowReason covers
// that case and keeps its conservative handling.
//
// Treating both cases the same causes each cancellation with a dedicated worker to wait for
// CancelConfirmationTimeout while holding the beacon. It then reports RecoveryRequired for a plan
// that was never delivered.
//
// The write path is unchanged. SyncInterruptAnnotations still annotates every Secret because a
// later step can assign a plan to that node.
func hasPlan(secret *corev1.Secret) bool {
	return len(secret.Data["plan"]) > 0
}

// nodeName resolves the human-readable name for a machine-plan Secret.
func nodeName(secret *corev1.Secret) string {
	if secret == nil {
		return ""
	}
	if name := secret.Labels[planv1alpha1.MachineLifecycleNameLabel]; name != "" {
		return name
	}
	return secret.Name
}

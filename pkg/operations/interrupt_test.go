package operations

import (
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/plan"
	ctrlfake "github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func interruptSecret(name string, annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet-default", Annotations: annotations},
	}
}

func TestSyncInterruptAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		paused      bool
		canceled    bool
		wantChanged bool
		wantFinal   map[string]string
	}{
		{
			name:        "pause on a clean secret",
			annotations: map[string]string{},
			paused:      true,
			wantChanged: true,
			wantFinal:   map[string]string{plan.PlanPausedAnnotation: "true"},
		},
		{
			name:        "pause is idempotent",
			annotations: map[string]string{plan.PlanPausedAnnotation: "true"},
			paused:      true,
			wantChanged: false,
			wantFinal:   map[string]string{plan.PlanPausedAnnotation: "true"},
		},
		{
			name:        "cancel beats pause and actively clears it",
			annotations: map[string]string{plan.PlanPausedAnnotation: "true"},
			paused:      true,
			canceled:    true,
			wantChanged: true,
			wantFinal:   map[string]string{plan.PlanCanceledAnnotation: "true"},
		},
		{
			name:        "clearing removes both keys rather than writing false",
			annotations: map[string]string{plan.PlanPausedAnnotation: "true", plan.PlanCanceledAnnotation: "true"},
			wantChanged: true,
			wantFinal:   map[string]string{},
		},
		{
			name:        "clearing a clean secret writes nothing",
			annotations: map[string]string{},
			wantChanged: false,
			wantFinal:   map[string]string{},
		},
		{
			name:        "an invalid hand-written value is overwritten, not left to fail closed on the agent",
			annotations: map[string]string{plan.PlanPausedAnnotation: "yes"},
			paused:      true,
			wantChanged: true,
			wantFinal:   map[string]string{plan.PlanPausedAnnotation: "true"},
		},
		{
			name:        "nil annotation map is handled",
			annotations: nil,
			paused:      true,
			wantChanged: true,
			wantFinal:   map[string]string{plan.PlanPausedAnnotation: "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)

			var written *corev1.Secret
			client.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
				written = s
				return s, nil
			}).AnyTimes()

			in := interruptSecret("plan-a", tt.annotations)
			// Snapshot before the call: in.Annotations aliases tt.annotations, so comparing the
			// two afterwards would be a tautology that an in-place mutation passes.
			before := maps.Clone(in.Annotations)

			changed, err := SyncInterruptAnnotations(client, []*corev1.Secret{in}, tt.paused, tt.canceled)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)

			// Checks the property assert.NotSame only gestures at: a shallow struct copy sharing
			// the same Annotations map header would leave the caller's Secret mutated.
			assert.Equal(t, before, in.Annotations, "the caller's Secret must not be mutated in place")

			if !tt.wantChanged {
				assert.Nil(t, written, "no Update should be issued when nothing differs")
				return
			}
			require.NotNil(t, written)
			assert.Equal(t, tt.wantFinal, written.Annotations)
		})
	}
}

// TestSyncInterruptAnnotations_PartialFailure ensures a failure on one Secret does not abort the rest.
// The call is idempotent per Secret. The controller may requeue the reconcile.
func TestSyncInterruptAnnotations_PartialFailure(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	var updated []string
	client.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		if s.Name == "plan-bad" {
			return nil, errors.New("boom")
		}
		updated = append(updated, s.Name)
		return s, nil
	}).Times(3)

	changed, err := SyncInterruptAnnotations(client, []*corev1.Secret{
		interruptSecret("plan-a", nil),
		interruptSecret("plan-bad", nil),
		interruptSecret("plan-c", nil),
	}, true, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan-bad")
	assert.Equal(t, []string{"plan-a", "plan-c"}, updated,
		"a failure on one secret must not stop the others from converging")
	assert.True(t, changed,
		"secrets that did converge were written, so the caller must still see changed")
}

// TestSyncInterruptAnnotations_AllFailuresReportNotChanged ensures that when all Updates fail
// changed remains false. A dirty Secret whose Update failed was not written.
func TestSyncInterruptAnnotations_AllFailuresReportNotChanged(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	client.EXPECT().Update(gomock.Any()).Return(nil, errors.New("boom")).Times(1)

	changed, err := SyncInterruptAnnotations(client, []*corev1.Secret{
		interruptSecret("plan-bad", nil),
	}, true, false)

	require.Error(t, err)
	assert.False(t, changed, "a dirty secret whose Update failed was not written")
}

func TestCancelPolicyRegistry(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "test.cattle.io", Version: "v1", Kind: "Widget"}
	t.Cleanup(func() { delete(cancelPolicies, gvk.String()) })

	t.Run("an unregistered type defaults to requiring recovery", func(t *testing.T) {
		got := CancelPolicyFor(schema.GroupVersionKind{Group: "unknown", Version: "v1", Kind: "Nope"})
		assert.True(t, got.RequiresRecovery,
			"a new operation type that forgets to register must fail towards over-warning")
		assert.NotEmpty(t, got.RecoveryMessage)
	})

	t.Run("a registered policy is returned verbatim", func(t *testing.T) {
		want := CancelPolicy{RequiresRecovery: false, RecoveryMessage: ""}
		RegisterCancelPolicy(gvk, want)
		assert.Equal(t, want, CancelPolicyFor(gvk))
	})

	t.Run("registering again replaces the policy", func(t *testing.T) {
		want := CancelPolicy{RequiresRecovery: true, RecoveryMessage: "recover me"}
		RegisterCancelPolicy(gvk, want)
		assert.Equal(t, want, CancelPolicyFor(gvk))
	})
}

// TestCancelPolicyRegistryConcurrentAccess verifies that concurrent registration and lookup are safe.
// Controllers normally register policies at init. A concurrent map read and write is fatal without locking.
func TestCancelPolicyRegistryConcurrentAccess(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "test.cattle.io", Version: "v1", Kind: "Racer"}
	t.Cleanup(func() {
		cancelPoliciesMu.Lock()
		defer cancelPoliciesMu.Unlock()
		delete(cancelPolicies, gvk.String())
	})

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(2 * goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			RegisterCancelPolicy(gvk, CancelPolicy{RequiresRecovery: true, RecoveryMessage: "recover me"})
		}()
		go func() {
			defer wg.Done()
			// True for both the registered policy and the default, so this is deterministic
			// regardless of how the two goroutines interleave.
			assert.True(t, CancelPolicyFor(gvk).RequiresRecovery)
		}()
	}
	wg.Wait()

	assert.Equal(t, CancelPolicy{RequiresRecovery: true, RecoveryMessage: "recover me"}, CancelPolicyFor(gvk))
}

// interruptTestGVK is a throwaway operation type used by tests to exercise policy-dependent branches.
var interruptTestGVK = schema.GroupVersionKind{Group: "test.cattle.io", Version: "v1", Kind: "Op"}

// planStateSecret builds a machine-plan Secret carrying a plan and the given agent-authored state.
// When state == "" the Secret simulates the legacy checksum flow.
func planStateSecret(name string, state plan.PlanState, progress string) *corev1.Secret {
	planBytes := []byte(`{"instructions":[]}`)
	data := map[string][]byte{"plan": planBytes}
	if state != "" {
		data[plan.PlanStateKey] = []byte(state)
	}
	if progress != "" {
		data[plan.PlanCheckpointKey] = []byte(progress)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "fleet-default", Annotations: map[string]string{},
			Labels: map[string]string{capr.ClusterNameLabel: "test"},
		},
		Type: plan.SecretTypeMachinePlan,
		Data: data,
	}
}

// newInterruptScope creates an InterruptScope backed by an in-memory Secret client for tests.
// It returns pointers for inspecting mutated status and written Secrets.
func newInterruptScope(t *testing.T, phase opv1alpha1.OperationPhase, secrets ...*corev1.Secret) (
	InterruptScope, *opv1alpha1.OperationSpec, *opv1alpha1.OperationStatus, *[]*corev1.Secret,
) {
	t.Helper()
	ctrl := gomock.NewController(t)
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](ctrl)

	written := &[]*corev1.Secret{}
	client.EXPECT().Update(gomock.Any()).DoAndReturn(func(s *corev1.Secret) (*corev1.Secret, error) {
		*written = append(*written, s)
		return s, nil
	}).AnyTimes()

	spec := &opv1alpha1.OperationSpec{}
	status := &opv1alpha1.OperationStatus{Phase: phase}

	return InterruptScope{
		LogPrefix:    "test",
		Namespace:    "fleet-default",
		Name:         "op-1",
		GVK:          interruptTestGVK,
		Spec:         spec,
		Status:       status,
		SetPhase:     func(p opv1alpha1.OperationPhase) { status.Phase = p; status.LastUpdated = metav1.Now() },
		Secrets:      func() ([]*corev1.Secret, error) { return secrets, nil },
		SecretClient: client,
		Store:        plan.NewStore(nil), // Store.Status performs no I/O
	}, spec, status, written
}

func TestHandleInterrupt_NoInterruptIsNotHandled(t *testing.T) {
	s, _, _, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
		planStateSecret("a", plan.PlanStateInProgress, ""))

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.False(t, handled, "an uninterrupted operation must fall through to its phase handler")
	assert.Empty(t, *written)
}

func TestHandleInterrupt_TerminalOperationIsNotGated(t *testing.T) {
	s, spec, _, written := newInterruptScope(t, opv1alpha1.OperationPhaseSucceeded,
		planStateSecret("a", plan.PlanStateSucceeded, ""))
	spec.Cancel = true

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.False(t, handled,
		"a terminal operation's phase handler is what releases the beacon; it must never be gated")
	assert.Empty(t, *written)
}

func TestHandleInterrupt_CancelWhilePending(t *testing.T) {
	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhasePending)
	spec.Cancel = true

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase,
		"no plan was ever dispatched, so there is nothing to wait for")
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(status))
	assert.Empty(t, *written, "no Secret carries a plan for a Pending operation")
	assert.False(t, opv1alpha1.RecoveryRequiredCondition.IsTrue(status))
	assert.True(t, status.CancelRequestedAt.IsZero())
}

func TestHandleInterrupt_CancelWhileInProgressWaitsForConfirmation(t *testing.T) {
	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
		planStateSecret("a", plan.PlanStateInProgress, ""))
	spec.Cancel = true

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase,
		"flipping to Canceled here would release the beacon while a node may still be mid-instruction")
	assert.Equal(t, opv1alpha1.CancelRequestedReason, opv1alpha1.CanceledCondition.GetReason(status))
	assert.False(t, status.CancelRequestedAt.IsZero(), "the confirmation timeout must be anchored")

	require.Len(t, *written, 1)
	assert.Equal(t, "true", (*written)[0].Annotations[plan.PlanCanceledAnnotation])
}

func TestHandleInterrupt_CancelRequestedAtIsStamppedOnce(t *testing.T) {
	first := metav1.NewTime(time.Now().Add(-30 * time.Second))
	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
		planStateSecret("a", plan.PlanStateInProgress, ""))
	spec.Cancel = true
	status.CancelRequestedAt = first

	_, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.Equal(t, first, status.CancelRequestedAt,
		"LastUpdated moves on step transitions, so the timeout anchor must never be rewritten")
}

// A node in a terminal plan state produces no cancellation write. Any terminal state counts as confirmation.
func TestHandleInterrupt_TerminalNodeCountsAsConfirmed(t *testing.T) {
	for _, state := range []plan.PlanState{plan.PlanStateSucceeded, plan.PlanStateFailed, plan.PlanStateCanceled} {
		t.Run(string(state), func(t *testing.T) {
			s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
				planStateSecret("a", state, ""))
			spec.Cancel = true

			handled, err := HandleInterrupt(s)
			require.NoError(t, err)
			assert.True(t, handled)
			assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
			assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(status))
		})
	}
}

// The tempting shortcut "no plan-state means nothing to wait for" is unsafe.
// State == "" means the agent uses the checksum flow and ignores interrupt annotations.
func TestHandleInterrupt_LegacyNodeNeverConfirmsAndTimesOut(t *testing.T) {
	t.Run("does not confirm immediately", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("legacy-a", "", ""))
		spec.Cancel = true

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase,
			"instantly confirming a legacy node would release the beacon out from under a running plan")
	})

	t.Run("times out with LegacyPlanFlowReason", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("legacy-a", "", ""))
		spec.Cancel = true
		status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-2 * CancelConfirmationTimeout))

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase,
			"a timed-out confirmation is still a cancellation, not a failure")
		assert.Equal(t, opv1alpha1.LegacyPlanFlowReason, opv1alpha1.CanceledCondition.GetReason(status))
		assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(status), "legacy-a")
	})

	t.Run("times out with AgentConfirmationTimeoutReason when the node does report plan-state", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("slow-a", plan.PlanStateInProgress, ""))
		spec.Cancel = true
		status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-2 * CancelConfirmationTimeout))

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
		assert.Equal(t, opv1alpha1.AgentConfirmationTimeoutReason, opv1alpha1.CanceledCondition.GetReason(status))
		assert.Contains(t, opv1alpha1.CanceledCondition.GetMessage(status), "slow-a")
	})
}

// planlessSecret builds a machine-plan Secret that was never assigned a plan. It has no Data.
func planlessSecret(name string) *corev1.Secret {
	secret := planStateSecret(name, "", "")
	secret.Data = nil
	return secret
}

// A Secret with no plan is not the legacy checksum flow. Read paths must skip it. The write path still covers it.
func TestHandleInterrupt_PlanlessSecretIsNotANode(t *testing.T) {
	t.Run("cancel confirms without waiting for it", func(t *testing.T) {
		s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("etcd-a", plan.PlanStateCanceled, ""),
			planlessSecret("worker-b"))
		spec.Cancel = true

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase,
			"waiting for a node with no plan burns the whole CancelConfirmationTimeout holding the beacon")
		assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(status))
		assert.Len(t, *written, 2, "the write walk stays unfiltered")
	})

	t.Run("cancel does not latch it as mutation evidence", func(t *testing.T) {
		RegisterCancelPolicy(interruptTestGVK, CancelPolicy{
			RequiresRecovery: true, RecoveryMessage: "run an ETCDSnapshotRestore",
		})
		t.Cleanup(func() {
			cancelPoliciesMu.Lock()
			defer cancelPoliciesMu.Unlock()
			delete(cancelPolicies, interruptTestGVK.String())
		})

		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("etcd-a", plan.PlanStateCanceled, ""),
			planlessSecret("worker-b"))
		spec.Cancel = true

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.False(t, status.AnyNodeMutationObserved,
			"a node that was never given a plan cannot have executed one")
		assert.True(t, opv1alpha1.RecoveryRequiredCondition.IsFalse(status),
			"telling a user to run an etcd restore because a worker has no plan is a false alarm")
	})

	t.Run("pause does not report it as still running", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("etcd-a", plan.PlanStatePaused, ""),
			planlessSecret("worker-b"))
		spec.Paused = true

		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(status),
			"every node that has a plan has stopped, so the pause is in effect, not merely requested")
		assert.NotContains(t, opv1alpha1.PausedCondition.GetMessage(status), "worker-b")
	})
}

func TestHandleInterrupt_RecoveryRequired(t *testing.T) {
	planBytes := []byte(`{"instructions":[]}`)
	checksum := plan.Checksum(planBytes)
	partial := `{"checksum":"` + checksum + `","completedInstructions":2,"totalInstructions":5}`
	untermed := `{"checksum":"` + checksum + `","completedInstructions":0,"terminationIncomplete":true}`

	tests := []struct {
		name         string
		policy       CancelPolicy
		secret       *corev1.Secret
		wantRequired bool
		wantReason   string
		wantInMsg    string
	}{
		{
			name:         "mutation observed under a RequiresRecovery policy",
			policy:       CancelPolicy{RequiresRecovery: true, RecoveryMessage: "run an ETCDSnapshotRestore"},
			secret:       planStateSecret("a", plan.PlanStateCanceled, partial),
			wantRequired: true,
			wantReason:   opv1alpha1.RecoveryRequiredReason,
			wantInMsg:    "run an ETCDSnapshotRestore",
		},
		{
			name:         "same evidence under a no-recovery policy reports nothing",
			policy:       CancelPolicy{RequiresRecovery: false},
			secret:       planStateSecret("a", plan.PlanStateCanceled, partial),
			wantRequired: false,
			wantReason:   opv1alpha1.NotRecoveryRequiredReason,
		},
		{
			name:         "a legacy node latches conservatively under a RequiresRecovery policy",
			policy:       CancelPolicy{RequiresRecovery: true, RecoveryMessage: "run an ETCDSnapshotRestore"},
			secret:       planStateSecret("legacy-a", "", ""),
			wantRequired: true,
			wantReason:   opv1alpha1.RecoveryRequiredReason,
			wantInMsg:    "predate plan-state reporting",
		},
		{
			name:         "terminationIncomplete is reported even under a no-recovery policy",
			policy:       CancelPolicy{RequiresRecovery: false},
			secret:       planStateSecret("a", plan.PlanStateCanceled, untermed),
			wantRequired: true,
			wantReason:   opv1alpha1.TerminationIncompleteReason,
			wantInMsg:    "may still be running",
		},
		{
			name:         "a clean cancel with no mutation requires nothing",
			policy:       CancelPolicy{RequiresRecovery: true, RecoveryMessage: "run an ETCDSnapshotRestore"},
			secret:       planStateSecret("a", plan.PlanStateCanceled, `{"checksum":"`+checksum+`","completedInstructions":0}`),
			wantRequired: false,
			wantReason:   opv1alpha1.NotRecoveryRequiredReason,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RegisterCancelPolicy(interruptTestGVK, tt.policy)
			t.Cleanup(func() { delete(cancelPolicies, interruptTestGVK.String()) })

			s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress, tt.secret)
			spec.Cancel = true
			status.CancelRequestedAt = metav1.NewTime(time.Now().Add(-2 * CancelConfirmationTimeout))

			_, err := HandleInterrupt(s)
			require.NoError(t, err)
			assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
			assert.Equal(t, tt.wantRequired, opv1alpha1.RecoveryRequiredCondition.IsTrue(status))
			assert.Equal(t, tt.wantReason, opv1alpha1.RecoveryRequiredCondition.GetReason(status))
			if tt.wantInMsg != "" {
				assert.Contains(t, opv1alpha1.RecoveryRequiredCondition.GetMessage(status), tt.wantInMsg)
			}
		})
	}
}

func TestHandleInterrupt_CancelBeatsPause(t *testing.T) {
	s, spec, _, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
		planStateSecret("a", plan.PlanStateInProgress, ""))
	spec.Paused = true
	spec.Cancel = true

	_, err := HandleInterrupt(s)
	require.NoError(t, err)
	require.Len(t, *written, 1)
	assert.Equal(t, "true", (*written)[0].Annotations[plan.PlanCanceledAnnotation])
	assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation,
		"the two annotations must never both be true; Rancher mirrors the agent's precedence")
}

func TestHandleInterrupt_Pause(t *testing.T) {
	t.Run("requested until every node stops", func(t *testing.T) {
		s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("a", plan.PlanStateInProgress, ""))
		spec.Paused = true

		handled, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.True(t, handled)
		require.Len(t, *written, 1)
		assert.Equal(t, "true", (*written)[0].Annotations[plan.PlanPausedAnnotation])
		assert.True(t, opv1alpha1.PausedCondition.IsTrue(status))
		assert.Equal(t, opv1alpha1.PauseRequestedReason, opv1alpha1.PausedCondition.GetReason(status))
	})

	t.Run("in effect once every node reports paused or terminal", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("a", plan.PlanStatePaused, ""),
			planStateSecret("b", plan.PlanStateSucceeded, ""))
		spec.Paused = true

		// Both nodes have already stopped: one paused at an instruction boundary, one had already
		// reached succeeded when the pause landed. Writing the annotation does not change either
		// reported plan state, so only the reporting branch is under test.
		_, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(status))
	})

	t.Run("a pause that never confirms just keeps reporting, it never times out", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			planStateSecret("legacy-a", "", ""))
		spec.Paused = true
		status.LastUpdated = metav1.NewTime(time.Now().Add(-24 * time.Hour))

		handled, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.True(t, handled)
		assert.Equal(t, opv1alpha1.OperationPhaseInProgress, status.Phase,
			"nothing downstream is gated on pause confirmation, so there is nothing to time out")
		assert.Equal(t, opv1alpha1.PauseRequestedReason, opv1alpha1.PausedCondition.GetReason(status))
	})
}

func TestHandleInterrupt_Resume(t *testing.T) {
	paused := planStateSecret("a", plan.PlanStatePaused, "")
	paused.Annotations[plan.PlanPausedAnnotation] = "true"

	s, _, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress, paused)
	opv1alpha1.PausedCondition.True(status) // the operation was paused on the previous reconcile

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.True(t, handled, "skip the phase dispatch for the one reconcile that clears annotations")
	require.Len(t, *written, 1)
	assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation)
	assert.False(t, opv1alpha1.PausedCondition.IsTrue(status))
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(status))

	// Second reconcile: nothing left to clear, so the phase dispatch runs again.
	handled, err = HandleInterrupt(s)
	require.NoError(t, err)
	assert.False(t, handled)
}

// An operation that was never paused must not list Secrets on every reconcile.
func TestHandleInterrupt_ResumeIsFreeWhenNeverPaused(t *testing.T) {
	s, _, _, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress)
	listed := false
	s.Secrets = func() ([]*corev1.Secret, error) { listed = true; return nil, nil }

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.False(t, listed, "PausedCondition is the record of a previous pause; without it, do nothing")
}

// Cancel beats pause in reporting as well as on Secrets. HandleInterrupt owns PausedCondition.
// Do not derive PausedCondition from spec.Paused once this gate exists.
func TestHandleInterrupt_CancelClearsTheStalePausedCondition(t *testing.T) {
	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
		planStateSecret("a", plan.PlanStateInProgress, ""))
	opv1alpha1.PausedCondition.True(status) // paused on a previous reconcile
	opv1alpha1.PausedCondition.Reason(status, opv1alpha1.PausedReason)
	spec.Paused = true
	spec.Cancel = true

	_, err := HandleInterrupt(s)
	require.NoError(t, err)

	require.Len(t, *written, 1)
	assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation)
	assert.False(t, opv1alpha1.PausedCondition.IsTrue(status),
		"the condition must not keep claiming a pause the Secrets no longer carry")
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(status))
}

// The Pending cancel shortcut returns before SyncInterruptAnnotations. The Pending shortcut must still clear pauses.
func TestHandleInterrupt_CancelWhilePendingClearsTheStalePausedCondition(t *testing.T) {
	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhasePending)
	opv1alpha1.PausedCondition.True(status)
	spec.Paused = true
	spec.Cancel = true

	_, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
	assert.False(t, opv1alpha1.PausedCondition.IsTrue(status))
	assert.Equal(t, opv1alpha1.NotPausedReason, opv1alpha1.PausedCondition.GetReason(status))
}

// handlePause has no phase guard and may write paused on every machine-plan Secret while Pending.
// The Pending cancel shortcut must clear those annotations to avoid halting agents cluster-wide.
func TestHandleInterrupt_CancelWhilePendingClearsAPreviousPausesAnnotations(t *testing.T) {
	paused := planStateSecret("a", plan.PlanStatePaused, "")
	paused.Annotations[plan.PlanPausedAnnotation] = "true"

	s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhasePending, paused)
	opv1alpha1.PausedCondition.True(status) // paused on a previous reconcile
	spec.Cancel = true

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
	assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(status))

	require.Len(t, *written, 1)
	assert.NotContains(t, (*written)[0].Annotations, plan.PlanPausedAnnotation)
}

// The clear is gated on the same record handleResume uses, so cancelling a Pending operation that
// was never paused stays infallible: no List, and therefore no way for it to fail.
func TestHandleInterrupt_CancelWhilePendingListsNothingWhenNeverPaused(t *testing.T) {
	s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhasePending)
	s.Secrets = func() ([]*corev1.Secret, error) { return nil, errors.New("must not be listed") }
	spec.Cancel = true

	handled, err := HandleInterrupt(s)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase)
}

// A nil element in the Secret slice must be skipped, not surfaced as an error and not counted as a
// node. Store.Status rejects nil, and that error returns before the timeout branch, so a nil would
// strand the operation InProgress holding the cluster's beacon with no escape — the cancellation
// timeout cannot rescue it. Counting it as unconfirmed instead of skipping would be no better: nil
// never becomes terminal, so the wait would run to timeout on a phantom that has no name to report.
// SyncInterruptAnnotations and nodeName already guard nil; these two loops must agree.
func TestHandleInterrupt_NilSecretInTheSliceIsSkipped(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		s, spec, status, written := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			nil, planStateSecret("a", plan.PlanStateCanceled, ""))
		spec.Cancel = true

		handled, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.True(t, handled)
		assert.Equal(t, opv1alpha1.OperationPhaseCanceled, status.Phase,
			"the only real node is terminal, so nothing may keep the confirmation loop waiting")
		assert.Equal(t, opv1alpha1.CanceledReason, opv1alpha1.CanceledCondition.GetReason(status))
		require.Len(t, *written, 1, "only the real Secret is writable")
	})

	t.Run("pause", func(t *testing.T) {
		s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
			nil, planStateSecret("a", plan.PlanStatePaused, ""))
		spec.Paused = true

		handled, err := HandleInterrupt(s)
		require.NoError(t, err)
		assert.True(t, handled)
		assert.Equal(t, opv1alpha1.PausedReason, opv1alpha1.PausedCondition.GetReason(status),
			"a nil must not be reported as a node that has not stopped")
	})
}

// failingSecretClient is a Secret client whose every Update fails, for driving
// SyncInterruptAnnotations' error return.
func failingSecretClient(t *testing.T) *ctrlfake.MockClientInterface[*corev1.Secret, *corev1.SecretList] {
	t.Helper()
	client := ctrlfake.NewMockClientInterface[*corev1.Secret, *corev1.SecretList](gomock.NewController(t))
	client.EXPECT().Update(gomock.Any()).Return(nil, errors.New("update boom")).AnyTimes()
	return client
}

// unreadableSecret builds a machine-plan Secret whose PlanStatus cannot be computed: the
// failure-count is not a number and the failed-checksum is scoped to the plan the Secret currently
// carries, so statusFromSecret reaches the strconv.Atoi that fails.
func unreadableSecret(name string) *corev1.Secret {
	secret := planStateSecret(name, plan.PlanStateInProgress, "")
	secret.Data["failed-checksum"] = []byte(plan.PlanHash(secret.Data["plan"]))
	secret.Data["failure-count"] = []byte("not-a-number")
	return secret
}

// TestHandleInterrupt_EveryErrorPathReportsHandled pins the load-bearing global invariant: an error
// must never be paired with handled == false.
//
// handled == false tells the caller to run its phase handler, and every phase handler eventually
// reaches AssignPlan, which issues a full Update on an object derived from the same cached Secret
// that SyncInterruptAnnotations just wrote — an intermittent 409 — and which unconditionally
// deletes both interrupt annotations, undoing the interrupt the user asked for. Falling through
// also means an operation that keeps making progress after being told to stop, over Secrets whose
// interrupt state is by definition unknown because the write is what failed.
//
// Nothing else in the suite would notice a refactor that flipped one of these to false, because
// every other test drives a client whose Update always succeeds.
func TestHandleInterrupt_EveryErrorPathReportsHandled(t *testing.T) {
	listFails := func(s *InterruptScope) {
		s.Secrets = func() ([]*corev1.Secret, error) { return nil, errors.New("list boom") }
	}
	updateFails := func(s *InterruptScope) { s.SecretClient = failingSecretClient(t) }

	tests := []struct {
		name string
		// line is the error return in interrupt.go this case drives, so a future reader can map
		// the case back to the branch it protects.
		line   int
		phase  opv1alpha1.OperationPhase
		secret *corev1.Secret
		setup  func(*InterruptScope, *opv1alpha1.OperationSpec, *opv1alpha1.OperationStatus)
	}{
		{
			name: "cancel while pending: listing to clear a previous pause fails",
			line: 244, phase: opv1alpha1.OperationPhasePending,
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, status *opv1alpha1.OperationStatus) {
				spec.Cancel = true
				opv1alpha1.PausedCondition.True(status)
				listFails(s)
			},
		},
		{
			name: "cancel while pending: clearing a previous pause fails",
			line: 247, phase: opv1alpha1.OperationPhasePending,
			secret: func() *corev1.Secret {
				s := planStateSecret("a", plan.PlanStatePaused, "")
				s.Annotations[plan.PlanPausedAnnotation] = "true"
				return s
			}(),
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, status *opv1alpha1.OperationStatus) {
				spec.Cancel = true
				opv1alpha1.PausedCondition.True(status)
				updateFails(s)
			},
		},
		{
			name: "cancel: listing fails",
			line: 261, phase: opv1alpha1.OperationPhaseInProgress,
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Cancel = true
				listFails(s)
			},
		},
		{
			name: "cancel: writing the canceled annotation fails",
			line: 269, phase: opv1alpha1.OperationPhaseInProgress,
			secret: planStateSecret("a", plan.PlanStateInProgress, ""),
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Cancel = true
				updateFails(s)
			},
		},
		{
			name: "cancel: reading a node's plan status fails",
			line: 275, phase: opv1alpha1.OperationPhaseInProgress,
			secret: unreadableSecret("a"),
			setup: func(_ *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Cancel = true
			},
		},
		{
			name: "pause: listing fails",
			line: 330, phase: opv1alpha1.OperationPhaseInProgress,
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Paused = true
				listFails(s)
			},
		},
		{
			name: "pause: writing the paused annotation fails",
			line: 334, phase: opv1alpha1.OperationPhaseInProgress,
			secret: planStateSecret("a", plan.PlanStateInProgress, ""),
			setup: func(s *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Paused = true
				updateFails(s)
			},
		},
		{
			name: "pause: reading a node's plan status fails",
			line: 341, phase: opv1alpha1.OperationPhaseInProgress,
			secret: unreadableSecret("a"),
			setup: func(_ *InterruptScope, spec *opv1alpha1.OperationSpec, _ *opv1alpha1.OperationStatus) {
				spec.Paused = true
			},
		},
		{
			name: "resume: listing fails",
			line: 374, phase: opv1alpha1.OperationPhaseInProgress,
			setup: func(s *InterruptScope, _ *opv1alpha1.OperationSpec, status *opv1alpha1.OperationStatus) {
				opv1alpha1.PausedCondition.True(status)
				listFails(s)
			},
		},
		{
			name: "resume: clearing the annotations fails",
			line: 382, phase: opv1alpha1.OperationPhaseInProgress,
			secret: func() *corev1.Secret {
				s := planStateSecret("a", plan.PlanStatePaused, "")
				s.Annotations[plan.PlanPausedAnnotation] = "true"
				return s
			}(),
			setup: func(s *InterruptScope, _ *opv1alpha1.OperationSpec, status *opv1alpha1.OperationStatus) {
				opv1alpha1.PausedCondition.True(status)
				updateFails(s)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var secrets []*corev1.Secret
			if tt.secret != nil {
				secrets = append(secrets, tt.secret)
			}
			s, spec, status, _ := newInterruptScope(t, tt.phase, secrets...)
			tt.setup(&s, spec, status)

			handled, err := HandleInterrupt(s)
			require.Error(t, err, "interrupt.go:%d must report the failure", tt.line)
			assert.True(t, handled,
				"interrupt.go:%d must not let the caller fall through to its phase handler while "+
					"the interrupt state of the Secrets is unknown", tt.line)
		})
	}
}

// The two remaining error returns, interrupt.go:280 (unconfirmedNodes) and :346 (unstoppedNodes),
// are structurally unreachable and therefore have no case above. Both are dominated by the
// latchMutationEvidence call immediately before them: it walks the same slice, in the same order,
// through the same Store.Status, so it fails first on any Secret that would make them fail. This
// test pins that domination, so a reordering that breaks it is caught rather than silently
// creating an untested path.
//
// Two Secrets are required, and the reason is the whole point of the test. With a single
// unreadable Secret both orderings produce identical observable state — an error, and no latched
// evidence — so there is nothing to discriminate on and the test passes under the very reorder it
// exists to forbid. The readable Secret supplies the discriminating signal: it carries mutation
// evidence and sorts before the unreadable one, so in the correct order latchMutationEvidence
// latches it and only then errors, while under the reorder the confirmation walk errors first and
// the flag is never set.
func TestHandleInterrupt_LatchDominatesTheConfirmationWalk(t *testing.T) {
	for name, apply := range map[string]func(*opv1alpha1.OperationSpec){
		"cancel": func(spec *opv1alpha1.OperationSpec) { spec.Cancel = true },
		"pause":  func(spec *opv1alpha1.OperationSpec) { spec.Paused = true },
	} {
		t.Run(name, func(t *testing.T) {
			s, spec, status, _ := newInterruptScope(t, opv1alpha1.OperationPhaseInProgress,
				planStateSecret("a", plan.PlanStateFailed, ""), // latchable evidence, terminal
				unreadableSecret("b"),                          // errors in Store.Status
			)
			apply(spec)

			_, err := HandleInterrupt(s)
			require.Error(t, err, "the unreadable Secret must still surface as an error")
			assert.True(t, status.AnyNodeMutationObserved,
				"latchMutationEvidence must run before the confirmation walk: under the reverse "+
					"order the walk errors on the unreadable Secret first and the evidence from "+
					"the readable one is silently lost, leaving interrupt.go:280/:346 reachable "+
					"and untested")
		})
	}
}

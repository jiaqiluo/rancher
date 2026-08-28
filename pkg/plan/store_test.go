package plan

import (
	"bytes"
	"encoding/json"
	"testing"

	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Helper function to build a mock secret pointer
func mockSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func TestMessage(t *testing.T) {
	tests := []struct {
		name     string
		results  []PlanStatus
		expected string
	}{
		{
			name:     "Empty results slice",
			results:  []PlanStatus{},
			expected: "",
		},
		{
			name: "Nil secret items are skipped safely",
			results: []PlanStatus{
				{Secret: nil, Pending: true},
			},
			expected: "",
		},
		{
			name: "Single node waiting for plan applied",
			results: []PlanStatus{
				{Secret: mockSecret("node-alpha"), InProgress: true},
			},
			expected: "waiting for plan applied for node-alpha",
		},
		{
			name: "Two nodes waiting for plan picked up (Verifies exact suffix '1 other node')",
			results: []PlanStatus{
				// Out of order to test lexicographical sorting picks node-alpha as primary
				{Secret: mockSecret("node-beta"), Pending: true},
				{Secret: mockSecret("node-alpha"), Pending: true},
			},
			expected: "waiting for plan to be picked up for node-alpha & 1 other node",
		},
		{
			name: "Four nodes waiting for probes (Verifies plural scaling suffix)",
			results: []PlanStatus{
				{Secret: mockSecret("node-d"), Applied: true, ProbesPassed: false},
				{Secret: mockSecret("node-b"), Applied: true, ProbesPassed: false},
				{Secret: mockSecret("node-a"), Applied: true, ProbesPassed: false},
				{Secret: mockSecret("node-c"), Applied: true, ProbesPassed: false},
			},
			expected: "waiting for probes for node-a & 3 other nodes",
		},
		{
			name: "Strictly failed or completely successful nodes are excluded",
			results: []PlanStatus{
				{Secret: mockSecret("node-good"), Applied: true, ProbesPassed: true},
				{Secret: mockSecret("node-dead"), Failed: true},
			},
			expected: "",
		},
		{
			name: "Mixed messages with priority ordering (Failing -> Pending -> InProgress -> Probes)",
			results: []PlanStatus{
				{Secret: mockSecret("node-probes"), Applied: true, ProbesPassed: false},
				{Secret: mockSecret("node-failing"), Failing: true, Failed: false},
				{Secret: mockSecret("node-progress"), InProgress: true},
				{Secret: mockSecret("node-pending"), Pending: true},
			},
			expected: "failing plan for node-failing, waiting for plan to be picked up for node-pending, waiting for plan applied for node-progress, waiting for probes for node-probes",
		},
		{
			name: "Mixed messages with duplicate nodes per tier",
			results: []PlanStatus{
				{Secret: mockSecret("node-p1"), Pending: true},
				{Secret: mockSecret("node-p2"), Pending: true},
				{Secret: mockSecret("node-f1"), Failing: true, Failed: false},
				{Secret: mockSecret("node-f2"), Failing: true, Failed: false},
				{Secret: mockSecret("node-f3"), Failing: true, Failed: false},
			},
			expected: "failing plan for node-f1 & 2 other nodes, waiting for plan to be picked up for node-p1 & 1 other node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := Message(tt.results)
			if actual != tt.expected {
				t.Errorf("\nExpected: %q\nGot:      %q", tt.expected, actual)
			}
		})
	}
}

func planSecretWith(planBytes []byte, data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: map[string]string{}},
		Data:       map[string][]byte{"plan": planBytes},
	}
	for k, v := range data {
		s.Data[k] = v
	}
	return s
}

func TestParsePlanCheckpoint(t *testing.T) {
	planBytes := []byte(`{"instructions":[]}`)
	good := []byte(`{"checksum":"` + Checksum(planBytes) + `","completedInstructions":2,"totalInstructions":5,"terminationIncomplete":true}`)
	stale := []byte(`{"checksum":"deadbeef","completedInstructions":9}`)

	tests := []struct {
		name string
		data map[string][]byte
		want *PlanCheckpoint
	}{
		{name: "absent key", data: nil, want: nil},
		{name: "empty value", data: map[string][]byte{PlanCheckpointKey: {}}, want: nil},
		{name: "unparsable", data: map[string][]byte{PlanCheckpointKey: []byte("{")}, want: nil},
		{name: "checksum mismatch is ignored", data: map[string][]byte{PlanCheckpointKey: stale}, want: nil},
		{
			name: "matching checksum is returned",
			data: map[string][]byte{PlanCheckpointKey: good},
			want: &PlanCheckpoint{Checksum: Checksum(planBytes), Completed: 2, Total: 5, TerminationIncomplete: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePlanCheckpoint(planSecretWith(planBytes, tt.data))
			if tt.want == nil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got nil", tt.want)
			}
			if *got != *tt.want {
				t.Errorf("\nExpected: %+v\nGot:      %+v", *tt.want, *got)
			}
		})
	}
}

func TestStoreStatus_DerivesFromPlanState(t *testing.T) {
	planBytes := []byte(`{"instructions":[]}`)

	tests := []struct {
		name           string
		data           map[string][]byte
		wantState      PlanState
		wantPending    bool
		wantInProgress bool
	}{
		{
			name:           "legacy secret with no plan-state falls back to checksum behavior",
			data:           nil,
			wantState:      "",
			wantPending:    false,
			wantInProgress: true,
		},
		{
			name:           "pending",
			data:           map[string][]byte{PlanStateKey: []byte(PlanStatePending), PlanRevisionKey: []byte("3")},
			wantState:      PlanStatePending,
			wantPending:    true,
			wantInProgress: false,
		},
		{
			name:           "in-progress",
			data:           map[string][]byte{PlanStateKey: []byte(PlanStateInProgress)},
			wantState:      PlanStateInProgress,
			wantInProgress: true,
		},
		{
			name:           "paused counts as in progress so Waiting stays true",
			data:           map[string][]byte{PlanStateKey: []byte(PlanStatePaused)},
			wantState:      PlanStatePaused,
			wantInProgress: true,
		},
		{
			name:      "canceled is neither pending nor in progress",
			data:      map[string][]byte{PlanStateKey: []byte(PlanStateCanceled)},
			wantState: PlanStateCanceled,
		},
		{
			name:      "succeeded is neither pending nor in progress",
			data:      map[string][]byte{PlanStateKey: []byte(PlanStateSucceeded)},
			wantState: PlanStateSucceeded,
		},
		{
			// A system-agent that predates plan-state never clears the pending that AssignPlan
			// wrote, so pending is the permanent resting state for that whole population. The
			// applied checksum is the only signal such a node emits, and it has to win.
			name: "an applied checksum matching the plan outranks a stale pending",
			data: map[string][]byte{
				PlanStateKey:       []byte(PlanStatePending),
				"appliedPlan":      planBytes,
				"applied-checksum": []byte(PlanHash(planBytes)),
			},
			wantState:   PlanStatePending,
			wantPending: false,
		},
		{
			// appliedPlan is mirrored from applied-checksum by the plansecret controller, so the
			// two disagree only while a plan has been reassigned back to previously applied
			// content. There the node has moved on and the freshly written pending is the truthful
			// answer, so it must survive.
			name: "appliedPlan alone does not outrank pending when the applied checksum disagrees",
			data: map[string][]byte{
				PlanStateKey:       []byte(PlanStatePending),
				"appliedPlan":      planBytes,
				"applied-checksum": []byte(PlanHash([]byte("something else"))),
			},
			wantState:   PlanStatePending,
			wantPending: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&Store{}).Status(planSecretWith(planBytes, tt.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.State != tt.wantState {
				t.Errorf("State: expected %q, got %q", tt.wantState, got.State)
			}
			if got.Pending != tt.wantPending {
				t.Errorf("Pending: expected %v, got %v", tt.wantPending, got.Pending)
			}
			if got.InProgress != tt.wantInProgress {
				t.Errorf("InProgress: expected %v, got %v", tt.wantInProgress, got.InProgress)
			}
		})
	}
}

// recordingSecretClient captures the Secret passed to Update and echoes it back, so tests can
// assert on exactly what AssignPlan wrote.
type recordingSecretClient struct {
	corecontrollers.SecretClient
	updated *corev1.Secret
	updates int
}

func (c *recordingSecretClient) Update(s *corev1.Secret) (*corev1.Secret, error) {
	c.updated = s.DeepCopy()
	c.updates++
	return s, nil
}

func TestAssignPlan_InterruptConvergence(t *testing.T) {
	newPlan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	newPlanBytes, err := json.Marshal(newPlan)
	if err != nil {
		t.Fatalf("marshaling the plan: %v", err)
	}
	oldPlanBytes := []byte(`{"instructions":[]}`)

	tests := []struct {
		name              string
		data              map[string][]byte
		annotations       map[string]string
		wantUpdate        bool
		wantState         string
		wantProgressEmpty bool
		wantPausedGone    bool
		wantCanceledGone  bool
		wantPending       bool
		wantInProgress    bool
	}{
		{
			name:              "new plan content writes pending and clears everything",
			data:              map[string][]byte{"plan": oldPlanBytes, PlanCheckpointKey: []byte(`{"checksum":"x"}`)},
			annotations:       map[string]string{PlanPausedAnnotation: "true"},
			wantUpdate:        true,
			wantState:         string(PlanStatePending),
			wantProgressEmpty: true,
			wantPausedGone:    true,
			wantPending:       true,
		},
		{
			name: "identical content over a canceled plan is kicked back to pending",
			data: map[string][]byte{
				"plan":            newPlanBytes,
				PlanStateKey:      []byte(PlanStateCanceled),
				PlanCheckpointKey: []byte(`{"checksum":"x"}`),
			},
			annotations:       map[string]string{PlanCanceledAnnotation: "true"},
			wantUpdate:        true,
			wantState:         string(PlanStatePending),
			wantProgressEmpty: true,
			wantCanceledGone:  true,
			wantPending:       true,
		},
		{
			name:           "identical content over a paused plan clears the annotation but does not reset the state",
			data:           map[string][]byte{"plan": newPlanBytes, PlanStateKey: []byte(PlanStatePaused)},
			annotations:    map[string]string{PlanPausedAnnotation: "true"},
			wantUpdate:     true,
			wantState:      string(PlanStatePaused),
			wantPausedGone: true,
			wantInProgress: true,
		},
		{
			name:       "identical content over a succeeded plan writes nothing",
			data:       map[string][]byte{"plan": newPlanBytes, PlanStateKey: []byte(PlanStateSucceeded)},
			wantUpdate: false,
			wantState:  string(PlanStateSucceeded),
		},
		{
			name:           "identical content on a legacy secret writes nothing",
			data:           map[string][]byte{"plan": newPlanBytes},
			wantUpdate:     false,
			wantState:      "",
			wantInProgress: true,
		},
		{
			// pkg/capr/planner also writes plan-state onto these same machine-plan Secrets, so a
			// Secret can reach AssignPlan carrying a terminal state authored by an unrelated
			// writer. New content must still be announced as pending: before this behavior the
			// stale succeeded state made statusFromSecret report neither Pending nor InProgress,
			// Waiting() went false on the very reconcile that delivered the plan, and callers such
			// as the etcd snapshot controller treated an unstarted operation as finished.
			name:        "stale succeeded state with new content is still announced as pending",
			data:        map[string][]byte{"plan": oldPlanBytes, PlanStateKey: []byte(PlanStateSucceeded)},
			wantUpdate:  true,
			wantState:   string(PlanStatePending),
			wantPending: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := tt.annotations
			if annotations == nil {
				annotations = map[string]string{}
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: annotations},
				Data:       tt.data,
			}
			c := &recordingSecretClient{}
			status, err := (&Store{secrets: c}).AssignPlan(secret, newPlan, 1, -1)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := string(status.State); got != tt.wantState {
				t.Errorf("status.State: expected %q, got %q", tt.wantState, got)
			}
			if status.Pending != tt.wantPending {
				t.Errorf("status.Pending: expected %v, got %v", tt.wantPending, status.Pending)
			}
			if status.InProgress != tt.wantInProgress {
				t.Errorf("status.InProgress: expected %v, got %v", tt.wantInProgress, status.InProgress)
			}

			if tt.wantUpdate != (c.updates > 0) {
				t.Fatalf("expected update=%v, got %d updates", tt.wantUpdate, c.updates)
			}
			if !tt.wantUpdate {
				return
			}
			if got := string(c.updated.Data[PlanStateKey]); got != tt.wantState {
				t.Errorf("plan-state: expected %q, got %q", tt.wantState, got)
			}
			if tt.wantProgressEmpty {
				v, ok := c.updated.Data[PlanCheckpointKey]
				if !ok {
					t.Error("plan-progress must be cleared by writing an empty value, not by deleting the key")
				} else if len(v) != 0 {
					t.Errorf("plan-progress: expected empty, got %q", v)
				}
			}
			if tt.wantPausedGone {
				if _, ok := c.updated.Annotations[PlanPausedAnnotation]; ok {
					t.Error("paused annotation must be removed")
				}
			}
			if tt.wantCanceledGone {
				if _, ok := c.updated.Annotations[PlanCanceledAnnotation]; ok {
					t.Error("canceled annotation must be removed")
				}
			}
		})
	}
}

// TestAssignPlan_SingleUpdate pins the atomicity requirement: writing plan-state: pending while
// the canceled annotation is still set makes the agent call handleCancellation with a
// non-terminal state, which writes plan-state: canceled straight back and wedges the operation.
func TestAssignPlan_SingleUpdate(t *testing.T) {
	newPlan := &Plan{}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n", Namespace: "ns",
			Annotations: map[string]string{PlanCanceledAnnotation: "true"},
		},
		Data: map[string][]byte{"plan": []byte(`{"old":true}`), PlanStateKey: []byte(PlanStateCanceled)},
	}
	c := &recordingSecretClient{}
	if _, err := (&Store{secrets: c}).AssignPlan(secret, newPlan, 1, -1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("expected exactly one Update, got %d", c.updates)
	}
	if _, ok := c.updated.Annotations[PlanCanceledAnnotation]; ok {
		t.Error("the canceled annotation must be gone in the same Update that writes plan-state: pending")
	}
	if got := string(c.updated.Data[PlanStateKey]); got != string(PlanStatePending) {
		t.Errorf("plan-state: expected pending, got %q", got)
	}
}

const healthyProbeStatuses = `{"a":{"healthy":true,"successCount":1}}`

// applyPlanAsLegacyAgent simulates the two actors that carry a machine-plan Secret to completion
// when the downstream system-agent predates plan-state.
//
// The agent runs its checksum flow: it writes applied-checksum and probe-statuses and never reads
// or writes plan-state (system-agent pkg/k8splan/reconcile.go takes the decideChecksumFlowAction
// branch whenever plan-state is absent). Rancher's own plansecret controller then mirrors a
// matching applied-checksum into appliedPlan and stamps the probes-passed annotation once the
// probes report healthy (pkg/controllers/capr/plansecret/plansecret.go:71-85). appliedPlan is
// therefore never written by the agent, which is why a legacy Secret can show a completed plan
// while plan-state still reads whatever Rancher last wrote.
func applyPlanAsLegacyAgent(secret *corev1.Secret) *corev1.Secret {
	secret = secret.DeepCopy()
	planData := secret.Data["plan"]

	secret.Data["applied-checksum"] = []byte(PlanHash(planData))
	secret.Data["probe-statuses"] = []byte(healthyProbeStatuses)

	secret.Data["appliedPlan"] = planData
	secret.Annotations[PlanProbesPassedAnnotation] = "2026-08-21T00:00:00Z"
	return secret
}

// mirrorAsPlansecret reproduces the two mirroring rules of Rancher's plansecret controller
// (pkg/controllers/capr/plansecret/plansecret.go:71-85): appliedPlan is populated from any
// applied-checksum that matches the plan, and the probes-passed annotation is stamped once the
// probe statuses report healthy. It runs on every Secret change, so anything AssignPlan clears
// without also clearing its source is restored almost immediately.
func mirrorAsPlansecret(t *testing.T, secret *corev1.Secret) *corev1.Secret {
	t.Helper()

	secret = secret.DeepCopy()
	plan := secret.Data["plan"]

	if string(secret.Data["applied-checksum"]) == PlanHash(plan) && !bytes.Equal(plan, secret.Data["appliedPlan"]) {
		secret.Data["appliedPlan"] = plan
	}
	if len(secret.Data["probe-statuses"]) > 0 {
		_, healthy, err := ParseProbeStatuses(secret.Data["probe-statuses"])
		if err != nil {
			t.Fatalf("mirrorAsPlansecret: parsing probe statuses: %v", err)
		}
		if healthy && secret.Annotations[PlanProbesPassedAnnotation] == "" {
			secret.Annotations[PlanProbesPassedAnnotation] = "2026-08-21T12:00:00Z"
		}
	}
	return secret
}

// TestAssignPlan_LegacyAgentConverges walks the full reconcile sequence for a downstream cluster
// whose system-agent predates plan-state, which is the state of every already-provisioned cluster
// until its agent is upgraded.
//
// AssignPlan writes plan-state: pending to announce new content, but such an agent never reads that
// key and so never moves it off pending. Since Waiting() short-circuits on Pending, and all of the
// day-2 operation controllers in pkg/controllers/operations gate on Waiting(), a Pending that
// outranks the applied checksum makes every one of those operations hang forever. The applied
// checksum is the only completion signal these nodes emit, so it has to win.
func TestAssignPlan_LegacyAgentConverges(t *testing.T) {
	t.Parallel()

	newPlan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	c := &recordingSecretClient{}
	store := &Store{secrets: c}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: map[string]string{}},
		Data:       map[string][]byte{"plan": []byte(`{"instructions":[]}`)},
	}

	// Reconcile 1: the operation delivers new content.
	status, err := store.AssignPlan(secret, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("reconcile 1: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("reconcile 1: expected the new content to be written once, got %d updates", c.updates)
	}
	if status.State != PlanStatePending || !status.Pending || !status.Waiting() {
		t.Fatalf("reconcile 1: expected a pending plan that is still waiting, got State=%q Pending=%v Applied=%v Waiting=%v",
			status.State, status.Pending, status.Applied, status.Waiting())
	}

	// Reconcile 2: the agent has not picked the plan up yet. Nothing is written and the operation
	// is still legitimately waiting.
	status, err = store.AssignPlan(status.Secret, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("reconcile 2: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("reconcile 2: expected no further writes, got %d updates total", c.updates)
	}
	if !status.Pending || status.Applied || !status.Waiting() {
		t.Fatalf("reconcile 2: expected to still be waiting on an unstarted plan, got Pending=%v Applied=%v Waiting=%v",
			status.Pending, status.Applied, status.Waiting())
	}

	// The agent applies the plan and the plansecret controller records it. plan-state is untouched
	// by both and therefore still reads pending.
	applied := applyPlanAsLegacyAgent(status.Secret)
	if got := PlanState(applied.Data[PlanStateKey]); got != PlanStatePending {
		t.Fatalf("precondition: a legacy agent must leave plan-state alone, got %q", got)
	}

	// Reconcile 3: the operation must now observe completion and stop waiting.
	status, err = store.AssignPlan(applied, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("reconcile 3: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("reconcile 3: expected no further writes, got %d updates total", c.updates)
	}
	if !status.Applied || !status.ProbesPassed {
		t.Fatalf("reconcile 3: expected the applied checksum and probes to be observed, got Applied=%v ProbesPassed=%v",
			status.Applied, status.ProbesPassed)
	}
	if status.Pending {
		t.Error("reconcile 3: a stale plan-state: pending must not survive an applied checksum that matches the plan")
	}
	if status.InProgress {
		t.Error("reconcile 3: an applied plan is not in progress")
	}
	if status.Waiting() {
		t.Error("reconcile 3: the operation hangs forever if Waiting() never goes false for a legacy agent")
	}
	if !status.Success() {
		t.Error("reconcile 3: expected the plan to be reported as successfully applied")
	}
}

// TestAssignPlan_PlanStateAgentLifecycle pins the pending -> in-progress -> succeeded path of a
// plan-state-aware agent, which is the path the applied-checksum precedence rule must not disturb.
//
// The rule is safe for these agents because they cannot present a matching applied checksum while
// still at pending: system-agent commits the pending -> in-progress transition to the API server
// before calling Apply and returns hard if that write fails (pkg/k8splan/reconcile.go:240-255), so
// applied-checksum is only ever written at in-progress or later.
func TestAssignPlan_PlanStateAgentLifecycle(t *testing.T) {
	t.Parallel()

	newPlan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	newPlanBytes, err := json.Marshal(newPlan)
	if err != nil {
		t.Fatalf("marshaling the plan: %v", err)
	}
	oldPlanBytes := []byte(`{"instructions":[]}`)

	c := &recordingSecretClient{}
	store := &Store{secrets: c}

	// A Secret left behind by a previous, successful operation.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n", Namespace: "ns",
			Annotations: map[string]string{PlanProbesPassedAnnotation: "2026-08-20T00:00:00Z"},
		},
		Data: map[string][]byte{
			"plan":              oldPlanBytes,
			"appliedPlan":       oldPlanBytes,
			"applied-checksum":  []byte(PlanHash(oldPlanBytes)),
			"probe-statuses":    []byte(healthyProbeStatuses),
			PlanStateKey:        []byte(PlanStateSucceeded),
			PlanRevisionKey:     []byte("7"),
			PlanCheckpointKey:   []byte(`{"checksum":"` + Checksum(oldPlanBytes) + `","completedInstructions":1}`),
			"failure-threshold": []byte("-1"),
		},
	}

	// New content is announced as pending, and the previous run's completion evidence is retired
	// with it.
	status, err := store.AssignPlan(secret, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("assign: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("assign: expected one write, got %d", c.updates)
	}
	if status.State != PlanStatePending || !status.Pending || status.Applied || status.ProbesPassed || !status.Waiting() {
		t.Fatalf("assign: expected a fresh pending plan, got State=%q Pending=%v Applied=%v ProbesPassed=%v Waiting=%v",
			status.State, status.Pending, status.Applied, status.ProbesPassed, status.Waiting())
	}
	if status.Checkpoint != nil {
		t.Errorf("assign: expected the previous plan's checkpoint to be dropped, got %+v", status.Checkpoint)
	}

	// The agent commits pending -> in-progress before it applies anything.
	inProgress := status.Secret.DeepCopy()
	inProgress.Data[PlanStateKey] = []byte(PlanStateInProgress)
	inProgress.Data[PlanRevisionKey] = []byte("8")

	status, err = store.AssignPlan(inProgress, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("in-progress: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("in-progress: expected no further writes, got %d updates total", c.updates)
	}
	if status.State != PlanStateInProgress || status.Pending || !status.InProgress || status.Applied || !status.Waiting() {
		t.Fatalf("in-progress: got State=%q Pending=%v InProgress=%v Applied=%v Waiting=%v",
			status.State, status.Pending, status.InProgress, status.Applied, status.Waiting())
	}

	// The agent finishes: applied-checksum, probes and the terminal state land, and plansecret
	// mirrors them into appliedPlan and the probes-passed annotation.
	done := applyPlanAsLegacyAgent(status.Secret)
	done.Data[PlanStateKey] = []byte(PlanStateSucceeded)

	status, err = store.AssignPlan(done, newPlan, 1, -1)
	if err != nil {
		t.Fatalf("succeeded: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("succeeded: expected no further writes, got %d updates total", c.updates)
	}
	if status.State != PlanStateSucceeded {
		t.Errorf("succeeded: expected state succeeded, got %q", status.State)
	}
	if status.Pending || status.InProgress {
		t.Errorf("succeeded: expected a settled plan, got Pending=%v InProgress=%v", status.Pending, status.InProgress)
	}
	if !status.Applied || !status.ProbesPassed || !status.Success() || status.Waiting() {
		t.Errorf("succeeded: expected success, got Applied=%v ProbesPassed=%v Success=%v Waiting=%v",
			status.Applied, status.ProbesPassed, status.Success(), status.Waiting())
	}
	if !bytes.Equal(status.Secret.Data["plan"], newPlanBytes) {
		t.Error("succeeded: the plan content must not have been rewritten during the lifecycle")
	}
}

// TestAssignPlan_NewContentEmptiesProbeStatusesRatherThanDeletingIt applies the "clear Secret data
// by writing an empty value, never by deleting the key" rule to the arm of AssignPlan that travels
// most: a plan content change.
//
// The consequence of getting it wrong is latent rather than immediate. The agent's conflict-retry
// merge only carries forward keys present in its in-hand copy, so a deleted key is silently
// restored on retry — and a restored probe-statuses is stale probe data about the plan that was
// just replaced. Today the probes-passed annotation is emptied in the same write and gates the
// read, so nothing observes it; that is a second line of defence, not a reason to keep the first
// one broken.
func TestAssignPlan_NewContentEmptiesProbeStatusesRatherThanDeletingIt(t *testing.T) {
	t.Parallel()

	newPlan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n", Namespace: "ns",
			Annotations: map[string]string{PlanProbesPassedAnnotation: "2026-08-20T00:00:00Z"},
		},
		Data: map[string][]byte{
			"plan":           []byte(`{"instructions":[]}`),
			"probe-statuses": []byte(healthyProbeStatuses),
		},
	}

	c := &recordingSecretClient{}
	if _, err := (&Store{secrets: c}).AssignPlan(secret, newPlan, 1, -1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, ok := c.updated.Data["probe-statuses"]
	if !ok {
		t.Fatal("probe-statuses must be cleared by writing an empty value, not by deleting the key")
	}
	if len(v) != 0 {
		t.Errorf("probe-statuses: expected empty, got %q", v)
	}
}

// TestAssignPlan_DeleteAndRecreateAfterCancel covers the delete-and-recreate path: a user cancels an
// operation, deletes it, and creates an identical one, which generates byte-identical plan content.
// That recreated operation has to actually run.
//
// The trap is that identical content invalidates nothing on its own. The planChanged branch of
// AssignPlan never executes, so appliedPlan, applied-checksum and the probe results left behind by
// the run that was canceled all still describe the plan being delivered. Every completion signal
// therefore reads as satisfied on the very reconcile that redelivers the work, and the operation
// reports Success() before the agent has re-run a single instruction.
//
// The agent itself is not confused: decidePlanStateAction treats pending as NeedsApplied
// unconditionally, without consulting the applied checksum (system-agent
// pkg/k8splan/plan_decision.go:26-34), so it does re-run the plan. Only Rancher's reporting is
// wrong, which is the worst shape for this bug to take.
func TestAssignPlan_DeleteAndRecreateAfterCancel(t *testing.T) {
	t.Parallel()

	plan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshaling the plan: %v", err)
	}

	// The Secret as the canceled operation left it: the plan had been applied and its probes had
	// passed before the cancellation landed. Reachable because pause carries no terminal-state
	// guard, so succeeded -> paused -> canceled records canceled without ever touching
	// applied-checksum (system-agent pkg/k8splan/interrupt.go:263-277 and :234-239).
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n", Namespace: "ns",
			Annotations: map[string]string{
				PlanCanceledAnnotation:     "true",
				PlanProbesPassedAnnotation: "2026-08-20T00:00:00Z",
			},
		},
		Data: map[string][]byte{
			"plan":             planBytes,
			"appliedPlan":      planBytes,
			"applied-checksum": []byte(PlanHash(planBytes)),
			"probe-statuses":   []byte(healthyProbeStatuses),
			PlanStateKey:       []byte(PlanStateCanceled),
			PlanCheckpointKey:  []byte(`{"checksum":"` + Checksum(planBytes) + `","completedInstructions":1}`),
		},
	}

	c := &recordingSecretClient{}
	status, err := (&Store{secrets: c}).AssignPlan(secret, plan, 1, -1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.updates != 1 {
		t.Fatalf("expected exactly one Update, got %d", c.updates)
	}
	if got := string(c.updated.Data[PlanStateKey]); got != string(PlanStatePending) {
		t.Errorf("plan-state: expected pending, got %q", got)
	}
	if _, ok := c.updated.Annotations[PlanCanceledAnnotation]; ok {
		t.Error("the canceled annotation must be cleared by the same Update")
	}
	if !bytes.Equal(c.updated.Data["plan"], planBytes) {
		t.Error("the recreated operation delivers byte-identical content; it must not be rewritten")
	}

	// The work is outstanding until the agent re-runs it.
	if status.Success() {
		t.Error("the recreated operation must not report success before the agent has re-run the plan")
	}
	if !status.Waiting() {
		t.Error("the recreated operation must report that it is still waiting")
	}
	if !status.Pending {
		t.Errorf("expected a pending plan, got Pending=%v InProgress=%v", status.Pending, status.InProgress)
	}
	if status.Applied || status.ProbesPassed {
		t.Errorf("the canceled run's completion evidence must be retired, got Applied=%v ProbesPassed=%v",
			status.Applied, status.ProbesPassed)
	}

	// Retired data keys must be emptied rather than deleted: the agent's conflict-retry merge only
	// carries forward keys present in its in-hand copy, so a deleted key is restored on retry.
	for _, k := range []string{"appliedPlan", "applied-checksum", "probe-statuses", PlanCheckpointKey} {
		v, ok := c.updated.Data[k]
		if !ok {
			t.Errorf("%q must be cleared by writing an empty value, not by deleting the key", k)
			continue
		}
		if len(v) != 0 {
			t.Errorf("%q: expected empty, got %q", k, v)
		}
	}
	if got, ok := c.updated.Annotations[PlanProbesPassedAnnotation]; !ok || got != "" {
		t.Errorf("the probes-passed annotation must be reset to empty, got %q (present=%v)", got, ok)
	}

	// The retirement has to survive the plansecret controller, which reconciles the same Secret and
	// re-mirrors appliedPlan from any applied-checksum that still matches the plan. This is why
	// clearing appliedPlan alone, or only the probes annotation, is not enough: the evidence would
	// be restored before the agent had done anything.
	mirrored := mirrorAsPlansecret(t, status.Secret)
	if len(mirrored.Data["appliedPlan"]) != 0 {
		t.Error("plansecret re-mirrored appliedPlan: the source applied-checksum was not retired")
	}
	remirrored, err := (&Store{secrets: c}).AssignPlan(mirrored, plan, 1, -1)
	if err != nil {
		t.Fatalf("after the plansecret mirror: unexpected error: %v", err)
	}
	if remirrored.Success() || !remirrored.Waiting() {
		t.Errorf("after the plansecret mirror: expected the work to still be outstanding, got Success=%v Waiting=%v",
			remirrored.Success(), remirrored.Waiting())
	}

	// Retiring the evidence must not wedge the operation the other way: once the agent re-runs the
	// plan and reports success, the recreated operation has to complete.
	done := applyPlanAsLegacyAgent(status.Secret)
	done.Data[PlanStateKey] = []byte(PlanStateSucceeded)

	status, err = (&Store{secrets: c}).AssignPlan(done, plan, 1, -1)
	if err != nil {
		t.Fatalf("after the re-run: unexpected error: %v", err)
	}
	if c.updates != 1 {
		t.Fatalf("after the re-run: expected no further writes, got %d updates total", c.updates)
	}
	if !status.Success() || status.Waiting() || status.Pending {
		t.Errorf("after the re-run: expected the recreated operation to complete, got Success=%v Waiting=%v Pending=%v",
			status.Success(), status.Waiting(), status.Pending)
	}
}

// TestAssignPlan_DeleteAndRecreateAfterFailure is the failure-route sibling of
// TestAssignPlan_DeleteAndRecreateAfterCancel. Where that test covers a plan that had succeeded
// before the cancellation landed, this one covers a plan that had failed.
//
// The route is failed -> paused -> canceled: neither pause nor cancel carries a terminal-state
// guard in the agent, so a plan that already recorded failed-checksum and failure-count can still
// be paused and then canceled. Deleting the operation and re-creating it generates byte-identical
// plan content, so nothing invalidates the previous run's failure evidence implicitly — exactly
// the argument that already applies to appliedPlan and applied-checksum.
//
// Left in place, the failure evidence makes statusFromSecret report Failed (or Failing) on the
// very first reconcile of the recreated operation, before the agent has re-run a single
// instruction. Every operation controller branches on planStatus.Failure() immediately after
// AssignPlan — see etcdsnapshotsave/controller.go:734 — so the recreated operation is declared
// dead on arrival.
func TestAssignPlan_DeleteAndRecreateAfterFailure(t *testing.T) {
	t.Parallel()

	plan := &Plan{OneTimeInstructions: []OneTimeInstruction{{CommonInstruction: CommonInstruction{Name: "a"}}}}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshaling the plan: %v", err)
	}

	tests := []struct {
		name string
		// maxFailures and failureThreshold are the arguments the recreated operation passes. They
		// are not rewritten on this path, because the content did not change.
		maxFailures      int
		failureThreshold int
		failureCount     string
		threshold        string
	}{
		{
			// EncryptionKeyRotation's shape (controller.go:970): a bounded threshold that the
			// leftover count has already reached, so the recreated operation reports Failed.
			name:             "count has reached a bounded threshold",
			maxFailures:      5,
			failureThreshold: 5,
			failureCount:     "5",
			threshold:        "5",
		},
		{
			// The snapshot controllers' shape (controller.go:727): an unbounded threshold, where
			// the same leftover count reports Failing instead. Less fatal, but still a report of
			// a failure belonging to a run the user already canceled and replaced.
			name:             "count under an unbounded threshold",
			maxFailures:      1,
			failureThreshold: -1,
			failureCount:     "3",
			threshold:        "-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "n", Namespace: "ns",
					Annotations: map[string]string{PlanCanceledAnnotation: "true"},
				},
				Data: map[string][]byte{
					"plan":              planBytes,
					"failed-checksum":   []byte(PlanHash(planBytes)),
					"failure-count":     []byte(tt.failureCount),
					"failure-threshold": []byte(tt.threshold),
					"max-failures":      []byte(tt.threshold),
					PlanStateKey:        []byte(PlanStateCanceled),
					PlanCheckpointKey:   []byte(`{"checksum":"` + Checksum(planBytes) + `","completedInstructions":1}`),
				},
			}

			c := &recordingSecretClient{}
			status, err := (&Store{secrets: c}).AssignPlan(secret, plan, tt.maxFailures, tt.failureThreshold)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if c.updates != 1 {
				t.Fatalf("expected exactly one Update, got %d", c.updates)
			}
			if status.Failure() || status.Failing {
				t.Errorf("the canceled run's failure evidence must be retired, got Failed=%v Failing=%v",
					status.Failed, status.Failing)
			}
			if !status.Pending || !status.Waiting() {
				t.Errorf("the recreated operation must report outstanding work, got Pending=%v Waiting=%v",
					status.Pending, status.Waiting())
			}

			// Emptied, never deleted: the agent's conflict-retry merge only carries forward keys
			// present in its in-hand copy, so a deleted key is silently restored on retry.
			for _, k := range []string{"failed-checksum", "failure-count"} {
				v, ok := c.updated.Data[k]
				if !ok {
					t.Errorf("%q must be cleared by writing an empty value, not by deleting the key", k)
					continue
				}
				if len(v) != 0 {
					t.Errorf("%q: expected empty, got %q", k, v)
				}
			}

			// Retiring the evidence must not blind the controller to a genuine re-failure.
			refailed := status.Secret.DeepCopy()
			refailed.Data["failed-checksum"] = []byte(PlanHash(planBytes))
			refailed.Data["failure-count"] = []byte(tt.failureCount)
			refailed.Data[PlanStateKey] = []byte(PlanStateFailed)

			status, err = (&Store{secrets: c}).AssignPlan(refailed, plan, tt.maxFailures, tt.failureThreshold)
			if err != nil {
				t.Fatalf("after the re-run: unexpected error: %v", err)
			}
			if c.updates != 1 {
				t.Fatalf("after the re-run: expected no further writes, got %d updates total", c.updates)
			}
			if !status.Failed && !status.Failing {
				t.Error("after the re-run: a genuine failure must still be reported")
			}
		})
	}
}

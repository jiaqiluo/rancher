package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// echoSecretClient is the minimal SecretClient an AssignPlan call needs: it accepts the write and
// hands the Secret straight back, the way the API server would for a conflict-free update.
type echoSecretClient struct {
	corecontrollers.SecretClient
}

func (f *echoSecretClient) Update(s *corev1.Secret) (*corev1.Secret, error) {
	return s.DeepCopy(), nil
}

// legacyAssignPlan is the verbatim body of AssignPlan as of commit 64cb2ce94d, before plan-state
// existed. It is the oracle for TestAssignPlan_LegacyEquivalence and must not be "fixed" or kept
// in sync with AssignPlan — divergence from it is precisely what the test is looking for.
func legacyAssignPlan(s *Store, secret *corev1.Secret, plan *Plan, maxFailures, failureThreshold int) (*PlanStatus, error) {
	data, err := json.Marshal(&plan)
	if err != nil {
		return nil, err
	}

	secret = secret.DeepCopy()
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}

	result := &PlanStatus{Secret: secret}

	if !bytes.Equal(secret.Data["plan"], data) {
		result.Pending = true
		delete(secret.Data, "probe-statuses")
		secret.Annotations[PlanLastUpdatedAnnotation] = time.Now().UTC().Format(time.RFC3339)
		secret.Annotations[PlanProbesPassedAnnotation] = ""

		secret.Data["plan"] = data
		if maxFailures > 0 || maxFailures == -1 {
			secret.Data["max-failures"] = []byte(strconv.Itoa(maxFailures))
		} else {
			delete(secret.Data, "max-failures")
		}

		if failureThreshold > 0 || failureThreshold == -1 {
			secret.Data["failure-threshold"] = []byte(strconv.Itoa(failureThreshold))
		} else {
			delete(secret.Data, "failure-threshold")
		}

		secret, err = s.secrets.Update(secret)
		if err != nil {
			return nil, err
		}
		result.Secret = secret
	} else {
		result.Pending = false
		result.InProgress = true
	}

	probes := secret.Data["probe-statuses"]
	if probesPassed, ok := secret.Annotations[PlanProbesPassedAnnotation]; ok && probesPassed != "" {
		if len(probes) > 0 {
			_, healthy, err := ParseProbeStatuses(probes)
			if err != nil {
				return nil, err
			}
			result.ProbesPassed = healthy
		}
	}

	planData := secret.Data["plan"]
	failedChecksum := string(secret.Data["failed-checksum"])
	failureCount := secret.Data["failure-count"]

	if len(failureCount) > 0 && PlanHash(planData) == failedChecksum {
		failureCount, err := strconv.Atoi(string(failureCount))
		if err != nil {
			return nil, err
		}
		if failureCount > 0 {
			result.Failed = true
			rawFailureThreshold := secret.Data["failure-threshold"]
			if len(rawFailureThreshold) > 0 {
				failureThreshold, err := strconv.Atoi(string(rawFailureThreshold))
				if err != nil {
					return nil, err
				}
				if failureCount < failureThreshold || failureThreshold == -1 {
					result.Failed = false
					result.Failing = true
				}
			}
		}
	}

	if bytes.Equal(planData, secret.Data["appliedPlan"]) {
		result.Applied = true
	}

	if result.Applied || result.Failed {
		result.InProgress = false
	}

	return result, nil
}

// TestAssignPlan_LegacyEquivalence pins one specific thing: a single AssignPlan call on a Secret
// that carries no plan-state behaves exactly as it did before plan-state existed. That is the
// first reconcile of any machine-plan Secret belonging to a cluster whose system-agent predates
// this feature. Every caller of AssignPlan — the etcd snapshot save and restore controllers and
// the encryption key rotation controller, all in pkg/controllers/operations — reads only the six
// status booleans, so a change in how any of them is derived silently changes day-2 operation
// routing on every already-provisioned cluster.
//
// Its scope stops at that first call, and deliberately so: from the second reconcile onward such a
// Secret carries the plan-state: pending that AssignPlan itself just wrote, which is outside the
// plan-state == "" population this oracle can speak about. The rest of that sequence — a legacy
// agent applying the plan without ever advancing plan-state, and the operation having to converge
// anyway — is covered by TestAssignPlan_LegacyAgentConverges, not here.
//
// The test diffs the current AssignPlan against legacyAssignPlan, a verbatim copy of the
// implementation at 64cb2ce94d, over 1296 combinations of the Secret data and annotations that
// feed the derivation. It asserts three things for plan-state == "" inputs: identical error
// behavior, identical values for all six booleans, and identical Secret contents.
//
// "Identical Secret contents" is checked modulo exactly the two keys this feature adds: on a plan
// content change AssignPlan now also writes plan-state: pending and an emptied plan-progress. That
// addition is the point of the feature, so the test pins it precisely (the new Secret must equal
// the old one plus exactly those two keys, with exactly those values) rather than ignoring the
// keys. Annotation values are compared too, except for the last-updated timestamp, which is a
// wall-clock read and would make the comparison time-dependent.
func TestAssignPlan_LegacyEquivalence(t *testing.T) {
	t.Parallel()

	store := &Store{secrets: &echoSecretClient{}}
	p := &Plan{}
	data, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshaling the plan: %v", err)
	}
	otherPlan := []byte(`{"other":true}`)

	probes := []byte(`{"a":{"healthy":true,"successCount":1}}`)
	badProbes := []byte(`not json`)

	planVals := [][]byte{nil, data, otherPlan}
	appliedVals := [][]byte{nil, data}
	failedChecksums := [][]byte{nil, []byte(PlanHash(data))}
	failureCounts := [][]byte{nil, []byte("3"), []byte("nope")}
	thresholds := [][]byte{nil, []byte("5"), []byte("bad")}
	probeVals := [][]byte{nil, probes, badProbes}
	annVals := []map[string]string{nil, {PlanProbesPassedAnnotation: "t"}}
	maxFailuresVals := []int{0, -1}

	compared := 0
	for _, pv := range planVals {
		for _, av := range appliedVals {
			for _, fc := range failedChecksums {
				for _, fcnt := range failureCounts {
					for _, th := range thresholds {
						for _, pr := range probeVals {
							for _, ann := range annVals {
								for _, mf := range maxFailuresVals {
									d := map[string][]byte{}
									set := func(k string, v []byte) {
										if v != nil {
											d[k] = v
										}
									}
									set("plan", pv)
									set("appliedPlan", av)
									set("failed-checksum", fc)
									set("failure-count", fcnt)
									set("failure-threshold", th)
									set("probe-statuses", pr)
									secret := &corev1.Secret{
										ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: ann},
										Data:       d,
									}

									compared++
									desc := fmt.Sprintf("data=%v ann=%v maxFailures=%d", d, ann, mf)
									got, gotErr := store.AssignPlan(secret, p, mf, 3)
									want, wantErr := legacyAssignPlan(store, secret, p, mf, 3)
									assertLegacyEquivalent(t, desc, got, gotErr, want, wantErr, !bytes.Equal(pv, data))
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d combinations", compared)
}

// assertLegacyEquivalent diffs one AssignPlan result against the legacy oracle. planChanged says
// whether the plan content differed, which is the only case in which the current implementation is
// allowed to add keys to the Secret.
func assertLegacyEquivalent(t *testing.T, desc string, got *PlanStatus, gotErr error, want *PlanStatus, wantErr error, planChanged bool) {
	t.Helper()

	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("error parity: got %v, legacy %v (%s)", gotErr, wantErr, desc)
	}
	if gotErr != nil {
		return
	}

	format := func(s *PlanStatus) string {
		return fmt.Sprintf("Pending=%v InProgress=%v Applied=%v ProbesPassed=%v Failing=%v Failed=%v",
			s.Pending, s.InProgress, s.Applied, s.ProbesPassed, s.Failing, s.Failed)
	}
	if format(got) != format(want) {
		t.Fatalf("status mismatch\n got: %s\nwant: %s\n(%s)", format(got), format(want), desc)
	}

	if got.Secret == nil || want.Secret == nil {
		t.Fatalf("AssignPlan must always return the Secret it evaluated (%s)", desc)
	}

	extra := map[string][]byte{}
	if planChanged {
		extra[PlanStateKey] = []byte(PlanStatePending)
		extra[PlanCheckpointKey] = []byte{}
		// The legacy implementation deleted probe-statuses here; the current one empties it, per
		// the "clear by writing an empty value, never by deleting the key" rule the agent's
		// conflict-retry merge imposes. Pinned as an extra key with an exactly-empty value rather
		// than excluded from the comparison, so a regression back to the delete is caught.
		extra["probe-statuses"] = []byte{}
	}
	if len(got.Secret.Data) != len(want.Secret.Data)+len(extra) {
		t.Fatalf("secret data key count: got %v, legacy %v (%s)", got.Secret.Data, want.Secret.Data, desc)
	}
	for k, v := range want.Secret.Data {
		if _, ok := extra[k]; ok {
			t.Fatalf("legacy secret unexpectedly carries %q, the equivalence claim is scoped to plan-state == \"\" (%s)", k, desc)
		}
		if !bytes.Equal(got.Secret.Data[k], v) {
			t.Fatalf("secret data %q: got %q, legacy %q (%s)", k, got.Secret.Data[k], v, desc)
		}
	}
	for k, v := range extra {
		actual, ok := got.Secret.Data[k]
		if !ok {
			t.Fatalf("secret data %q must be present, and cleared by writing an empty value rather than by deleting the key (%s)", k, desc)
		}
		if !bytes.Equal(actual, v) {
			t.Fatalf("secret data %q: got %q, want %q (%s)", k, actual, v, desc)
		}
	}

	if len(got.Secret.Annotations) != len(want.Secret.Annotations) {
		t.Fatalf("annotations: got %v, legacy %v (%s)", got.Secret.Annotations, want.Secret.Annotations, desc)
	}
	for k, v := range want.Secret.Annotations {
		actual, ok := got.Secret.Annotations[k]
		if !ok {
			t.Fatalf("annotation %q was dropped (%s)", k, desc)
		}
		// The last-updated annotation is a wall-clock read taken independently by each
		// implementation, so only its presence is comparable.
		if k == PlanLastUpdatedAnnotation {
			continue
		}
		if actual != v {
			t.Fatalf("annotation %q: got %q, legacy %q (%s)", k, actual, v, desc)
		}
	}
}

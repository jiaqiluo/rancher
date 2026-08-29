package operations

import (
	"fmt"
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/plan"
	planv1alpha1 "github.com/rancher/rancher/pkg/plan/api/plan.cattle.io/v1alpha1"
	plancontrollers "github.com/rancher/rancher/pkg/plan/generated/controllers/plan.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/wrangler"
	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CleanupBudget bounds cleanup retrying, measured from DeletionTimestamp.
// Cleanup can fail permanently for reasons such as a missing cluster or RBAC errors.
// Do not retry forever. Give up after this budget elapses so the operation can be deleted.
// Make it a var so tests may shorten it.
var CleanupBudget = 2 * time.Minute

// CleanupBudgetExhausted returns true when DeletionTimestamp is older than the budget.
func CleanupBudgetExhausted(obj metav1.Object) bool {
	ts := obj.GetDeletionTimestamp()
	if ts == nil {
		return false
	}
	return time.Since(ts.Time) >= CleanupBudget
}

// clusterRefResolver is the subset of dynamic.Controller required to resolve ClusterRef.
type clusterRefResolver interface {
	Get(gvk schema.GroupVersionKind, namespace, name string) (runtime.Object, error)
}

// CleanupScope holds the data CleanupOperation needs from each controller.
//
// Populate it from an OnRemove handler. wrangler adds its own finalizer to every object on the
// first reconcile, which is what guarantees the handler observes the deletion at all. Rancher no
// longer manages a finalizer of its own for this.
type CleanupScope struct {
	// LogPrefix is the controller log tag, for example "etcdsnapshotsave".
	LogPrefix string

	// Object is the operation being deleted. Only its metadata is read.
	Object metav1.Object

	// ClusterRef identifies the cluster the operation acted on. A nil ref means nothing to clean.
	ClusterRef *corev1.ObjectReference

	Dynamic clusterRefResolver
	Clients *wrangler.CAPIContext
	Secrets corecontrollers.SecretClient

	// Beacons and OwnerKey release this operation's hold on the cluster beacon.
	// Leave Beacons nil or OwnerKey empty to skip beacon release.
	Beacons  plancontrollers.BeaconClient
	OwnerKey string

	// AfterRelease clears operation-type-specific ownership state from the beacon, such as
	// EncryptionKeyRotation's owner-ref annotation. It runs immediately after the release attempt,
	// on the same beacon, inside the same retry budget. It is not called when the cluster or the
	// beacon cannot be resolved, because then there is nothing to clear.
	AfterRelease func(beacon *planv1alpha1.Beacon) error
}

// CleanupOperation undoes the cluster-side effects of a deleting operation. It clears the interrupt
// annotations from the cluster's machine-plan Secrets, then releases the operation's hold on the
// cluster beacon.
//
// Call it from an OnRemove handler and return its error unchanged. wrangler removes its finalizer
// only on a nil return, so a non-nil error requeues the deletion with backoff. Once CleanupBudget
// elapses the failure is logged and swallowed, because a deleting operation must not become a
// deletion trap.
//
// Cleanup is best-effort. A missing cluster or a missing beacon is success: there is nothing left
// to clean.
func CleanupOperation(scope CleanupScope) error {
	namespace, clusterName, err := cleanupCluster(scope)
	switch {
	case err == nil || apierrors.IsNotFound(err):
		return nil
	case !CleanupBudgetExhausted(scope.Object):
		return err
	default:
		// A condition written here could not be reliably persisted: the same reconcile also lets
		// the finalizer go, so the object is about to disappear. Log lines carry
		// InterruptCleanupIncompleteReason for log-based alerting.
		logrus.Errorf("[%s] %s/%s: %s: %s",
			scope.LogPrefix, scope.Object.GetNamespace(), scope.Object.GetName(),
			opv1alpha1.InterruptCleanupIncompleteReason,
			CleanupFailureMessage(namespace, clusterName, err))
		return nil
	}
}

// cleanupCluster performs both cleanups in the only safe order.
//
// Annotations are cleared first and the beacon released last. Releasing the beacon lets the next
// operation acquire the cluster, so releasing while a paused annotation is still on the Secrets
// would hand a halted cluster to a new operation. The reverse failure is benign: a cleared
// annotation with a still-held beacon leaves the cluster no worse than before cleanup ran.
//
// The returned namespace and cluster name describe where the leftover state lives. They are
// reported even on failure so the operator has somewhere to look.
func cleanupCluster(scope CleanupScope) (string, string, error) {
	if scope.ClusterRef == nil {
		return "", "", nil
	}

	clusterObj, namespace, beaconName, err := ResolveClusterBeacon(scope.ClusterRef, scope.Dynamic, scope.Clients)
	if err != nil {
		return "", "", err
	}
	clusterName := clusterObj.GetName()

	collected, err := plan.NewCollector(scope.Secrets, clusterObj, namespace).Collect()
	if err != nil {
		return namespace, clusterName, err
	}
	if _, err := SyncInterruptAnnotations(scope.Secrets, collected, false, false); err != nil {
		return namespace, clusterName, err
	}

	return namespace, clusterName, releaseClusterBeacon(scope, namespace, beaconName)
}

// ResolveClusterBeacon resolves a ClusterRef into the cluster object and its beacon's namespace
// and name. Resolve every cluster-scoped artifact through the adapter rather than through the
// ClusterRef directly, matching what the phase handlers do: for an imported cluster the beacon
// lives in the namespace named after the cluster, which the ClusterRef does not carry.
//
// A NotFound error means the cluster is gone. Callers cleaning up after a deleted operation should
// treat that as nothing left to do.
func ResolveClusterBeacon(clusterRef *corev1.ObjectReference, dynamic clusterRefResolver,
	clients *wrangler.CAPIContext) (*unstructured.Unstructured, string, string, error) {
	gvk := schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind)
	ref, err := dynamic.Get(gvk, clusterRef.Namespace, clusterRef.Name)
	if err != nil {
		return nil, "", "", err
	}

	ustrMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ref)
	if err != nil {
		return nil, "", "", err
	}
	ustr := unstructured.Unstructured{Object: ustrMap}

	a, err := NewAdapter(clients, &ustr)
	if err != nil {
		return nil, "", "", err
	}
	clusterObj, err := a.ClusterObject()
	if err != nil {
		return nil, "", "", err
	}

	namespace, beaconName := a.BeaconRef()
	return clusterObj, namespace, beaconName, nil
}

// releaseClusterBeacon releases a deleting operation's hold on the cluster beacon.
//
// The terminal-phase handlers release the beacon themselves. This covers the case they cannot: an
// operation deleted while non-terminal still owns the beacon, and nothing else reclaims a foreign
// owner key, so every later day-2 operation on that cluster would wait forever in AcquireBeacon.
//
// A missing beacon is success. Holding neither the owner slot nor a delegate slot is success with
// no write, so cleaning up an operation that never acquired the beacon costs one cached read.
func releaseClusterBeacon(scope CleanupScope, namespace, beaconName string) error {
	if scope.Beacons == nil || scope.OwnerKey == "" {
		return nil
	}

	beacon, err := scope.Beacons.Get(namespace, beaconName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// Same guard the terminal-phase handlers use before releasing.
	if plan.IsOwningBeaconHolder(beacon, scope.OwnerKey) || plan.IsInDelegateChain(beacon, scope.OwnerKey) {
		if err := plan.ReleaseBeacon(beacon, scope.Beacons, scope.OwnerKey); err != nil {
			return err
		}
	}

	if scope.AfterRelease == nil {
		return nil
	}
	return scope.AfterRelease(beacon)
}

// CleanupFailureMessage returns the operator-facing message when cleanup fails.
// namespace and clusterName come from the adapter (BeaconRef and clusterObj.GetName()).
// Do not derive them from the operation's ClusterRef. If the cluster cannot be resolved,
// return discovery commands that search across all namespaces.
func CleanupFailureMessage(namespace, clusterName string, cause error) string {
	head := fmt.Sprintf("could not clear this cluster's leftover operation state within %s: %v. "+
		"Agents on those nodes may still be halted, and the cluster beacon may still be held, "+
		"which blocks every later day-2 operation", CleanupBudget, cause)

	if namespace == "" || clusterName == "" {
		return fmt.Sprintf("%s. The cluster could not be resolved, so its namespace is not known. "+
			"Find the machine-plan secrets with: kubectl get secret -A --field-selector type=%s ; "+
			"then clear each with: kubectl annotate secret -n <namespace> <name> %s- %s- ; "+
			"find the beacon with: kubectl get beacon -A",
			head, plan.SecretTypeMachinePlan, plan.PlanCanceledAnnotation, plan.PlanPausedAnnotation)
	}

	// Mirrors exactly what plan.NewCollector(client, clusterObj, namespace) selects: every Secret
	// in namespace carrying rke.cattle.io/cluster-name=<clusterName>.
	return fmt.Sprintf("%s; clear the annotations by hand with: "+
		"kubectl annotate secret -n %s -l %s=%s %s- %s- ; "+
		"then release the beacon with: kubectl patch beacon -n %s %s --subresource=status "+
		`--type=merge -p '{"status":{"owner":"","active":false,"delegates":null}}'`,
		head, namespace, capr.ClusterNameLabel, clusterName,
		plan.PlanCanceledAnnotation, plan.PlanPausedAnnotation,
		namespace, clusterName)
}

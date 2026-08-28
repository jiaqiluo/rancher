package operations

import (
	"fmt"
	"slices"
	"time"

	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/plan"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InterruptCleanupFinalizer protects interrupt annotations on machine-plan Secrets.
// Deleting an operation that wrote annotations can leave those Secrets paused.
// The finalizer prevents stranded paused Secrets when deleting the operation.
// Add the finalizer only when the operation writes its first interrupt annotation.
const InterruptCleanupFinalizer = "operation.cattle.io/interrupt-cleanup"

// InterruptCleanupBudget bounds cleanup retrying from DeletionTimestamp.
// Cleanup can fail permanently for reasons such as a missing cluster or RBAC errors.
// Do not retry forever. Drop the finalizer after this budget elapses.
// Make it a var so tests may shorten it.
var InterruptCleanupBudget = 2 * time.Minute

// HasInterruptFinalizer reports whether obj carries the cleanup finalizer.
func HasInterruptFinalizer(obj metav1.Object) bool {
	return slices.Contains(obj.GetFinalizers(), InterruptCleanupFinalizer)
}

// AddInterruptFinalizer adds the cleanup finalizer and returns true when it changed the object.
func AddInterruptFinalizer(obj metav1.Object) bool {
	if HasInterruptFinalizer(obj) {
		return false
	}
	obj.SetFinalizers(append(slices.Clone(obj.GetFinalizers()), InterruptCleanupFinalizer))
	return true
}

// AddInterruptFinalizerAndUpdate adds the cleanup finalizer and persists the update.
func AddInterruptFinalizerAndUpdate[T metav1.Object](obj T, update func(T) (T, error)) error {
	if !AddInterruptFinalizer(obj) {
		return nil
	}
	_, err := update(obj)
	return err
}

// RemoveInterruptFinalizer removes the cleanup finalizer and returns true when it changed the object.
func RemoveInterruptFinalizer(obj metav1.Object) bool {
	finalizers := obj.GetFinalizers()
	i := slices.Index(finalizers, InterruptCleanupFinalizer)
	if i < 0 {
		return false
	}
	obj.SetFinalizers(slices.Delete(slices.Clone(finalizers), i, i+1))
	return true
}

// InterruptCleanupBudgetExhausted returns true when DeletionTimestamp is older than the budget.
func InterruptCleanupBudgetExhausted(obj metav1.Object) bool {
	ts := obj.GetDeletionTimestamp()
	if ts == nil {
		return false
	}
	return time.Since(ts.Time) >= InterruptCleanupBudget
}

// InterruptCleanupFailureMessage returns the operator-facing message when cleanup fails.
// namespace and clusterName come from the collector (BeaconRef and clusterObj.GetName()).
// Do not derive them from the operation's ClusterRef. If the cluster cannot be resolved,
// return a discovery command that lists machine-plan Secrets across all namespaces.
func InterruptCleanupFailureMessage(namespace, clusterName string, cause error) string {
	head := fmt.Sprintf("could not clear the interrupt annotations from this cluster's machine-plan "+
		"secrets within %s: %v. Agents on those nodes may still be halted", InterruptCleanupBudget, cause)

	if namespace == "" || clusterName == "" {
		return fmt.Sprintf("%s, and the cluster could not be resolved, so their namespace is not "+
			"known. Find them with: kubectl get secret -A --field-selector type=%s ; then clear "+
			"each with: kubectl annotate secret -n <namespace> <name> %s- %s-",
			head, plan.SecretTypeMachinePlan, plan.PlanCanceledAnnotation, plan.PlanPausedAnnotation)
	}

	// Mirrors exactly what plan.NewCollector(client, clusterObj, namespace) selects: every Secret
	// in namespace carrying rke.cattle.io/cluster-name=<clusterName>.
	return fmt.Sprintf("%s; clear them by hand with: kubectl annotate secret -n %s -l %s=%s %s- %s-",
		head, namespace, capr.ClusterNameLabel, clusterName, plan.PlanCanceledAnnotation, plan.PlanPausedAnnotation)
}

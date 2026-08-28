package operations

import (
	"time"

	opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"
	"github.com/rancher/rancher/pkg/plan"
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

// clusterRefResolver is the subset of dynamic.Controller required to resolve ClusterRef.
type clusterRefResolver interface {
	Get(gvk schema.GroupVersionKind, namespace, name string) (runtime.Object, error)
}

// interruptCleanupObject is the subset of operation objects needed by CleanupInterrupts.
type interruptCleanupObject[T any] interface {
	metav1.Object
	DeepCopy() T
}

// interruptCleanupController is the controller interface CleanupInterrupts needs.
type interruptCleanupController[T interruptCleanupObject[T]] interface {
	EnqueueAfter(namespace, name string, after time.Duration)
	Update(obj T) (T, error)
}

// InterruptCleanupScope holds data and controller for best-effort finalizer cleanup.
// Use it when a deleting operation may still have interrupt annotations to clear.
type InterruptCleanupScope[T interruptCleanupObject[T]] struct {
	LogPrefix  string
	Object     T
	ClusterRef *corev1.ObjectReference
	Dynamic    clusterRefResolver
	Clients    *wrangler.CAPIContext
	Secrets    corecontrollers.SecretClient
	Controller interruptCleanupController[T]
}

// CleanupInterrupts clears interrupt annotations from the cluster's machine-plan Secrets.
// Then it removes the cleanup finalizer.
//
// Cleanup is best-effort and not a deletion trap. Removal rules:
// 1. Nothing to clean: clusterRef is nil or cluster resolves NotFound. Drop finalizer.
// 2. Cleanup succeeded. Drop finalizer.
// 3. Transient failure. Requeue for retries while the budget remains.
// 4. Budget exhausted. Drop finalizer and log leftover state for discovery.
//
// Rule 4 logs the failure instead of writing a status condition. A condition written here
// cannot be reliably persisted because the same reconcile also removes the finalizer.
// Log lines carry InterruptCleanupIncompleteReason for log-based alerting.
func CleanupInterrupts[T interruptCleanupObject[T]](scope InterruptCleanupScope[T]) error {
	if !HasInterruptFinalizer(scope.Object) {
		return nil
	}

	namespace, clusterName, err := ClearInterruptAnnotations(scope.ClusterRef, scope.Dynamic, scope.Clients, scope.Secrets)
	switch {
	case err == nil || apierrors.IsNotFound(err):
		// Rules 1 and 2
	case !InterruptCleanupBudgetExhausted(scope.Object):
		// Rule 3
		logrus.Debugf("[%s] %s/%s: retrying interrupt cleanup: %v",
			scope.LogPrefix, scope.Object.GetNamespace(), scope.Object.GetName(), err)
		scope.Controller.EnqueueAfter(scope.Object.GetNamespace(), scope.Object.GetName(), 5*time.Second)
		return nil
	default:
		// Rule 4
		logrus.Errorf("[%s] %s/%s: %s: %s",
			scope.LogPrefix, scope.Object.GetNamespace(), scope.Object.GetName(), opv1alpha1.InterruptCleanupIncompleteReason,
			InterruptCleanupFailureMessage(namespace, clusterName, err))
	}

	return RemoveInterruptFinalizerAndUpdate(scope.Object.DeepCopy(), scope.Controller)
}

// ClearInterruptAnnotations resolves clusterRef and clears interrupt annotations on machine-plan Secrets.
func ClearInterruptAnnotations(clusterRef *corev1.ObjectReference, dynamic clusterRefResolver, clients *wrangler.CAPIContext,
	secrets corecontrollers.SecretClient) (string, string, error) {
	if clusterRef == nil {
		return "", "", nil
	}

	gvk := schema.FromAPIVersionAndKind(clusterRef.APIVersion, clusterRef.Kind)
	ref, err := dynamic.Get(gvk, clusterRef.Namespace, clusterRef.Name)
	if err != nil {
		return "", "", err
	}

	ustrMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ref)
	if err != nil {
		return "", "", err
	}
	ustr := unstructured.Unstructured{Object: ustrMap}

	a, err := NewAdapter(clients, &ustr)
	if err != nil {
		return "", "", err
	}
	clusterObj, err := a.ClusterObject()
	if err != nil {
		return "", "", err
	}
	namespace, _ := a.BeaconRef()
	clusterName := clusterObj.GetName()

	collected, err := plan.NewCollector(secrets, clusterObj, namespace).Collect()
	if err != nil {
		return namespace, clusterName, err
	}

	_, err = SyncInterruptAnnotations(secrets, collected, false, false)
	return namespace, clusterName, err
}

// RemoveInterruptFinalizerAndUpdate removes the interrupt cleanup finalizer from obj (if present)
// and persists that mutation through the controller.
func RemoveInterruptFinalizerAndUpdate[T interruptCleanupObject[T]](obj T, controller interruptCleanupController[T]) error {
	if !RemoveInterruptFinalizer(obj) {
		return nil
	}
	_, err := controller.Update(obj)
	return err
}

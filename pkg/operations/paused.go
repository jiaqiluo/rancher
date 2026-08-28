package operations

import opv1alpha1 "github.com/rancher/rancher/pkg/apis/operation.cattle.io/v1alpha1"

// IsPaused returns true when the operation's Paused flag is set.
// Controllers must skip reconciliation for paused operations. The only allowed change is user-driven.
// The paused annotation is propagated to machine-plan Secrets. Cancel takes precedence.
func IsPaused(spec *opv1alpha1.OperationSpec) bool {
	return spec.Paused
}

# Pausing and cancelling day-2 operations

Applies to `Operation`-CRD-driven day-2 operations — `ETCDSnapshotSave`,
`ETCDSnapshotRestore` and `EncryptionKeyRotation` — on imported and CAPRKE2
(turtles-imported) clusters. Provisioning V2 clusters have their own
planner-embedded day-2 mechanism, which is not interruptible and is not covered
here.

Nothing stops you creating an `Operation` CR against a Provisioning V2 cluster:
the mgmt-cluster adapter resolves it to CAPR, so pause and cancel do reach it.
Be aware of the blast radius before you do. The interrupt annotations go on
every machine-plan Secret in the cluster, including the ones the CAPR planner
owns, and while `plan.cattle.io/paused` is set those nodes apply nothing at all
— provisioning and upgrades included, not just this operation. The planner
clears both annotations whenever it next writes plan content to a node, but see
"Stranded annotations" below for what that does and does not guarantee.

## Pausing

    kubectl patch etcdsnapshotsave -n <ns> <name> --type=merge -p '{"spec":{"paused":true}}'

Pausing stops the plan at the next instruction boundary on every node and
records a resume checkpoint. Probes keep running, so cluster health data stays
current. Clearing `spec.paused` resumes from the checkpoint rather than
restarting from the first instruction.

`PausedCondition` distinguishes two states. `PauseRequested` means Rancher has
written the annotation but not every node has stopped yet; `Paused` means every
node has confirmed. A pause that never confirms keeps reporting
`PauseRequested` indefinitely — it never times out, because nothing downstream
depends on the confirmation.

**A paused operation keeps holding the cluster's beacon**, and for
`EncryptionKeyRotation` it also leaves the CAPI cluster paused. It therefore
blocks every other day-2 operation on that cluster for as long as it stays
paused. Do not leave an operation paused indefinitely.

If Rancher cannot evaluate the pause at all, `PausedCondition` reports `PauseFailed`.
Rancher retries rather than override the request on a timer. Use resume or cancel to escape.
`ResumeFailed` on `PausedCondition` means the operation is still halted and Rancher is retrying.
`ResumeFailed` on `FailedCondition` means resume could not succeed and the operation was failed.

A resume that keeps failing *transiently* — an API blip, a Secret write that
keeps conflicting — retries indefinitely, exactly as every other step of these
controllers does, and reports `ResumeFailed` on `PausedCondition` the whole
time. Only a failure that cannot resolve on retry fails the operation. If a
resume is stuck retrying and you want the cluster back, `spec.cancel` is the
escape: unlike pause and resume it is bounded, and after fifteen minutes of
being unable to evaluate it Rancher drives the operation to `Canceled` anyway
and releases the beacon.

## Cancelling

    kubectl patch encryptionkeyrotation -n <ns> <name> --type=merge -p '{"spec":{"cancel":true}}'

Cancelling is permanent. `spec.cancel` cannot be unset — the API server rejects
`true -> false` — because these are one-shot, Job-like resources. To retry,
delete the operation and create a new one.

If both `spec.paused` and `spec.cancel` are set, cancel wins.

The operation remains `InProgress` until every node confirms or five minutes elapse.
This prevents releasing the beacon while a node may still be mid-instruction.
`CanceledCondition` uses `CancelRequested` while waiting.

Three reasons explain a cancellation that completed without full confirmation:

- `AgentConfirmationTimeout` — the named nodes did not report a terminal plan
  state in time.
- `LegacyPlanFlow` — the named nodes report no `plan-state` at all, so their
  agent predates plan-state support and ignores the interrupt annotations
  entirely. Rancher asked and cannot know whether anything listened. Upgrade
  those agents.
- `CancelEvaluationFailed` — Rancher could not evaluate the cancellation at all
  (a corrupt machine-plan Secret, or a Secret read that kept failing) and gave
  up rather than hold the cluster's beacon indefinitely. Recovery is reported
  conservatively here, because Rancher never found out what the nodes did.

## Recovering after a cancellation

`RecoveryRequiredCondition` reports whether the canceled operation may have
left the cluster needing manual attention. Cancelling an `ETCDSnapshotSave`
never does. Cancelling an `ETCDSnapshotRestore` or an `EncryptionKeyRotation`
may, and the condition's message says what to do — normally, run an
`ETCDSnapshotRestore`.

Rancher does **not** create that restore automatically. An etcd restore is
itself disruptive and hard to reverse; triggering one without explicit consent
is the wrong default.

The condition also fires, regardless of operation type, when a node reported
that processes from an interrupted instruction may still be running. That means
the node is not necessarily quiescent yet.

## Stranded annotations

Deleting an operation normally clears the interrupt annotations it wrote, via a
finalizer. Two cases bypass that:

- `kubectl delete --force --grace-period=0`, or manually stripping the
  finalizer.
- Cleanup failing for longer than its two-minute budget, in which case Rancher
  drops the finalizer anyway rather than leaving an undeletable object, and
  logs the exact command to run.

In either case, clear them by hand. The machine-plan Secrets live in the
namespace named after the downstream cluster, not in the operation's own
namespace:

    kubectl annotate secret -n <cluster-namespace> \
      -l rke.cattle.io/cluster-name=<cluster-name> \
      plan.cattle.io/canceled- plan.cattle.io/paused-

If the cluster itself is gone and you do not know the namespace, find the
Secrets first:

    kubectl get secret -A --field-selector type=rke.cattle.io/machine-plan

Do run it. A leftover annotation is not harmless, and nothing is guaranteed to
clear it for you:

- While `plan.cattle.io/paused` is set, the agent on that node executes no plan
  at all — not just this operation's. Every later day-2 operation, config change
  and upgrade stalls on it, reporting only that it is waiting for the node.
  `plan.cattle.io/canceled` takes precedence over `paused`: a node handed new
  plan content while it is still set records that plan as cancelled instead of
  executing it.
- The writers that do clear both annotations — `pkg/plan`'s `AssignPlan` and,
  for Provisioning V2, the CAPR planner's `UpdatePlan` — only touch the Secrets
  they are writing to, and any one step of an operation targets a filtered
  subset of the cluster's nodes. A node that nothing subsequently writes a plan
  to keeps its annotation indefinitely.

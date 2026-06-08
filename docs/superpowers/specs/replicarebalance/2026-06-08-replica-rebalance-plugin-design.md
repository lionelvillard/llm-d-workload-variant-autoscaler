# Replica Rebalance Plugin — Design

**Status:** Proposal
**Date:** 2026-06-08
**Coordinator plugin:** `replica-rebalance`
**Depends on:** [Coordinator Component Introduction](../coordinator/2026-06-05-coordinator-component-introduction.md) (PR [#1242](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1242))

## Context

The Coordinator dispatches "under control" scale targets — managed HPAs and KEDA ScaledObjects that do **not** consume `wva_desired_replicas` — to a registered set of plugins each tick. This proposal introduces the first plugin: `replica-rebalance`.

When several HPAs in a cluster are driven by metrics other than `wva_desired_replicas` (CPU, custom metrics, queue depth, etc.), and an upstream system rejects pod creation past some capacity (a `ResourceQuota`, Kueue, an admission controller, or any quota-management layer), each HPA's view is local: it sees its own metric, computes a desired replica count, and asks the workload to scale. There is no actor in the system whose job is to make sure the **collective** ask fits.

`replica-rebalance` fills that gap. It observes the empirical evidence of contention — `status.currentReplicas < status.desiredReplicas` on managed scale targets persisting across multiple ticks — and lowers `spec.maxReplicas` (HPA) / `spec.maxReplicaCount` (ScaledObject) on targets that hold ceiling they aren't using. When contention clears, it raises max back up to a recovery ceiling (set by annotation, or the value the plugin first observed if the annotation is absent).

## Goals

- React to persistent stuck scale-up at any managed scale target by reducing the upper replica bound on greedy peers.
- Restore the upper replica bound when the cluster has been quiet for a configured number of ticks.
- Stay strictly within the Coordinator plugin contract: leader-only, deterministic, optimistic concurrency on writes, no shared state with other plugins.
- Ship a minimal, auditable v0; keep the policy interface internal so future increments add greediness rules without redesigning the plugin.

## Non-goals

- Capacity modeling. The plugin makes no claim about what the cluster *can* run; it reads what HPAs say is happening.
- Per-namespace or per-pool budget enforcement.
- Pre-empting Pending pods or interacting with the kube-scheduler beyond setting replica counts.
- Multi-policy selection in v0. The plugin ships exactly one policy (`largest-headroom`); the policy registry exists in code but is not operator-visible.
- Cross-cluster coordination.

## Glossary

- **Scale target** — a managed `HorizontalPodAutoscaler` or KEDA `ScaledObject`. Selection rules are inherited from the Coordinator.
- **Contended target** — a scale target whose `status.currentReplicas < status.desiredReplicas` has held for K consecutive Coordinator ticks (default `K=3`).
- **Deficit** — `desiredReplicas − currentReplicas` summed across the contended set.
- **Recovery ceiling** — the upper bound up to which the plugin may raise a target's max during the recovery pass. Sourced from the `replicarebalance.wva.llm-d.ai/max-max-replicas` annotation when present, or the max observed at the moment the plugin first sees the target (in-memory; reset on leader failover).

## Component identity

A new Coordinator plugin under `internal/coordinator/plugins/replicarebalance/`, registered through the existing plugin host machinery. It satisfies the `coordinator.Plugin` interface from PR #1242 (`Name()`, `Tick(ctx, []client.Object)`).

The plugin handles both kinds in the selected slice:

- `*autoscalingv2.HorizontalPodAutoscaler` — lever is `spec.maxReplicas`.
- `*kedav1alpha1.ScaledObject` — lever is `spec.maxReplicaCount`. Status is read from KEDA's child HPA; the parent SO is the patch target.

## Contention detection

`status.currentReplicas` reflects the workload's actual pod count via the scale subresource. Pods rejected by an upstream quota system are never created, so they don't inflate `currentReplicas`. Persistent `currentReplicas < desiredReplicas` is therefore direct evidence that something upstream — typically a quota layer — is refusing to honor the HPA's request, which is the signal this plugin reacts to.

**Per-target contention rule:** a target is *contended* iff `status.currentReplicas < status.desiredReplicas` has held for K consecutive ticks (configurable, default `K=3`).

- HPA: read directly from `status`.
- ScaledObject: resolve to the KEDA-generated child HPA (`keda-hpa-<so-name>` or via `metav1.OwnerReferences` reverse lookup) and read status from there. Lookup failure → skip this tick for this target, don't fail the cycle.

**Why "K consecutive ticks":** a single tick of `current < desired` is normal during any in-flight scale-up. K consecutive ticks distinguishes "scaling up normally" from "blocked by quota / capacity." `K=3` with the default 15s Coordinator interval gives ~45s of confirmation.

**Known caveat (documented, not enforced):** this signal cannot distinguish quota-rejection from any *other* cause of stalled creation (controller bugs, finalizer deadlocks, webhook timeouts, image pull problems on a Deployment that scaled past one ready replica). When a target stays contended *after* the plugin has applied a write — the deficit doesn't shrink — operators should look elsewhere; rebalancing won't help. We surface this through metrics and ops docs.

**Cross-tick state:** an in-memory per-target `consecutiveStuck` counter keyed by `(kind, namespace, name)`, incremented when the rule holds, reset to 0 when it doesn't. Leader failover wipes the counter — acceptable; new leader needs at most K extra ticks before acting.

**Contended set:** every target whose counter ≥ K. The policy receives this set; it does not re-check contention.

**No-op short circuit:** if the contended set is empty, the plugin returns `nil` for the tick.

## Policy interface

The plugin owns the tick loop, contention filter, write path, damping, and invariants. The policy owns *who to adjust and by how much, in which direction*.

```go
// internal/coordinator/plugins/replicarebalance/policy/policy.go

type Policy interface {
    Name() string
    Decide(ctx context.Context, in Input) []MaxReplicasChange
}

type Input struct {
    Contended []ScaleTarget // targets past the K-tick contention threshold
    All       []ScaleTarget // every managed target the Coordinator selected this tick
}

type ScaleTarget struct {
    Kind            string // "HorizontalPodAutoscaler" | "ScaledObject"
    Key             types.NamespacedName
    MinReplicas     int32
    MaxReplicas     int32
    CurrentReplicas int32
    DesiredReplicas int32
    CurrentMetrics  []autoscalingv2.MetricStatus
    Annotations     map[string]string
    // Resolved from annotations by the plugin before policies see them.
    MinMaxReplicas int32 // floor for max (default = MinReplicas)
    MaxMaxReplicas int32 // ceiling for max (recovery ceiling)
}

type MaxReplicasChange struct {
    Key    types.NamespacedName
    Kind   string
    NewMax int32
    Reason string // policy-supplied; surfaces in events
}
```

A policy returns one entry per target it wants to adjust. Targets the policy chooses to leave alone get no entry. Both lower and raise are valid return values.

**Plugin invariants enforced after the policy returns:**

- `NewMax >= max(1, MinReplicas, MinMaxReplicas)`.
- `NewMax <= MaxMaxReplicas`.
- For any *cut* (`NewMax < observedMax`), `NewMax >= CurrentReplicas` — never force eviction.
- Per-target damping (default 60s) suppresses repeat writes.

A policy that violates an invariant has its decision dropped with a logged warning. The plugin does not silently clamp.

## v0 policy: `largest-headroom`

For each contended target, consider every non-contended target sorted by `(MaxReplicas - CurrentReplicas)` desc; lower the top one's max to `CurrentReplicas` (surrender unused ceiling) until the contended deficit is covered or candidates are exhausted. Cuts at most `maxCutsPerTick` targets per tick (default 1) to avoid thrash.

Other policies (`largest-current`, `fair-share`, `priority`, `metric-aware`) are deferred. The interface admits them; the registry keeps room. The v0 config does not expose a policy choice.

## Auxiliary recovery pass

Because `largest-headroom` does not natively model raises, the plugin runs a parallel pass each tick:

- For every managed target where `MaxReplicas < MaxMaxReplicas` and there has been **no contention anywhere in the cluster** for the past R ticks (default `R = 12`, i.e. `4 × K`), step max *up* by `recoveryStep` (default `+1`) toward `MaxMaxReplicas`.
- This guarantees no permanent ratchet-down.
- Recovery writes flow through the same write path, damping, and metrics with `direction="up"`.

Future policies that natively model raises (`fair-share`, `metric-aware`) opt out of this pass via a `WantsRecovery() bool` method on the `Policy` interface (defaults to `true`).

## Write path

Per `MaxReplicasChange` the policy returns:

1. **Validate against invariants.** On violation: drop, log a warning with the policy name and target key. Do not silently clamp.
2. **Skip no-ops.** `NewMax == observedMax` → drop silently.
3. **Damping check.** Per-target cache keyed by `(kind, namespace, name)`; if the last accepted write was less than `dampingInterval` ago (default 60s), skip. In-memory; resets on leader failover.
4. **Patch with optimistic concurrency.** JSON-merge patch on the right field for the kind, with `resourceVersion` precondition derived from the object observed this tick.
5. **Conflict handling.** On 409: re-fetch, re-apply invariants against fresh state, retry **once**. Second 409 → drop, increment `wva_replica_rebalance_conflicts_total{kind=...}`. No unbounded retry; next tick reconsiders.
6. **On success:** update damping cache, emit a `Normal ReplicaRebalanced "set max=<old>→<new>"` event on the target, increment `wva_replica_rebalance_writes_total{kind=..., direction=up|down}`.

**ScaledObject:** patch goes to the SO itself. KEDA re-derives the child HPA's `maxReplicas` from `spec.maxReplicaCount` on its next reconcile. Never patch the child HPA directly.

## RBAC (via Kustomize)

Following the project's Kustomize-first install pattern. No kubebuilder markers.

- **New:** `config/rbac/replica_rebalance_role.yaml` — `ClusterRole` named `replica-rebalance-plugin`:
  - `apiGroups: ["autoscaling"]`, `resources: ["horizontalpodautoscalers"]`, `verbs: ["get", "list", "watch", "patch"]`.
  - `apiGroups: ["keda.sh"]`, `resources: ["scaledobjects"]`, `verbs: ["get", "list", "watch", "patch"]`.
- **New:** `config/rbac/replica_rebalance_role_binding.yaml` — `ClusterRoleBinding` to the existing controller `ServiceAccount`.
- **Edit:** `config/rbac/kustomization.yaml` — add the two new files to `resources:`.

The `scaledobjects` rule references a CRD that may not be installed. We ship the rule unconditionally — RBAC referencing a non-existent CRD is harmless because the Coordinator's KEDA-discovery guard suppresses ScaledObject listing entirely when the CRD is absent.

The existing controller `ClusterRole` for HPA (which grants `get/list/watch/update` for the `wva_desired_replicas` reconciler path) is **not** modified. The new `patch` permission ships as a separate Kustomize-level resource so it's auditable as a unit and removable without touching unrelated rules.

## Configuration (v0)

```yaml
coordinator:
  plugins:
    replicaRebalance:
      enabled: false        # off by default
      contentionThreshold: 3   # K consecutive ticks
      dampingInterval: 60s
```

**Annotation (one, on the HPA or ScaledObject):**

- `replicarebalance.wva.llm-d.ai/max-max-replicas` — operator-set recovery ceiling. Plugin will not raise the target's max above this value. If absent, the recovery ceiling defaults to the value of max observed at first encounter (in-memory; reset on leader failover).

No `min-max-replicas`, no `weight`, no `priority`, no `exempt`. The floor for max is just `MinReplicas`. Out-of-band exemption is "remove the `llm-d.ai/managed: "true"` annotation," which the Coordinator already honors. Recovery cadence (`R`), recovery step, and `maxCutsPerTick` are constants in code; promoted to config in a follow-up only when an operator hits a case the defaults don't cover.

## Metrics (v0)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `wva_replica_rebalance_writes_total` | Counter | `kind`, `direction` (up/down) | Successful patches |
| `wva_replica_rebalance_contended_targets` | Gauge | — | Targets currently past the K-tick threshold |
| `wva_replica_rebalance_conflicts_total` | Counter | `kind` | Patches dropped after 409 + retry |

Damping events, invariant-violation counters, policy-decision counters, and the unrebalanceable-seconds histogram are deferred — they become valuable when there's a second policy or operator-tuned thresholds; in v0 they'd be diagnostic noise nobody reads.

## Events

- `Normal ReplicaRebalanced "set max=<old>→<new>"` on every successful write (cut or raise).

No warning events in v0. Failures are visible in logs and the conflicts counter.

## Logs (structured, via `ctrl.Log`)

- INFO per write: target, old/new max, direction.
- WARN on 409-after-retry drop.
- WARN on policy invariant rejection.
- DEBUG per skipped target during contention scoring or damping.

## Testing

**Unit (`internal/coordinator/plugins/replicarebalance/...`):**

- Contention detection with a fake clock and synthetic HPA status across K-tick windows; recovery; counter wipe on leader reset; ScaledObject path with stubbed child-HPA lookup.
- `largest-headroom` policy: pure `(input) -> []MaxReplicasChange`. Cases: single contended with one fat candidate, single contended with no candidates, multiple contended, candidates already at `CurrentReplicas`, `maxCutsPerTick` respected.
- Plugin invariants: a fake policy returns invalid `NewMax`; assert rejection and absence of a patch.
- Damping: two consecutive ticks with the same change → one patch; second goes through after `dampingInterval`; different targets independent.
- Recovery pass: target with `MaxReplicas < MaxMaxReplicas` and no contention for R ticks → one upward patch per tick to the ceiling. Contention anywhere → no upward patches.
- Write path: `controller-runtime` fake client. Clean patch; 409 + successful retry; 409 on both attempts; patch body shape (JSON-merge, only the right field).

**Envtest:**

- Coordinator manager with only `replicaRebalance` enabled. Two managed HPAs with mocked stuck status; assert lower-on-greedy within ~3 × interval; clear stuck status; assert recovery within R × interval.
- Same scenario with a managed ScaledObject, gated on KEDA CRDs (skipped otherwise, mirroring the `ScaledObjectReconciler` registration guard). Asserts `spec.maxReplicaCount` is the field patched.

**E2E:**

- New `make test-e2e-rebalance` target under the existing harness. Inference simulator + a synthetic quota that rejects pod creation past N replicas. Two pools, one HPA each, both on CPU. Drive load past quota; assert the plugin lowers max on the lower-traffic pool within ~1 minute and Pending pods clear.
- Document in `docs/developer-guide/testing.md`.
- All container images from `quay.io` / `registry.k8s.io` (project rule; no Docker Hub).

**Manual verification (PR description):**

- `enabled=false` → no goroutine, no writes, no metrics emitted.
- `enabled=true` and no contention → one debug-level tick, zero writes.
- Contention seeded → `wva_replica_rebalance_writes_total{direction="down"}` increments and a `Normal ReplicaRebalanced` event appears on the target.
- Contention cleared → `direction="up"` increments tracking the recovery cap.

## Rollout & migration

- v0 ships off by default. The Coordinator itself is also off by default (per PR #1242). Enabling this plugin while the Coordinator is disabled → startup error.
- No CRD changes, no new types, no changes to `wva_desired_replicas` flow, no changes to existing reconcilers.
- New RBAC is additive; an operator who skips the new Kustomize resources picks up zero permissions and the plugin will fail to write — but it's off by default, so this matters only at opt-in time.
- Removing the plugin: `enabled: false` + restart. No state lingers (damping cache and recovery ceilings are in-memory).

**Operator pre-flight:**

1. Set `replicarebalance.wva.llm-d.ai/max-max-replicas` on every managed HPA / ScaledObject the operator wants the plugin to be able to raise. Without it, the recovery ceiling is the max observed on first encounter — which may or may not match operator intent.
2. Confirm targets carry `llm-d.ai/managed: "true"` (already required by the Coordinator's selection rule).

A short ops note lands in `docs/developer-guide/replica-rebalance.md` covering when to turn it on, the one annotation, the three metrics, and the failure mode (deficit doesn't shrink → likely a non-quota stall; check pod conditions on the contended workload).

## Out of scope (deferred)

- Multiple selectable policies. Interface exists in code; only `largest-headroom` is wired in v0.
- Per-namespace or per-pool budgets / quotas.
- Cross-policy coordination, hot-swap, dynamic config reload.
- A dry-run mode that emits events without patching.
- Per-target weights, priorities, exemption annotations.

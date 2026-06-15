# Overprovisioning buffer for fast scale-up

**Status:** Draft
**Date:** 2026-06-15
**Audience:** WVA contributors, llm-d-router (EPP) contributors

## Problem

Model server cold start is slow (minutes). When demand bursts, HPA / KEDA computes
the right replica count quickly, but new pods are not Ready in time — the burst is
served by an undersized fleet and SLOs are missed.

We want a way to keep `N` extra warm pods on top of the scaler's "honest" target.
Extra pods are real, Ready, fully initialized model servers; they simply do not
receive traffic until they are needed.

## Goals

- Keep a configurable number of warm "buffer" pods over and above the scaler's
  target replica count.
- Hide buffer pods from EPP request routing.
- Promote a buffer pod to active in milliseconds when the system saturates.
- Scale down by terminating buffer pods first.
- Work with both HPA and KEDA `ScaledObject`.
- No new CRDs in v1. No changes to the deprecated `VariantAutoscaling` CRD.
- No changes to how WVA produces metrics for HPA/KEDA. The user's existing
  metrics pipeline keeps working.

## Non-goals (v1)

- Cross-pool / cross-variant orchestration.
- Predictive promotion based on historical load.
- A formal CRD for buffer policy.
- Solving metric dilution automatically (documented requirement; see Risks).

## Concepts

- **Active pod.** Has label `llmd.ai/role=active`. EPP routes requests to it.
- **Buffer pod.** Has label `llmd.ai/role=buffer`. EPP excludes it from candidates.
- **Active count (`A`).** Number of Ready pods with `role=active` in the
  Deployment selector.
- **Buffer target (`B`).** Computed from annotations. Number of Ready buffer
  pods we want to keep around.
- **Scale target.** The object that owns scaling for the Deployment: an
  `autoscaling/v2.HorizontalPodAutoscaler` or a `keda.sh/v1alpha1.ScaledObject`.
  Annotations and the dynamic min-replicas patch live on this object.

## High-level design

```
                 +----------------------+
                 |  HPA / ScaledObject  |
                 |  annotations:        |
                 |    buffer-min/pct/max|
                 +----------+-----------+
                            |
                            v
   +--------+        +-------------+      +---------+
   | Pods   |<------>| WVA buffer  |<---->| EPP     |
   | role=  |  patch | controller  | poll | metrics |
   | active |  label +-------------+      +---------+
   | buffer |              |                  |
   +--------+              v                  v
                      patch min          (read-only metric
                      replicas              already exposed)
                      on scale target
```

WVA owns all writes: pod labels, `pod-deletion-cost` annotations, and the
scale target's `minReplicas` / `minReplicaCount`.

EPP owns one read responsibility (saturation metric, already exposed) and one
filter responsibility (exclude `role=buffer` pods from candidates).

## Configuration surface

Annotations on the scale target (HPA or ScaledObject):

| Annotation | Type | Default | Meaning |
|---|---|---|---|
| `llmd.ai/buffer-min` | int | `0` | Absolute floor for buffer count. |
| `llmd.ai/buffer-percent` | non-negative int | `0` | Buffer as percent of `A`. |
| `llmd.ai/buffer-max` | int | unbounded | Absolute cap. |

Effective:
```
B = clamp(ceil(A * percent / 100), min, max)
```
If all annotations are absent or zero, `B = 0` and the feature is off.

A scale target with **no** WVA buffer annotations is ignored entirely.

### Saturation thresholds

WVA uses the scaler's **own external metric** as the saturation signal, polled
from EPP at WVA's existing fast-poll cadence (the same channel WVA already uses
for scale-from-zero). Specifically:

- For HPA: read `spec.metrics[*].external.metric.name` and `target.averageValue` /
  `target.value`.
- For KEDA: read the `triggers[*]` metric query and `metricThreshold`.

WVA uses that target as the threshold for promotion/demotion.

V1 hard-codes the supporting parameters (no annotations for them):

- **Tolerance** (`tol`): `0.1` (10%). Same idea as HPA's tolerance — avoid
  flapping near the target.
- **Demotion sustain window** (`D`): `60s`. Saturation must stay below
  `target * (1 - tol)` for this duration before WVA demotes one pod.

These can be made configurable in a follow-up if measured to be wrong.

## Components

### 1. `ScaleFloorTarget` interface (WVA)

Abstracts HPA vs KEDA so the rest of the controller is scaler-agnostic.

```go
type BufferPolicy struct {
    Min     int
    Percent int
    Max     int // <0 means unbounded
}

type SaturationSignal struct {
    MetricName    string
    Target        resource.Quantity
    Tolerance     float64 // e.g., 0.1 = 10%
}

type ScaleFloorTarget interface {
    // Identify the Deployment this target governs.
    DeploymentRef() types.NamespacedName

    // Read current buffer policy from annotations.
    Policy() BufferPolicy

    // Read the metric the user wired up so WVA can poll it for saturation.
    Saturation() SaturationSignal

    // Patch the scaler's "min" floor. No-op if value unchanged.
    PatchMin(ctx context.Context, n int) error
}
```

Implementations:

- `hpaTarget` — reads `autoscaling/v2.HPA`; patches `spec.minReplicas`.
- `kedaTarget` — reads `keda.sh/v1alpha1.ScaledObject`; patches `spec.minReplicaCount`.

A resolver per Deployment picks the implementation. If both an HPA *and* a
ScaledObject reference the same Deployment, the controller emits a status
condition `Conflicting` and skips reconciliation; KEDA owns its child HPA
already, so the user must remove the standalone HPA.

### 2. WVA buffer reconciler

A new controller in `internal/controller/buffer_reconciler.go`. It watches:

- Pods (label-indexed on `llmd.ai/role`)
- HPAs and ScaledObjects (filtered by presence of `llmd.ai/buffer-*` annotations)

Per scale target reconcile (slow loop, ~5s):

1. Resolve governed Deployment, list its pods.
2. `A = #(pods Ready, role=active)`.
3. Compute `B` from annotations.
4. **Bootstrap:** for any pod with no `llmd.ai/role` label, label it `active`
   (don't blackhole pre-existing fleets when the feature is enabled).
5. **Pod-deletion-cost:** ensure the `controller.kubernetes.io/pod-deletion-cost`
   annotation is `0` on `role=buffer` pods and `1000` on `role=active` pods.
6. Patch scale target's min to `max(user_min, A + B)`.

Per scale target fast loop (~hundreds of ms; uses EPP poll already in WVA):

1. Read saturation signal value `v` from EPP for the pool.
2. `target = SaturationSignal.Target` (same metric the scaler reads).
3. **Promote** when `v > target * (1 + tolerance)` AND a Ready `role=buffer`
   pod exists: pick the oldest such pod (longest-warm), patch label
   `role=buffer → role=active`, set `pod-deletion-cost=1000`.
   Promote at most one pod per tick.
4. **Demote** when `v < target * (1 - tolerance)` for `D` consecutive seconds
   (default 60s in v1, hard-coded) AND `A > floor`: pick the youngest
   `role=active` pod, patch label `role=active → role=buffer`, set
   `pod-deletion-cost=0`. Demote at most one pod per tick.

   `floor = max(1, user_min - B)` where `user_min` is the value captured in
   the `llmd.ai/user-min-replicas` annotation — see Bootstrap below for how
   the user's intended min is recovered.

Promotion is immediate (no sustain window) because the cost of promoting one
extra pod is bounded (one routable warm pod) and the cost of being late is high
(SLO miss). Demotion uses a sustain window because flapping has measurable
cost (label churn, EPP candidate-set churn, wasted scale-down attempts).

### 3. EPP filter (llm-d-router)

A new built-in candidate predicate in `pkg/epp/requestcontrol/candidates.go`:

```go
// ExcludeBufferLabelPredicate returns false for pods labeled
// llmd.ai/role=buffer. Used as the default Locate predicate when
// the --exclude-buffer-pods flag is set.
```

Wired in via a new EPP CLI flag `--exclude-buffer-pods` (default false). When
true, `DatastoreEndpointCandidates.Locate(...)` AND the candidate-cache key
both factor in the role label so candidate-set caches don't leak buffer pods
across requests.

EPP makes no other change. In particular:

- EPP does not write pod labels.
- EPP does not need extra RBAC.
- EPP does not implement promotion/demotion logic — that lives in WVA.

### 4. Bootstrap and "user min" preservation

The user's *original* `minReplicas` (or `minReplicaCount`) is the floor below
which WVA must not push the scale target. Once WVA starts patching this field
to `A + B`, the original value is gone.

On first observation of a scale target with buffer annotations and no
`llmd.ai/user-min-replicas` annotation:

1. Read the current `spec.minReplicas` (HPA) or `spec.minReplicaCount` (KEDA).
2. Patch annotation `llmd.ai/user-min-replicas: "<value>"` onto the scale target.
3. From then on, `min` we write is `max(user_min, A + B)`.

If the user later changes their intended min, they update the annotation
explicitly (we don't try to detect "is this WVA-written or user-written").

## Lifecycles

### Cold start from zero

```
t=0   Deployment min=1, A=0, B=0 (no buffer until A>0 or annotations enable it)
      Pod p1 starts; WVA observes it; bootstrap labels p1 role=active.
      pod-deletion-cost=1000.
      A=1, B = clamp(ceil(1 * pct/100), min, max).
      WVA patches scaler min = 1 + B.
      Scaler eventually starts B more pods → labeled role=buffer by WVA.
```

### Burst

```
t=0   A=5, B=2. Buffer pods b1, b2 are Ready and warm.
t=Δ   Saturation metric jumps above target * (1+tol).
      WVA fast loop: promote b1 → active. EPP picks it up within ~50ms
      (existing candidate cache TTL).
t=2Δ  Still saturated → promote b2 → active. A=7, no buffer left.
t=3Δ  WVA slow loop: A=7, B=2 ⇒ min=9. Scaler spawns 2 fresh pods,
      labeled buffer when first observed.
```

### Scale down

```
A=5, B=2, total=7. Demand drops.
EPP saturation metric falls below target * (1-tol).
After D=60s sustained, WVA demotes one active → buffer. A=4, B=2.
Next slow loop: min = 4 + 2 = 6. Scaler also recommends 4 (since the metric
filtered to role=active also dropped). minReplicas=6 floors recommendation;
scaler terminates 1 pod, picking lowest pod-deletion-cost = a buffer pod.
Process repeats one demotion at a time until A matches honest demand.
```

## Data model

No CRDs.

Pod labels (written by WVA only):

- `llmd.ai/role` — `active` | `buffer`

Pod annotations (written by WVA only):

- `controller.kubernetes.io/pod-deletion-cost` — `0` (buffer) | `1000` (active)

Scale target annotations:

- `llmd.ai/buffer-min`, `llmd.ai/buffer-percent`, `llmd.ai/buffer-max` (input, user-written)
- `llmd.ai/user-min-replicas` (output, WVA-written, see Bootstrap)

## Failure modes

| Failure | Behavior |
|---|---|
| WVA crashes mid-promotion (label patch in-flight) | Idempotent: next reconcile re-evaluates label set against `A` and corrects. |
| User removes buffer annotations on a running scale target | WVA stops patching min. Existing labels remain (user can clean up). Scaler recovers. |
| Scaler is both HPA and KEDA-managed | `Conflicting` condition; WVA skips. |
| EPP filter not enabled but buffer pods exist | Buffer pods receive traffic; promotion still works but feature is degraded. Document loudly. |
| Pod selected for promotion is not Ready (race) | Predicate filters by Ready; if none Ready, promotion fails this tick, retried next tick. |
| Metric is averaged over all pods (not active-only) | Metric dilution → scaler under-scales. Documented; see Risks. |

## Risks and open questions

1. **Metric dilution.** If the scaler's metric is averaged across all pods in
   the Deployment selector, idle buffer pods drag the average down. **Mitigation:
   require the user to filter the scaler's metric query by `llmd.ai/role=active`.**
   HPA external/object metrics support a `metric.selector`; KEDA scalers can
   include the label match in their PromQL. Document loudly. Optionally
   (future) emit a startup warning if WVA detects a metric query without the
   role filter.

2. **EPP filter must be enabled.** If the cluster runs llm-d-router without
   `--exclude-buffer-pods`, buffer pods receive traffic and the feature
   degrades silently. WVA should expose a status condition `EPPFilterUnknown`
   when buffer pods are first labeled, asking the operator to confirm. (V1: no
   active probing; just a once-per-pool log line and condition.)

3. **Promotion latency.** Promotion = label patch + EPP candidate cache
   refresh (~50ms). Path is: EPP metric → WVA fast poll → label patch →
   EPP watch → candidate set update. Worst case dominated by WVA's poll
   interval. Document this; tune the interval downward if measured to be too
   slow in production.

4. **`pod-deletion-cost` is a hint, not a guarantee.** Kubernetes scheduler
   "tries" to honor it but isn't required to. If the scheduler ever picks an
   active pod for termination, WVA detects the missing pod and the feature
   self-heals on the next slow loop (a buffer pod is promoted to fill the
   gap). Acceptable degradation.

5. **`user-min-replicas` annotation drift.** If the user changes their
   intended floor via the scale target's spec while WVA is the active writer,
   the annotation goes stale. V1 documents that the annotation is the source
   of truth once set; updating intended min means updating the annotation.

6. **CRD-less coordination.** All state lives in pod labels, annotations on
   the scale target, and the scale target's spec. No new etcd objects. This
   is the right tradeoff for v1 but limits introspection (no `kubectl get
   bufferpolicies`). Future iterations may introduce a CRD if usage demands it.

## Integration points

### llm-d-workload-variant-autoscaler (this repo)

- New package `internal/controller/buffer/` with the reconciler and
  `ScaleFloorTarget` implementations.
- New RBAC: `pods/patch` on the namespaces being managed; `get/list/watch/patch`
  on `horizontalpodautoscalers` and `keda.sh/scaledobjects`.
- Reuse the existing fast-poll EPP metric channel.

### llm-d-router

- New built-in predicate `ExcludeBufferLabelPredicate` in
  `pkg/epp/requestcontrol/candidates.go`.
- New CLI flag `--exclude-buffer-pods` in `pkg/epp/server/options.go`.
- Datastore must already index pods by label (it does, via the existing
  pod-watch path) — add the role label to the Locate cache key when the flag
  is on.
- No RBAC changes.

### KEDA / HPA users

- Add the buffer annotations to the existing scale target.
- **Required:** ensure the scaler's metric query filters by `llmd.ai/role=active`.
- Restart EPP with `--exclude-buffer-pods` (or set in EPP config).

## Out of scope (explicitly)

- New CRDs.
- Promotion based on predictions or historical load.
- Cross-Deployment / cross-variant promotion logic.
- A web UI / dashboard for buffer state. (Pod labels + standard metrics suffice.)
- Auto-tuning the buffer policy. The admin sets min/percent/max.
- Coupling to WVA's saturation/queueing model. The scaler's metric is enough
  for v1.

## Testing strategy

Unit:

- `BufferPolicy.Compute` math (rounding, clamping, zero-cases).
- `ScaleFloorTarget` resolver: HPA-only, KEDA-only, both (Conflicting),
  neither (skip).
- Bootstrap labeling rule.
- Promotion / demotion pod selection (oldest buffer, youngest active, tie-break
  by name).

Integration (envtest):

- Reconcile loop drives `min` correctly on label transitions.
- `pod-deletion-cost` is set on every label transition.
- `user-min-replicas` annotation is captured exactly once.

E2E (Make target):

- Simulator-based: trigger saturation; verify a buffer pod is promoted and
  EPP starts routing to it within the documented latency budget.
- Verify scale-down terminates buffer pods first (check pod names against
  `pod-deletion-cost`).

## Migration / rollout

- Feature is off by default (no annotations ⇒ no behavior change).
- llm-d-router upgrade can ship with `--exclude-buffer-pods=false` default;
  users opt in per-EPP.
- Existing Deployments work as-is. WVA labels their pods `active` on first
  observation only when buffer annotations are added to the scale target.
- Removing the feature: delete buffer annotations; WVA stops patching `min`;
  operator manually clears pod labels (or lets them age out via pod
  replacement).

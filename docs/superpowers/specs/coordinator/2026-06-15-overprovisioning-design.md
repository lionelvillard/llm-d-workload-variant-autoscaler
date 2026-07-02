# Overprovisioning buffer for fast scale-up (KEDA)

**Status:** Draft (revised 2026-07-02 — KEDA-only)
**Date:** 2026-06-15
**Audience:** WVA contributors, llm-d-router (EPP) contributors

## Problem

Model server cold start is slow (minutes). When demand bursts, KEDA computes
the right replica count quickly, but new pods are not Ready in time — the burst is
served by an undersized fleet and SLOs are missed.

We want a way to keep `N` extra warm pods on top of KEDA's "honest" target.
Extra pods are real, Ready, fully initialized model servers; they simply do not
receive traffic until they are needed.

## Goals

- Keep a configurable number of warm "buffer" pods over and above KEDA's
  target replica count.
- Hide buffer pods from EPP request routing.
- Promote a buffer pod to active in milliseconds when the system saturates.
- Scale down by terminating buffer pods first.
- Integrate with KEDA `ScaledObject` as the only supported scale target in v1.
- No new CRDs. No changes to the deprecated `VariantAutoscaling` CRD.
- No changes to the operator's existing metrics pipeline; the same signal
  KEDA reads becomes the buffer controller's saturation signal.

## Non-goals (v1)

- Direct HPA support without KEDA. (Users on a bare `HorizontalPodAutoscaler`
  are not supported in v1. KEDA's `ScaledObject` is the only supported entry
  point. Reason: KEDA already generates and owns the underlying HPA; supporting
  both surfaces doubles the abstraction cost for little user value in-scope.)
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
- **Scale target.** The `keda.sh/v1alpha1.ScaledObject` that governs scaling
  for the Deployment. Buffer annotations and the dynamic `minReplicaCount`
  patch live on this object.
- **Saturation signal.** A numeric value re-derived by WVA using the
  `ScaledObject`'s own trigger definition (see below). Compared against the
  same threshold KEDA compares against.

## High-level design

```
                +--------------------------------+
                | keda.sh/v1alpha1.ScaledObject  |
                |   annotations:                 |
                |     buffer-min/pct/max         |
                |   spec.triggers[]:             |
                |     prometheus query+threshold |
                |   spec.advanced.scalingModifiers?
                +----------------+---------------+
                                 |
                                 v
       +--------+        +--------------+       +-----------+
       | Pods   |<------>|  WVA buffer  |<------|Prometheus |
       |        |  patch |  controller  | query |           |
       | role=  |  label +--------------+       +-----------+
       | active |              |
       | buffer |              v
       +--------+   patch spec.minReplicaCount
                    on the ScaledObject
                    (KEDA propagates to its child HPA)
```

WVA owns all writes: pod labels, `pod-deletion-cost` annotations, and
`ScaledObject.spec.minReplicaCount`.

EPP owns one filter responsibility (exclude `role=buffer` pods from
candidates). EPP is not on the buffer-management critical path — it neither
writes labels nor decides promotion/demotion.

## Configuration surface

Annotations on the `ScaledObject`:

| Annotation | Type | Default | Meaning |
|---|---|---|---|
| `llmd.ai/buffer-min` | non-negative int | `0` | Absolute floor for buffer count. |
| `llmd.ai/buffer-percent` | non-negative int | `0` | Buffer as percent of `A`. |
| `llmd.ai/buffer-max` | non-negative int | unbounded | Absolute cap. |

Effective:
```
B = clamp(ceil(A * percent / 100), min, max)
```
If all annotations are absent or zero, `B = 0` and the feature is off.

A `ScaledObject` with **no** WVA buffer annotations is ignored entirely.

### Saturation signal

WVA re-derives the same signal KEDA uses. The read path is:

1. Read `ScaledObject.spec.triggers[]`.
2. Pick the target trigger:
   - If `spec.advanced.scalingModifiers` is set, all named triggers in the
     formula are candidates (see below).
   - Otherwise, pick the first trigger with `type: prometheus`. If none exist,
     emit `UnsupportedTriggerConfig` and skip.
3. For each selected trigger, read `metadata.serverAddress`,
   `metadata.query`, `metadata.threshold`, and `metadata.activationThreshold`.
4. Poll Prometheus directly at WVA's fast-poll cadence (same client WVA
   already uses for scale-from-zero). Each trigger yields a scalar value.

For a single-trigger case: `v = value(trigger)`, `target = trigger.threshold`.

For the `scalingModifiers` case: WVA evaluates the `formula` string with the
same expression grammar KEDA uses (github.com/expr-lang/expr, formerly
antonmedv/expr; the KEDA-supported flavor). Values are bound to the trigger
names. `v = eval(formula)`, `target = scalingModifiers.target`. See
"scalingModifiers support" below for the scope and limits.

WVA does not read KEDA's fallback metric or reach into the generated HPA — it
does the same Prometheus query KEDA does and compares to the same threshold.

Supporting parameters (hard-coded in v1):

- **Tolerance** (`tol`): `0.1` (10%). Same idea as HPA's tolerance — avoid
  flapping near the target.
- **Demotion sustain window** (`D`): `60s`. Saturation must stay below
  `target * (1 - tol)` for this duration before WVA demotes one pod.

These can be made configurable in a follow-up if measured to be wrong.

### scalingModifiers support

If the `ScaledObject` sets `spec.advanced.scalingModifiers.formula`, WVA
executes the formula itself using the [`expr`](https://github.com/expr-lang/expr)
library — the same one KEDA uses. Each named trigger in the formula is
resolved by running its Prometheus query; the resulting scalars are bound as
variables named after the trigger's `name` field.

Restrictions in v1:

- Every trigger referenced in the formula must have `type: prometheus`. If
  any referenced trigger is non-Prometheus (e.g., `kubernetes-workload`,
  `metrics-api`), WVA emits `UnsupportedTriggerConfig` and skips.
- The `metricType` field of `scalingModifiers` is honored only informationally
  (all Prometheus scalars are treated as numeric).
- The formula must be pure (no side effects) — enforced by `expr`'s default
  environment.
- If any trigger's Prometheus query fails, WVA treats the ScaledObject as
  "signal-degraded": it stops promotion and demotion for that cycle and keeps
  the current label set stable, but continues to enforce the `minReplicaCount`
  floor. Mirrors KEDA's behavior when its own scalers are unhealthy — KEDA
  does not evaluate the formula while any referenced trigger is in fallback.

### KEDA-specific behaviors WVA respects

- **`autoscaling.keda.sh/paused` / `paused-replicas`.** If either is set on
  the `ScaledObject`, WVA yields: it stops patching `minReplicaCount` and
  stops promotion/demotion. Labels remain in place. When the annotation is
  removed, WVA resumes on its next reconcile.
- **`spec.fallback`.** If WVA cannot reach Prometheus, it enters the same
  signal-degraded state as above. WVA does not attempt to detect whether
  KEDA itself has fallen back (that would require reading its generated HPA
  status); it just stops moving until its own signal returns.
- **`spec.idleReplicaCount` / scale-to-zero.** When the workload is scaled
  to zero by KEDA (activation cleared), the buffer disappears with it — there
  are no pods to label. When traffic resumes and KEDA lifts the replica count
  to `minReplicaCount`, WVA labels the first pod `active`, then patches the
  min to `active + B` as usual. Documented: buffer does not survive
  scale-to-zero.
- **`spec.pollingInterval`.** WVA's fast poll runs at its own cadence
  (independent of KEDA's, and typically faster than KEDA's default 30s). This
  is intentional; the whole point of the feature is to react faster than KEDA
  alone.
- **`spec.advanced.horizontalPodAutoscalerConfig.behavior`.** Operator-owned
  knob for HPA behavior (e.g., scale-down stabilization). WVA does not read
  or write it, but the docs call it out as a complementary tuning surface.

## Components

### 1. WVA buffer reconciler

A new controller in `internal/controller/buffer/`. It watches:

- `keda.sh/v1alpha1.ScaledObject` (filtered by presence of
  `llmd.ai/buffer-*` annotations)
- Pods matching each managed ScaledObject's target Deployment
  (label-indexed on `llmd.ai/role`)

Per ScaledObject reconcile (slow loop, ~5s):

1. Resolve `spec.scaleTargetRef` to the governed Deployment; list its pods.
2. `A = #(pods Ready, role=active)`.
3. Compute `B` from annotations.
4. **Bootstrap labels:** for any pod with no `llmd.ai/role` label, label it
   `active` (don't blackhole pre-existing fleets when the feature is
   enabled). Uses a patch on the Pod object, no restart.
5. **Bootstrap `user-min-replicas`:** if the ScaledObject does not carry
   `llmd.ai/user-min-replicas`, snapshot `spec.minReplicaCount` (or `0` if
   unset) into that annotation.
6. **Pod-deletion-cost:** ensure the `controller.kubernetes.io/pod-deletion-cost`
   annotation is `0` on `role=buffer` pods and `1000` on `role=active` pods.
7. Patch `ScaledObject.spec.minReplicaCount = max(user_min, A + B)`. No-op if
   unchanged.

Per ScaledObject fast loop (poll cadence, order-of hundreds of ms):

1. Resolve the trigger(s) and compute `v` (see Saturation signal).
2. **Promote** when `v > target * (1 + tolerance)` AND a Ready `role=buffer`
   pod exists: pick the oldest such pod (longest-warm), patch label
   `role=buffer → role=active`, set `pod-deletion-cost=1000`.
   Promote at most one pod per tick.
3. **Demote** when `v < target * (1 - tolerance)` for `D` consecutive seconds
   AND `A > floor`: pick the youngest `role=active` pod, patch label
   `role=active → role=buffer`, set `pod-deletion-cost=0`. Demote at most
   one pod per tick.
   `floor = max(1, user_min - B)`.

Promotion is immediate (no sustain window) because the cost of promoting one
extra pod is bounded (one routable warm pod) and the cost of being late is
high (SLO miss). Demotion uses a sustain window because flapping has
measurable cost (label churn, EPP candidate-set churn, wasted scale-down
attempts).

### 2. Saturation evaluator

A small package `internal/controller/buffer/saturation/` implements:

```go
type Evaluator interface {
    // Evaluate resolves the ScaledObject's trigger(s) and returns the
    // saturation ratio v/target. > 1 means saturated.
    Evaluate(ctx context.Context, so *kedav1.ScaledObject) (ratio float64, err error)
}
```

Two paths inside:

- Single-trigger fast path: query Prometheus, divide by threshold.
- Formula path: for each named trigger, query Prometheus; bind values;
  evaluate `spec.advanced.scalingModifiers.formula` via
  `github.com/expr-lang/expr`; divide by `scalingModifiers.target`.

Reuses WVA's existing Prometheus client (`internal/collector` or equivalent);
no new HTTP client wiring.

### 3. EPP filter (llm-d-router)

A new built-in candidate predicate in `pkg/epp/requestcontrol/candidates.go`:

```go
// ExcludeBufferLabelPredicate returns false for pods labeled
// llmd.ai/role=buffer. Applied to the default Locate() path when
// the --exclude-buffer-pods flag is set.
```

Wired via a new EPP CLI flag `--exclude-buffer-pods` (default `false`). When
true, `DatastoreEndpointCandidates.Locate(...)` AND the candidate-cache key
both factor in the role label so cache entries don't leak buffer pods across
requests.

EPP makes no other change. In particular:

- EPP does not write pod labels.
- EPP does not need extra RBAC.
- EPP does not implement promotion/demotion logic — that lives in WVA.

## Lifecycles

### Cold start (KEDA lifts from zero)

```
t=0   ScaledObject present, minReplicaCount=1, no pods (idle at 0).
      Traffic arrives → KEDA activates → replica count → 1.
t=Δ   Pod p1 becomes Ready. WVA observes it.
      Bootstrap labels p1 role=active. pod-deletion-cost=1000.
      A=1, B = clamp(ceil(1 * pct/100), min, max).
      WVA patches ScaledObject.spec.minReplicaCount = max(user_min, 1+B).
      KEDA respects the new floor → spawns B more pods → labeled role=buffer.
```

### Burst

```
t=0   A=5, B=2. Buffer pods b1, b2 are Ready and warm.
t=Δ   Saturation metric jumps above target * (1+tol).
      WVA fast loop: promote b1 → active. EPP picks it up within ~50ms
      (existing candidate cache TTL).
t=2Δ  Still saturated → promote b2 → active. A=7, no buffer left.
t=3Δ  WVA slow loop: A=7, B=2 ⇒ min=9. KEDA lifts replicas to 9 (via its
      generated HPA); new pods → labeled buffer when first observed by WVA.
```

### Scale down

```
A=5, B=2, total=7. Demand drops.
Saturation ratio falls below (1-tol).
After D=60s sustained, WVA demotes one active → buffer. A=4, B=2.
Next slow loop: min = 4 + 2 = 6.
Meanwhile the metric (filtered to role=active in PromQL) shows lower per-pod
load — KEDA's own scaling recommends 4. minReplicaCount=6 floors it; the
resulting scale-down terminates 1 pod, picking lowest pod-deletion-cost
(= a buffer pod). Repeats one demotion at a time until A matches honest demand.
```

## Data model

No CRDs.

Pod labels (written by WVA only):

- `llmd.ai/role` — `active` | `buffer`

Pod annotations (written by WVA only):

- `controller.kubernetes.io/pod-deletion-cost` — `0` (buffer) | `1000` (active)

ScaledObject annotations:

- `llmd.ai/buffer-min`, `llmd.ai/buffer-percent`, `llmd.ai/buffer-max` (input, user-written)
- `llmd.ai/user-min-replicas` (output, WVA-written; first-observation snapshot)

## Failure modes

| Failure | Behavior |
|---|---|
| WVA crashes mid-promotion (label patch in-flight) | Idempotent: next reconcile re-evaluates label set against `A` and corrects. |
| User removes buffer annotations on a running ScaledObject | WVA stops patching min. Existing labels remain (user can clean up). KEDA recovers to `user_min`. |
| KEDA `ScaledObject` is paused (annotation set) | WVA yields — no min-writes, no promotion/demotion. |
| ScaledObject references non-Prometheus trigger (single-trigger case) | Emit `UnsupportedTriggerConfig`; skip. |
| ScaledObject uses `scalingModifiers` with any non-Prometheus trigger | Emit `UnsupportedTriggerConfig`; skip. |
| Prometheus query fails / times out | Signal-degraded: stop promoting/demoting; keep enforcing the current min. Alert on repeated failures. |
| EPP filter not enabled but buffer pods exist | Buffer pods receive traffic; promotion still works but feature is degraded. Documented; status condition. |
| Pod selected for promotion is not Ready (race) | Predicate filters by Ready; if none Ready, promotion fails this tick, retried next tick. |
| Metric is averaged over all pods (not active-only) | Metric dilution → KEDA under-scales. Documented; see Risks. |
| KEDA scales workload to zero | Buffer disappears with it. Documented: buffer does not survive scale-to-zero. |

## Risks and open questions

1. **Metric dilution.** If the trigger's PromQL averages across all pods in
   the Deployment selector, idle buffer pods drag the average down and KEDA
   under-scales. **Mitigation: the operator must filter the trigger's PromQL
   by `llmd.ai/role="active"`.** This is a documented requirement.
   Optionally (future) emit a warning if WVA detects a query without the
   `role` filter.

2. **EPP filter must be enabled.** If the cluster runs llm-d-router without
   `--exclude-buffer-pods`, buffer pods receive traffic and the feature
   degrades silently. WVA exposes a status condition `EPPFilterUnknown`
   when buffer pods are first labeled, asking the operator to confirm. (V1:
   no active probing; log line and condition only.)

3. **Promotion latency.** Promotion = label patch + EPP candidate cache
   refresh (~50ms). Path is: Prometheus query → WVA fast poll → label patch →
   EPP watch → candidate set update. Worst case dominated by WVA's poll
   interval. Document this; tune the interval downward if measured to be
   too slow.

4. **`pod-deletion-cost` is a hint, not a guarantee.** Kubernetes scheduler
   "tries" to honor it but is not required to. If an active pod is
   terminated anyway, WVA detects the gap and promotes a buffer pod to
   refill it — acceptable, self-healing degradation.

5. **`user-min-replicas` annotation drift.** If the user changes their
   intended floor via the ScaledObject spec while WVA is the active writer,
   the annotation goes stale. V1 documents that the annotation is the
   source of truth once set; updating intended min means updating the
   annotation.

6. **Formula compatibility with KEDA.** WVA re-implements what KEDA does
   internally when it evaluates `scalingModifiers.formula`. This is a
   duplication risk: if KEDA changes its expr environment or adds custom
   functions, WVA's evaluation could diverge. V1 restricts the expression
   surface to pure numeric expressions on Prometheus trigger values (no
   custom functions), which is a strict subset of what KEDA allows. If the
   user's formula uses features outside that subset, WVA emits
   `UnsupportedFormula` and skips.

7. **CRD-less coordination.** All state lives in pod labels, ScaledObject
   annotations, and the ScaledObject spec. No new etcd objects. This is
   the right tradeoff for v1 but limits introspection (no
   `kubectl get bufferpolicies`). A CRD may follow if usage demands it.

## Integration points

### llm-d-workload-variant-autoscaler (this repo)

- New package `internal/controller/buffer/` with the reconciler.
- New package `internal/controller/buffer/saturation/` with the trigger
  evaluator (Prometheus-only in v1) and `expr` formula runner.
- New RBAC:
  - `pods`: `get`, `list`, `watch`, `patch`
  - `keda.sh/scaledobjects`: `get`, `list`, `watch`, `patch`
  - No new HPA verbs — WVA never touches KEDA's generated HPA directly.
- New Go dependencies: `github.com/kedacore/keda/v2/apis/keda/v1alpha1`
  (types only), `github.com/expr-lang/expr`.
- Reuse existing Prometheus client for query execution.

### llm-d-router

- New built-in predicate `ExcludeBufferLabelPredicate` in
  `pkg/epp/requestcontrol/candidates.go`.
- New CLI flag `--exclude-buffer-pods` in `pkg/epp/server/options.go`.
- Datastore already indexes pods; add the `role` label to the Locate cache
  key when the flag is on.
- No RBAC changes.

### KEDA users

- Add the buffer annotations to the existing ScaledObject.
- **Required:** the trigger's PromQL must filter by
  `llmd.ai/role="active"` so KEDA scales on active-pod load only.
- Restart EPP with `--exclude-buffer-pods` (or set in EPP config).

## Out of scope (explicitly)

- Direct `HorizontalPodAutoscaler` support (no KEDA). Deferred to a follow-up
  if user demand justifies the abstraction cost.
- New CRDs.
- Promotion based on predictions or historical load.
- Cross-Deployment / cross-variant promotion logic.
- A web UI / dashboard for buffer state. (Pod labels + standard metrics suffice.)
- Auto-tuning the buffer policy. The admin sets min/percent/max.
- Coupling to WVA's saturation/queueing model. KEDA's trigger metric is
  enough for v1.

## Testing strategy

Unit:

- `BufferPolicy.Compute` math (rounding, clamping, zero-cases).
- Saturation evaluator: single Prometheus trigger; formula with two named
  triggers; non-Prometheus trigger → error; formula referencing an unknown
  trigger name → error; Prometheus failure → signal-degraded.
- Bootstrap labeling rule.
- `user-min-replicas` snapshot happens exactly once.
- Promotion / demotion pod selection (oldest buffer, youngest active,
  tie-break by name).
- Paused annotation short-circuits the reconciler.

Integration (envtest with KEDA CRDs):

- Reconcile loop patches `minReplicaCount` correctly on label transitions.
- `pod-deletion-cost` is set on every label transition.
- `user-min-replicas` annotation is captured exactly once and honored on
  subsequent reconciles.
- `ScaledObject` deletion cleans up controller state.

E2E (Make target):

- KEDA + Prometheus stack + simulated model server. Trigger saturation;
  verify a buffer pod is promoted and EPP starts routing to it within the
  documented latency budget.
- Verify scale-down terminates buffer pods first (assert pod names against
  `pod-deletion-cost`).
- Pause the ScaledObject mid-flight; verify WVA yields.

## Migration / rollout

- Feature is off by default (no annotations ⇒ no behavior change).
- llm-d-router upgrade ships with `--exclude-buffer-pods=false` default;
  users opt in per-EPP.
- Existing Deployments work as-is. WVA labels their pods `active` on first
  observation only after buffer annotations are added to the ScaledObject.
- Removing the feature: delete buffer annotations; WVA stops patching min;
  operator manually clears pod labels (or lets them age out via pod
  replacement).

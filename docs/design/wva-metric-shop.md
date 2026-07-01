# WVA as a "metric shop": decomposing the scaling decision

## Purpose

This document explores refocusing WVA from an all-in-one autoscaler into a
**metric shop** — a producer of the specific metrics that a general-purpose
autoscaler (KEDA) needs in order to scale llm-d workloads. It builds on
[wva-vs-keda-decider.md](./wva-vs-keda-decider.md), which establishes that
KEDA's `expr` scaling-modifier formula *aggregates* signals while WVA
*optimizes*.

The central question: `wva_desired_replicas` is an all-encompassing metric that
folds every concern into one number. Can it be **decomposed** into more granular,
load-oriented metrics — letting KEDA do the final assembly — and if so, **where
should each granular metric be produced**: vLLM, EPP, or WVA as a last resort?

## Layered decomposition

The scaling decision splits into three layers, distinguished by *what state each
needs*:

| Layer | Metric | State it needs | Natural home |
|---|---|---|---|
| **L0 — Raw load** | request rate, in/out tokens, running/waiting queue, KV-cache usage | Per-pod only | **vLLM** (native), **EPP** (per-pool aggregate) |
| **L1 — Per-variant demand** | SLO-feasible replica count for *this* variant at current load, on its accelerator | This variant's load **+ a performance model** | **EPP** (see below) or **WVA** |
| **L2 — Coordinated allocation** | `wva_desired_replicas` (+ accelerator choice) | Multiple variants' demand + shared GPU budget + priority + cost (intra-model and/or cross-model — see below) | **KEDA `expr`** (some policies) or **WVA** (others) |

The assembly relationship is:
`desired_i = coordinate(L1 demand of all variants, L2 constraints)`.

A key observation: **in unlimited mode — today's default — L2 is a no-op.** Each
variant simply receives its own L1 demand; there is no cross-variant arbitration
because capacity is assumed infinite. So today `wva_desired_replicas ≈ L1`, and
any KEDA "assembly" is already a pass-through. L2 only bites in **limited mode**
(a fixed GPU budget shared across variants).

### Variant vs. pool: two coordination scopes

"Variant" and "pool" are not the same level, and the difference splits L2 in two.
A **variant** is a model on one accelerator arrangement; a single **model** can
have several variants (e.g. Llama-8B on A100 *and* on H100), and WVA groups
variants by model. An **InferencePool / EPP** hosts one base model, so it spans
**all variants of that model**. The per-variant scaling unit is the
Deployment ↔ ScaledObject ↔ HPA (1:1:1):

```
Model  ══ InferencePool ══ EPP        (1 : 1 : 1)
 ├─ Variant: A100  ── Deployment ── ScaledObject ── HPA
 └─ Variant: H100  ── Deployment ── ScaledObject ── HPA
```

So EPP is **not** single-variant — it is single-*model*, spanning that model's
variants. That distinction splits L2 into two scopes:

| L2 scope | Coordinates | Widest scope that can see it |
|---|---|---|
| **L2-intra** | variants *of the same model* (A100 vs H100 mix, P/D split) | **EPP** — it sees all pods/variants in its pool |
| **L2-cross** | *across models* (shared GPU budget, inter-model priority) | **No single EPP** — WVA, or `expr` over all EPPs |

Whether these two scopes are cleanly separable, or coupled through shared
physical GPU pools under a hard budget, depends on real llm-d pool composition
(can one pool's selector span multiple accelerator deployments?). **That topology
question is unresolved and deliberately out of scope here**; the layering below is
written to hold either way.

## Where L1 should live: the EPP finding

We analyzed the EPP (Endpoint Picker) codebase
([llm-d-router](https://github.com/llm-d/llm-d-router)) to determine whether it
could host the L1 performance model. Findings:

**EPP already produces L0 and the per-pool aggregation.** It maintains
per-endpoint `KVCacheUsagePercent`, `WaitingQueueSize`, `RunningRequestsSize`,
and emits pool-level aggregates — `llm_d_epp_average_kv_cache_utilization`,
`llm_d_epp_average_queue_size`, `llm_d_epp_average_running_requests`,
`llm_d_epp_ready_endpoints` — plus TTFT / ITL / TPOT histograms per model. It even
carries a **roofline saturation model**
(`saturation = max(queue_ratio, kv_cache_ratio)`) and a clean producer/consumer
**plugin registry** where a new estimator can be registered.

**EPP is architecturally confined to a single InferencePool.** Per its
architecture docs: *"Single InferencePool and single EPP due to Envoy
limitations."* One base model per pool, one EPP per pool, a datastore that is *"a
local cache for the given InferencePool"* with **no cross-pool visibility**.

**The scopes line up.** L1 (per-variant demand) needs only that variant's load;
EPP hosts every variant of its model and already collects those signals per pod,
so it can produce L1 for each of its variants. L2-cross (across models) needs a
view no single EPP has. So:

- **L0 → vLLM / EPP** (already there).
- **L1 → EPP**, as a plugin. EPP already aggregates the load signals; it is
  missing only the load→replicas mapping, which its plugin framework is built to
  accept. Everything WVA emits today that is *per-variant and load-derived*
  (`wva_required_capacity`, `wva_saturation_utilization`, `wva_spare_capacity`) is
  a candidate to migrate down into EPP.
- **L2 → KEDA or WVA** (next section).

**Placement trade-off, stated honestly.** Pushing L1 into EPP means the
benchmarking-derived performance profiles (α + β·batch fits, saturation bounds;
see [modeling-optimization.md](./modeling-optimization.md)) must be delivered to
*every* EPP instance rather than living centrally in WVA, and it splits ownership
of the performance model into a data-plane component. The alternative — keep L1
in WVA and emit per-variant demand as a granular metric — centralizes the model
but keeps WVA on the simple path. This is a placement decision, not a capability
limit: EPP provably *can* host L1.

**Governance criterion for adding metrics to EPP.** The right filter is: *how
relevant is the metric to scheduling?* This draws a clean line:

- Per-variant load/demand signals (queue, KV, latency-derived, and an
  SLO-feasible demand estimate) **are** scheduling-relevant — EPP already uses
  queue/KV/latency for endpoint selection. Emitting them is a coherent extension.
- The L2 rationing *policy* is **not** scheduling-relevant — EPP would never
  consume its own `desired_replicas`. It does not belong in EPP.

## Where L2 should live: is it assemblable by `expr`?

The real L2 question is not "is WVA the right thing" but "**can the coordinated
allocation be assembled from per-EPP metrics + the expression language, so no WVA
binary needs to ship?**" (Note: KEDA scaling modifiers use
[`expr-lang`](https://expr-lang.org/), not CEL.)

### The mechanism: "global metric" is a PromQL aggregation, not a component

No single EPP needs a global view:

1. Each EPP emits a per-variant series for every variant of its model —
   `epp_variant_demand{variant=…}`, plus `gpu_per_replica`, `priority`, `cost` as
   metrics/labels — all computable from its own pool's pods.
2. Prometheus scrapes **all** EPPs — the global picture now exists as a set of
   labeled series.
3. The "global metric" is a **PromQL query** over that set:
   `sum(epp_variant_demand)`, `sum(epp_variant_demand{priority > mine})`, etc.
   Aggregation happens in PromQL, in no binary.
4. The GPU budget `G` is a separate metric (DCGM / node-exporter / static).

So the L2 *inputs* are all present in Prometheus without any WVA process.

### The dividing line: aggregate-decomposable vs. full-vector policies

**Assemblable in `expr` (no WVA binary):**

- **Proportional rationing** — `desired_i = min(d_i, d_i · G / sum(d))`. Inputs:
  `d_i`, `sum(d)`, `G` — **three triggers, independent of N.** Scales as variants
  come and go.
- **Priority-preemptive rationing** — variant *i*'s formula reads one aggregate,
  `sum(demand{priority > mine})`, then `min(d_i, G − that)`. Bounded by the number
  of priority *classes*, not variants.

These work because every ScaledObject runs the **same deterministic formula over
the same PromQL aggregates**, so their independent outputs are mutually
consistent *by construction* — no negotiation.

**Not assemblable in `expr`:**

- **True cost-minimizing combinatorial allocation** — "give the marginal GPU to
  the most cost-efficient variant, iterate." Needs the full per-variant vector and
  an iterative solve. `expr` cannot ingest a dynamic-length vector (each trigger is
  one scalar → O(N) triggers per ScaledObject → O(N²) hand-edited config) and has
  no loop for the greedy iteration.
- **WVA's *current* `cost-aware` optimizer is in this camp** — it greedily compares
  all variants' cost/capacity ratios. So today's WVA policy is *not*
  `expr`-expressible. That is a property of the chosen policy, not of L2 in general.

### Consequence for packaging

> If the coordination policy is **aggregate-decomposable** (proportional or
> priority-tiered rationing), the "global optimizer" dissolves into: per-EPP L1
> emission + PromQL aggregation + one identical `expr` formula per ScaledObject.
> **No WVA binary.** WVA-the-process is required only when the policy needs a
> full-vector solve — or when the consistency guarantee below is required.

## Consistency: the property the distributed scheme cannot guarantee

The remaining reason to keep a WVA binary even for `expr`-expressible policies is
**joint feasibility of the allocation** — not "atomicity" in a strict sense.

An allocation `[r_A, r_B, …]` is *feasible* per variant when each count meets that
variant's own SLO and bounds; it is *jointly* feasible when the counts also
satisfy the shared constraint — total GPUs used, `Σ rᵢ·gpusᵢ`, fits the budget
`G`. Two individually-correct counts can still be jointly infeasible (A wants 7,
B wants 6, but only 10 GPUs exist). This is the property below.

### Neither system reads all variants atomically

WVA's metric collection is **not** atomic either: its optimizer assembles a demand
vector by scraping many pods/Prometheus across a collection window, so `demand_A`
may be from t=5s and `demand_B` from t=8s in the same solve. The distinction is
narrower than atomic-vs-not:

- **WVA:** inputs are skewed across one collection window, but the **decision is a
  single solve** → the *output* vector is jointly feasible (shares always sum ≤ G)
  against that one snapshot, however skewed. **One decider, one window.**
- **Distributed `expr`/HPA:** inputs are skewed **and** each variant's share is
  computed in a *different* HPA evaluation, against a *different* snapshot.
  **N deciders, N windows.**

So the right lens is the **worst-case width of the inconsistency window** across
all ScaledObjects/HPAs.

### Verified KEDA + HPA control flow

- KEDA's `keda-operator` watches all ScaledObjects but, for each, only **manages
  one HPA** and directly handles the **0↔1** edge. It does **not** compute the 1→N
  replica count. (KEDA's admission webhook forbids two ScaledObjects targeting one
  workload — strict 1:1.)
- For **1→N**, the decision is delegated to the **stock Kubernetes HPA
  controller** in `kube-controller-manager`. Verified from the Kubernetes docs: it
  is a **single control loop that runs intermittently** (not continuous),
  processing **each HPA once per sync period** (`--horizontal-pod-autoscaler-sync-period`,
  **default 15s**), issuing a **per-HPA metrics query** against its own
  `scaleTargetRef`. Each HPA is **evaluated independently**; there is no shared
  snapshot and no cross-HPA barrier.
- Formula: `desiredReplicas = ceil[ currentReplicas × (currentMetricValue /
  desiredMetricValue) ]`, skipped within a 0.1 default tolerance.
- KEDA's `pollingInterval` (default 30s) gates the 0→1 path and, **only if
  `useCachedMetrics: true` (default `false`)**, the value served to the HPA.
  With the default (fresh metrics), each HPA query hits a fresh PromQL evaluation.

### Worst-case inconsistency window

**Distributed `expr`/HPA (defaults):**

| Source of skew | Default contribution |
|---|---|
| Prometheus scrape phase skew between variants | ≤ ~15–30s |
| HPA evaluation skew (each HPA reconciles on its own phase) | ≤ 15s |
| Metric staleness if `useCachedMetrics: true` | + up to 30s (`pollingInterval`) |
| Scale-**down** stabilization: each HPA holds the max desired over the window | up to **300s** (`behavior.scaleDown.stabilizationWindowSeconds`, k8s default) |

- Transient over-request window (scale-up, default fresh metrics):
  ~30s + ~15s ≈ **~45s**; with cached metrics ≈ **~75s**. During this window two
  variants' live shares were computed against demand readings up to ~45–75s apart,
  so independently-computed shares can transiently sum to **more than G**
  (oversubscription → pending pods).
- Scale-**down** tail: each HPA holds its max desired for up to **300s**, so the
  sum of *held* allocations can exceed G for up to ~5 minutes after demand drops —
  an asymmetric effect independent of input skew.

**WVA:** bounded by the collection sweep + optimization interval
(`GLOBAL_OPT_INTERVAL`, default 60s). Inputs may be up to ~one sweep old, but the
allocation **never self-contradicts** — a single solve divides the fixed `G`, so
the output is jointly feasible by construction.

### Conclusion

> The argument is not *atomic vs. non-atomic*. It is **bounded single-solve skew
> (~60s, always jointly feasible) vs. compounded multi-loop skew (~45–75s + a 300s
> scale-down stabilization tail, feasibility not guaranteed).** The distributed
> scheme's per-variant shares are each individually correct but never guaranteed
> to jointly fit `G`; WVA's are jointly feasible by construction.

**Caveats:**

1. These are worst-case windows under default tuning; typical skew is much
   smaller, and stabilization windows can be tuned down (at the cost of churn).
2. Oversubscription is self-correcting and often harmless in cloud (pending pods →
   cluster autoscaler). It bites only under a **hard, fixed GPU budget** with no
   burst headroom — which is exactly WVA limited-mode's target scenario.

## Summary: the metric-shop division of labor

| Layer | Home | Justification |
|---|---|---|
| **L0** raw load | vLLM / EPP | already emitted; per-pod and per-pool |
| **L1** per-variant demand | **EPP plugin** | EPP hosts every variant of its model and collects the signals per pod; scheduling-relevant; plugin seam exists |
| **L2** aggregate rationing (proportional / priority-tiered) | **KEDA `expr`** over PromQL aggregates of all EPPs | no binary needed; consistent by construction |
| **L2-intra** per-model variant mix (A100 vs H100) | **EPP** (unlimited) / **WVA** (if coupled to a hard budget) | in scope of one EPP unless it competes cross-model for shared GPUs |
| **L2-cross** full-vector cost optimization, or hard-budget joint feasibility | **WVA binary** | exceeds `expr`; no single EPP sees it; needs single-snapshot solve |

WVA-the-binary is therefore **not** "the home of L2." It is the home of the
**subset of L2 policies that exceed `expr` + PromQL** — chiefly **cross-model**
coordination and hard-budget joint feasibility. Per-model variant mix (L2-intra)
is EPP-scoped except where a hard shared budget couples it to cross-model
allocation. For everything else, the metric shop is: EPP produces L1, PromQL
aggregates it, and one KEDA `expr` formula per ScaledObject assembles the result.

## References

- [wva-vs-keda-decider.md](./wva-vs-keda-decider.md) — expressiveness comparison
- [modeling-optimization.md](./modeling-optimization.md) — WVA modeling & optimization
- [controller-behavior.md](./controller-behavior.md) — WVA reconciliation & intervals
- EPP: <https://github.com/llm-d/llm-d-router>
- KEDA ScaledObject spec: <https://keda.sh/docs/latest/reference/scaledobject-spec/>
- KEDA concepts (operator / metrics adapter): <https://keda.sh/docs/latest/concepts/>
- Kubernetes HPA: <https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/>
- expr language: <https://expr-lang.org/docs/language-definition>

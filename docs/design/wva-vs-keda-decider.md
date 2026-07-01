# WVA optimizer vs. KEDA scaling-modifier formula

## Purpose

Both KEDA and WVA contain a **decider**: a component that turns observed signals
into a scaling decision. This document is the **side-by-side comparison** of the
two deciders. It is the companion to
[wva-metric-shop.md](./wva-metric-shop.md), which uses this comparison to decide
*where each metric and each policy should live* (vLLM, EPP, KEDA `expr`, or the
WVA binary).

Scope note, to keep the comparison honest:

- This compares the **decision/assembly step only** — the "L2" layer in the
  metric-shop decomposition. The **performance model** (L1: mapping load to an
  SLO-feasible per-variant replica count) is *separable* from both deciders and
  can live in EPP; it is **not** an intrinsic property of either decider, so it is
  not the axis of comparison here.
- "KEDA can only see one variant" is **false** and no argument below relies on it.
  A ScaledObject's `expr` formula can read globally-aggregated inputs via PromQL
  (`sum(...)` over all pods / variants / namespaces). The real distinctions are
  about *what the decider can compute* and *whether its output is jointly
  feasible* — not about input visibility. (**Jointly feasible**: the per-variant
  counts not only each meet their own SLO/bounds but also fit the shared GPU
  budget *together* — `Σ rᵢ·gpusᵢ ≤ G`. A wants 7 and B wants 6 can each be
  correct yet violate a 10-GPU budget.)

## The two deciders in one line

- **KEDA `expr`** ([`scalingModifiers`](https://keda.sh/docs/latest/reference/scaledobject-spec/),
  evaluated by [`expr-lang`](https://expr-lang.org/)): a closed-form, stateless
  scalar formula that folds a ScaledObject's named triggers into one
  `composite-metric`, thresholded independently per ScaledObject.
- **WVA optimizer** (`pkg/solver`, `cost-aware` / `greedy-by-score`): an iterative
  solver that produces one coordinated allocation across all variants in a single
  pass.

They occupy the **same slot** but sit at opposite ends of an expressiveness
spectrum. What one can express is a **strict subset** of the other.

## Side-by-side

| Dimension | KEDA `expr` formula | WVA optimizer |
|---|---|---|
| **Kind of computation** | Closed-form, stateless scalar expression | Iterative constrained optimization / search |
| **Aggregate multiple metrics** | Yes — multiple named triggers, plus PromQL within each | Yes |
| **Globally-aggregated inputs** | Yes — via PromQL (`sum`, `max`, label filters) | Yes |
| **Loops / search / constraint solving** | No | Yes |
| **Unit of decision** | One composite metric **per ScaledObject**, thresholded independently | One allocation across **all** variants |
| **Aggregate-decomposable rationing** (proportional, priority-tiered) | **Yes** — identical formula per ScaledObject over shared PromQL aggregates; O(1)–O(classes) triggers | Yes |
| **Full-vector cost-minimizing allocation** (greedy marginal-GPU, ILP) | **No** — needs the full per-variant vector + iteration; O(N²) hand-edited triggers and no loop | Yes — this is what the solver does |
| **Joint feasibility under a hard shared budget** | Not guaranteed — see *Consistency* below | Guaranteed by a single solve against fixed `G` |
| **Accelerator heterogeneity** (GPU type / arrangement choice) | Not represented | First-class, via performance profiles |
| **Objective function** | None (per-object HPA ratio `ceil(value/target)`) | Explicit: minimize cost s.t. SLOs (and capacity) |
| **Consistency of a multi-variant decision** | N independent HPA loops, staggered snapshots (see below) | One snapshot, one solve → internally consistent output |
| **State / history** | Stateless per evaluation | Stateful pipeline: collect → analyze → optimize |
| **Extensibility ceiling** | The `expr` grammar + exposed trigger values | Arbitrary Go; pluggable solver strategies |
| **Failure behavior** | Formula skipped; KEDA `fallback` applies | Solver runs on collected snapshot; degraded-mode policies |

The three rows in **bold** are the load-bearing ones; the notes below expand them.

## Notes on the load-bearing rows

### Aggregation is not optimization

`expr` is a closed-form, stateless scalar function — no loops, recursion, search,
or constraint solving. It transforms numbers into one number. That is enough for
**aggregate-decomposable** coordination, where each variant's share is a formula
over a few global aggregates:

- **Proportional rationing:** `desired_i = min(d_i, d_i · G / sum(d))` — three
  triggers (`d_i`, `sum(d)`, `G`), independent of N.
- **Priority-preemptive rationing:** `min(d_i, G − sum(demand{priority > mine}))`
  — bounded by the number of priority *classes*, not variants.

These are consistent *by construction* because every ScaledObject runs the **same
deterministic formula over the same PromQL aggregates**.

It is **not** enough for a **full-vector** allocation — "give the marginal GPU to
the most cost-efficient variant, then iterate" — which needs the entire
per-variant vector and a loop. WVA's current `cost-aware` optimizer is exactly
this: it greedily compares all variants' cost/capacity ratios. So today's WVA
policy is *not* `expr`-expressible — a property of the chosen policy, not of
coordination in general.

### Consistency is a window, not a barrier

The distinction is **not** "atomic vs. non-atomic." WVA's collection is not atomic
either — its optimizer assembles a demand vector across a collection window. The
real difference is where the skew lands:

- **WVA:** inputs skewed across one window, but the **decision is a single solve**,
  so the output vector is jointly feasible against that one snapshot. One decider,
  one window (~one `GLOBAL_OPT_INTERVAL`, default 60s).
- **KEDA `expr` + HPA:** KEDA maps each ScaledObject to its **own HPA**; the stock
  Kubernetes HPA controller reconciles each HPA **independently**, once per ~15s
  sync, with a per-HPA metrics query and no shared snapshot. Each variant's share
  is computed against a *different* snapshot → independently-correct shares can
  transiently sum to **more than `G`** (worst case ~45–75s of over-request, plus a
  up-to-300s scale-down stabilization tail).

So for aggregate-decomposable policies, `expr` *can* express the coordination, but
its per-variant shares are **each individually correct without being guaranteed
jointly feasible**; WVA's are jointly feasible by construction. The full
derivation of the worst-case window is in
[wva-metric-shop.md](./wva-metric-shop.md#consistency-the-property-the-distributed-scheme-cannot-guarantee).

### The performance model is separable (and not a KEDA limitation)

`expr` consumes precomputed scalars. WVA's analyzer *derives* an SLO-feasible
per-variant demand from a fitted performance model (ITL ≈ α + β·batch, saturation
bounds; see [modeling-optimization.md](./modeling-optimization.md)). But that L1
model can live outside both deciders — EPP is the natural home (see the metric-shop
doc). So "KEDA has no performance model" is not a decider deficiency; it just means
*something upstream* (EPP or WVA) must produce the demand scalar `expr` consumes.
This is why L1 is excluded from the table above.

## Worked boundary example

Two models share a pool of 10 GPUs. Model A is high-criticality, B is best-effort.
Load rises on both; combined demand is 14 replicas' worth.

- **KEDA, priority rationing (expressible):** each ScaledObject runs
  `min(d_i, G − sum(demand{priority > mine}))`. A (top priority) takes its full
  demand; B takes the remainder. This *does* respect priority and the budget — but
  A's HPA and B's HPA evaluate on independent ~15s loops against staggered
  snapshots, so during a transient their live counts can briefly sum above 10
  (pending pods), self-correcting over the next sync periods.
- **KEDA, cost-minimizing reallocation (not expressible):** "spend the 10 GPUs to
  minimize total \$ while both SLOs hold, choosing GPU type per variant" cannot be
  written as a per-object scalar formula.
- **WVA:** a single solve sees both models, the 10-GPU cap, both SLOs, priority,
  and per-accelerator cost. It emits a jointly feasible, cost-minimal allocation in
  one pass — no transient oversubscription against its snapshot.

The gap is therefore **not** "B can't see A," and **not** "KEDA can't ration." It
is: (1) `expr` cannot express a full-vector cost-minimizing solve, and (2) even for
policies it *can* express, N independent HPA loops do not guarantee joint
feasibility under a hard budget.

## Implication for the proposal

- **Simple / soft-capacity case:** L1 in EPP + an aggregate-decomposable `expr`
  formula per ScaledObject is sufficient. **No WVA binary.** Transient
  oversubscription is harmless where pending pods trigger a cluster autoscaler.
- **Hard-budget / cost-optimal case:** WVA makes the expressive decision — the
  jointly-feasible, cost-minimal, model-based allocation — and emits
  `wva_desired_replicas` per variant. KEDA's formula degenerates to a pass-through
  (`formula: "wva_desired_replicas"`, `target: "1"`), or blends it with a fast
  local signal (`max(wva_desired_replicas, local_estimate)`) for reactivity between
  WVA cycles.

In both cases KEDA remains the actuation path (metric server → HPA → Deployment).
WVA-the-binary is reserved for the decisions `expr` + PromQL cannot make: a
full-vector solve, or joint feasibility under a hard shared budget.

## References

- [wva-metric-shop.md](./wva-metric-shop.md) — metric decomposition, placement, and packaging
- [modeling-optimization.md](./modeling-optimization.md) — WVA modeling and optimization
- KEDA `scalingModifiers`: <https://keda.sh/docs/latest/reference/scaledobject-spec/>
- expr language: <https://expr-lang.org/docs/language-definition>

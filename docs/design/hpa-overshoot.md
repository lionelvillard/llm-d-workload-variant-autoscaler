# HPA External Metrics and Pending Pod Overshoot

When Kubernetes HPA scales on an external metric such as queue length, it does not account for pods that have been created but are not yet ready (pending pods). This document demonstrates how this blind spot leads to massive overshoot, analyzes the root cause formally, and addresses common mitigation questions.

## Problem Statement

HPA with external metrics of type `Value` computes desired replicas using:

```
desiredReplicas = ⌈ currentReplicas × ( queue_length / targetValue ) ⌉
```

Here `currentReplicas` is the deployment's `scale.status.replicas` — the total number of pods, **including pending (not-yet-ready) pods**. This means the formula feeds back on its own output: `desired` at cycle N becomes `currentReplicas` at cycle N+1.

The formula does not consider:

- That pending pods contribute zero processing capacity until they are ready.
- The processing capacity those pending pods will add once they start.

When pod startup time is non-trivial (common for GPU model servers that must load large model weights), the queue keeps growing while new pods are pending. Each HPA evaluation cycle sees a larger queue **and** a larger `currentReplicas` (inflated by pending pods). Because these two growing values are **multiplied together**, the result is a compounding feedback loop that produces catastrophically more pods than actually needed — far worse than the additive overshoot seen with `AverageValue` metrics.

## Experiment

### Parameters

| Parameter | Value |
|---|---|
| Incoming request rate (λ) | 100 req/s (constant) |
| Processing rate per pod (μ) | 10 req/s |
| **Steady-state pods needed (λ/μ)** | **10** |
| Initial ready pods (R₀) | 1 |
| Queue growth rate (g = λ − R₀·μ) | 90 req/s |
| HPA `targetValue` (T) | 250 items |
| HPA sync period (P) | 15 s |
| Pod startup time (S) | 60 s |

### Scale-Up Phase (t = 0 to t = 60 s)

During the entire startup window, no new pods become ready. Only the original pod processes traffic at 10 req/s. The queue grows at 90 req/s:

| Time | Queue Length | `currentReplicas` (R) | `desired` = ⌈R × Q / 250⌉ | Pods Created | Total Pods (ready + pending) |
|------|-------------|----------------------|---------------------------|--------------|------------------------------|
| t=0  | 0           | 1                    | 1                         | —            | 1 (1+0)                      |
| t=15 | 1 350       | 1                    | 6                         | +5           | 6 (1+5)                      |
| t=30 | 2 700       | 6                    | 65                        | +59          | 65 (1+64)                    |
| t=45 | 4 050       | 65                   | 1 053                     | +988         | 1 053 (1+1 052)              |
| t=60 | 5 400       | 1 053                | 22 745                    | +21 692      | 22 745 (1+22 744)            |

Every 15 s the queue has grown because the only pod doing work is the original one. But unlike `AverageValue`, the `Value` formula **multiplies** the queue ratio by `currentReplicas` — which includes all the pending pods from previous cycles. Each cycle's desired count becomes the next cycle's `currentReplicas`, creating a compounding feedback loop. The growth factor at each step equals Q(t)/T, which itself increases linearly with time: 5.4, 10.8, 16.2, 21.6.

**Peak desired: 22 745 pods. Steady-state need: 10 pods. Overshoot: 2 274x.**

### Landing Phase (t = 60 s onward)

As batches of pods become ready, processing capacity eventually far exceeds demand:

| Time | Event | Ready Pods | Processing Capacity | Net Queue Rate |
|------|-------|-----------|---------------------|----------------|
| t=60  | —                                       | 1      | 10 req/s      | +90 req/s (growing)            |
| t=75  | Batch 1 ready (5 pods from t=15)        | 6      | 60 req/s      | +40 req/s (still growing)      |
| t=90  | Batch 2 ready (59 pods from t=30)       | 65     | 650 req/s     | −550 req/s (draining)          |
| t=105 | Batch 3 ready (988 pods from t=45)      | 1 053  | 10 530 req/s  | queue empty, 1 043 pods idle   |
| t=120 | Batch 4 ready (21 692 pods from t=60)   | 22 745 | 227 450 req/s | queue empty, 22 735 pods idle  |

Unlike the `AverageValue` case, the first batch (5 pods) is too small to match demand — capacity at t=75 (60 req/s) is still below the incoming rate (100 req/s), so the queue continues to grow. Only at t=90, when batch 2 arrives, does capacity finally exceed demand and the queue begins to drain. Meanwhile, 22 680 more pods are still spinning up with nothing to do. The system ends with **22 745 running pods serving 100 req/s** — 22 735 of them idle — until HPA's cooldown window allows a scale-down, which introduces yet another delay.

## Formal Analysis

During the startup window `[0, S]`, no new capacity comes online. The queue grows linearly:

```
Q(t) = g · t = (λ − R₀ · μ) · t
```

The `Value` formula at HPA cycle `n` (time `t = nP`) is:

```
desired(n) = ⌈ desired(n−1) × g · nP / T ⌉
```

Each cycle multiplies the previous desired count by a growing factor `g·nP/T`. Unrolling the recurrence:

```
desired(n) ≈ ∏_{k=1}^{n} (g·kP / T) = (gP/T)^n × n!
```

This is **factorial growth** — faster than exponential. With our parameters (g=90, P=15, T=250):

```
gP/T = 90 × 15 / 250 = 5.4
```

| Cycle (n) | Formula | Approximation | Actual (with ceiling) |
|-----------|---------|--------------|----------------------|
| 1 | 5.4¹ × 1! | 5.4 | 6 |
| 2 | 5.4² × 2! | 58.3 | 65 |
| 3 | 5.4³ × 3! | 944.8 | 1 053 |
| 4 | 5.4⁴ × 4! | 20 407 | 22 745 |

The overshoot ratio at `t = S`:

```
overshoot = desired(S/P) / (λ / μ)
```

Unlike `AverageValue` where overshoot grows **linearly** with startup time, `Value` overshoot grows **factorially** with the number of HPA cycles during the startup window (`S/P`). This makes the overshoot catastrophically worse for slow-starting pods:

- **Startup time (S)**: More cycles during startup → factorial blowup.
- **Sync period (P)**: Shorter sync periods mean more cycles, compounding faster.
- **Growth rate (g)**: Bursty traffic increases the per-cycle multiplier.

The overshoot grows so fast that even modest parameter changes (a slightly longer startup, a slightly shorter sync period) can push desired replicas from hundreds to tens of thousands.

## What a Pending-Aware Autoscaler Does Differently

At t=15, both HPA and a pending-aware autoscaler create 5 pods (scaling to 6 total). The difference appears at t=30:

- **HPA**: sees queue=2 700 and currentReplicas=6, computes `⌈6 × 2700/250⌉ = 65`, creates 59 **more** pods — treating the 5 pending pods as justification for even more scaling rather than recognizing them as incoming capacity.
- **Pending-aware autoscaler**: sees queue=2 700 *but also* 5 pending pods representing 50 req/s of incoming capacity. Combined with the 1 ready pod (10 req/s), total expected capacity (60 req/s) is still below demand (100 req/s), so it may create a few more pods — but nowhere near 59. It reasons about **future capacity**, not just current metric values.

By t=45, HPA has spiraled to 1 053 pods; by t=60, to 22 745. The pending-aware autoscaler converges to ~10 pods — the actual steady-state need — with no resource waste and no thundering-herd scale-down.

## FAQ

### Can a large `stabilizationWindowSeconds` prevent the overshoot?

**Not fully.** The stabilization window controls **which `desired` value HPA acts on**, not **how it computes `desired`**.

For scale-up, the stabilization window keeps a sliding window of recent `desired` values and picks the smallest:

```
effectiveDesired = min(desired[t], desired[t-1], ..., desired[t-window])
```

With the `Value` metric type, the stabilization window has a more nuanced effect than with `AverageValue`. By holding `currentReplicas` low, it suppresses the multiplicative feedback — if R stays at 1, the formula behaves like simple division. But the queue continues to grow unchecked during the delay, and once the window allows scaling, the larger R feeds back into the formula and the compounding resumes.

**Example with a 30 s stabilization window:**

| Time | Queue | R | `desired` = ⌈R × Q/250⌉ | Stabilized (min over 30 s) | Actual R |
|------|-------|---|--------------------------|---------------------------|----------|
| t=15 | 1 350 | 1 | 6    | min(1, 6) = 1         | 1   |
| t=30 | 2 700 | 1 | 11   | min(1, 6, 11) = 1     | 1   |
| t=45 | 4 050 | 1 | 17   | min(6, 11, 17) = 6    | 6   |
| t=60 | 5 400 | 6 | 130  | min(11, 17, 130) = 11 | 11  |
| t=75 | 6 750 | 11 | 297 | min(17, 130, 297) = 17 | 17 |
| t=90 | 8 100 | 17 | 551 | min(130, 297, 551) = 130 | 130 |

The stabilization window reduces the peak substantially (130 at t=90 vs 22 745 at t=60 without stabilization) because holding R low breaks the multiplicative feedback loop. However, capacity doesn't arrive until 60 s after each scaling decision, so the queue has grown to 8 100 by t=90 with almost no processing capacity online. The multiplicative feedback eventually kicks in despite the damping — and the delayed capacity means worse latency during the entire ramp-up.

### Can `scaleUp.policies` with rate limits help?

**Partially, but with significant trade-offs.** A rate limit such as `maxPods: 10, periodSeconds: 60` caps the ramp slope, but it still does not account for pending pods. With the `Value` metric type, rate limits are more effective than with `AverageValue` because they indirectly suppress the multiplicative feedback (by keeping R low). However, you either cap too aggressively (underscale and violate latency SLOs) or too loosely (the compounding eventually overwhelms the limit). Choosing the right rate limit requires predicting traffic patterns ahead of time, which defeats the purpose of autoscaling.

### What about using a larger `targetValue`?

A larger target reduces the per-cycle multiplier (`Q/T` is smaller), but the factorial growth structure remains. For example, with T=500 instead of 250, the desired count at t=60 is still 1 491 — a 149x overshoot. The multiplicative feedback is the root cause, not the target value. Additionally, a larger target means you tolerate a longer queue at steady state, increasing per-request latency.

### Why doesn't HPA account for pending pods in external metrics?

HPA's core algorithm was designed around resource metrics (CPU, memory) where the metric value naturally adjusts as pods come online — a pending pod contributes 0% CPU, which already factors into the average. External metrics, however, represent a global value (total queue length) that does not change based on how many pods exist.

With the `Value` metric type, this blind spot is compounded by the `currentReplicas` multiplier. The formula `⌈currentReplicas × metric / target⌉` treats pending pods as if they were contributing to the problem (by inflating `currentReplicas`) when in fact they are part of the solution (incoming capacity). This makes pending-pod blindness **multiplicatively** rather than additively harmful.

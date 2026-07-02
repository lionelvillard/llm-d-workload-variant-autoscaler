# Proposal: Overprovisioning buffer for fast scale-up (KEDA)

**Authors:** [TBD]
**Status:** Draft (revised 2026-07-02 — KEDA-only)
**Created:** 2026-06-23
**Last Updated:** 2026-07-02

---

## Problem Statement

Model server cold start is slow — minutes, not seconds. When demand bursts, KEDA quickly computes the right replica count, but the new pods are not Ready in time. The burst is served by an undersized fleet and SLOs are missed before KEDA's decision can take effect.

The scaler is doing its job; the bottleneck is pod startup latency. By the time a freshly requested replica has pulled its image, loaded model weights, and warmed its KV cache, the burst that justified it may already be over — and the requests that arrived in the meantime were served slowly or dropped.

Operators today work around this by permanently over-provisioning: setting a high `minReplicaCount` so spare capacity is always running. This wastes GPUs during steady state and still does not adapt to where the spare capacity is actually needed.

What is missing is a way to keep a small pool of **warm, Ready, fully initialized** model servers standing by — real pods that receive no traffic until the system saturates, at which point they can be promoted to serving in milliseconds instead of minutes.

### Where this fits

```
       Prometheus                       +---------------+   reads same trigger
  ------------------> KEDA ScaledObject | WVA buffer    | <-- (PromQL + threshold)
   (trigger query)      |               | controller    |
                        v               +---------------+
                     scales Deployment          |
                                                v
                                    patch spec.minReplicaCount to A + B,
                                    label pods active/buffer,
                                    set pod-deletion-cost
```

The `ScaledObject` is driven by whatever Prometheus query the operator already configured. This proposal does **not** introduce or depend on any WVA-produced scaling metric — WVA runs the same PromQL KEDA runs and compares against the same threshold. It adds a buffer of warm pods on top of KEDA's target, and a fast path to put those pods into service the moment they are needed.

---

## Goals

1. Keep a configurable number of warm "buffer" pods over and above KEDA's honest target replica count.
2. Hide buffer pods from EPP request routing so they receive no traffic until promoted.
3. Promote a buffer pod to active in milliseconds when the system saturates.
4. Scale down by terminating buffer pods first.
5. Integrate with KEDA `ScaledObject` as the only supported scale target in v1.
6. No new CRDs. No changes to the deprecated `VariantAutoscaling` CRD.
7. No changes to how KEDA is driven — the operator's existing Prometheus trigger and scaling pipeline keep working unchanged.

## Non-Goals

- Direct `HorizontalPodAutoscaler` support (no KEDA). Deferred to a follow-up if there is demand.
- Cross-pool / cross-variant orchestration.
- Predictive promotion based on historical load.
- A formal CRD for buffer policy.
- Auto-tuning the buffer policy — the admin sets the min/percent/max bounds.
- Solving metric dilution automatically (it becomes a documented configuration requirement; see Risks).
- Coupling to any WVA scaling engine or analyzer — KEDA's own trigger metric is sufficient as the saturation signal.

---

## Proposed Solution

The feature adds two cooperating responsibilities and stores all state in pod labels and annotations on the existing `ScaledObject` — no new Kubernetes objects.

- **WVA** owns all writes: it labels pods `active` or `buffer`, sets their deletion-cost annotations, and patches the `ScaledObject.spec.minReplicaCount` floor to keep buffer capacity provisioned.
- **EPP (llm-d-router)** owns one filter: when enabled, it excludes pods labeled `llmd.ai/role=buffer` from routing candidates. Buffer pods stay warm but receive no traffic.

A buffer pod is a real, Ready model server. Promotion is a single label flip — `buffer → active` — which EPP picks up on its next candidate-cache refresh (~50ms), versus minutes for a cold start.

### Concepts

- **Active pod** — labeled `llmd.ai/role=active`. EPP routes requests to it.
- **Buffer pod** — labeled `llmd.ai/role=buffer`. EPP excludes it from candidates.
- **Active count (`A`)** — number of Ready `role=active` pods in the Deployment.
- **Buffer target (`B`)** — number of Ready buffer pods to keep warm, computed from annotations.
- **Scale target** — the KEDA `ScaledObject` that governs scaling. Buffer annotations and the `minReplicaCount` floor live here.

### Configuration Surface

Buffer policy is expressed as annotations on the `ScaledObject`:

| Annotation | Type | Default | Meaning |
|---|---|---|---|
| `llmd.ai/buffer-min` | non-negative int | `0` | Absolute floor for buffer count. |
| `llmd.ai/buffer-percent` | non-negative int | `0` | Buffer as a percent of `A`. |
| `llmd.ai/buffer-max` | non-negative int | unbounded | Absolute cap. |

The effective buffer is `B = clamp(ceil(A * percent / 100), min, max)`. If all annotations are absent or zero, `B = 0` and the feature is off. A ScaledObject with no buffer annotations is ignored entirely — existing deployments see no behavior change.

WVA also records the operator's original `minReplicaCount` in an `llmd.ai/user-min-replicas` annotation the first time it observes the target, so it never pushes the floor below the operator's intended minimum once it begins patching `min = max(user-min, A + B)`.

### Behavior

WVA keeps the buffer provisioned and reacts to saturation using **the same Prometheus trigger KEDA is already configured with**. It reads `spec.triggers[]` from the ScaledObject, executes the trigger's `query` against Prometheus at WVA's fast cadence, and compares to the trigger's `threshold`. `spec.advanced.scalingModifiers.formula` is supported in the Prometheus-only subset via the same `expr` library KEDA uses.

- **Provision.** WVA keeps `ScaledObject.spec.minReplicaCount = max(user-min, A + B)`, so KEDA always runs `B` warm pods beyond the active set. New pods are labeled `buffer` when first observed.
- **Promote.** When the saturation ratio rises above `1 + tolerance` and a Ready buffer pod exists, WVA flips the oldest such pod to `active`. Promotion is immediate — the cost of one extra serving pod is small; the cost of being late is an SLO miss.
- **Demote.** When the ratio stays below `1 - tolerance` for a sustained window (60s default), WVA flips the youngest active pod back to `buffer`. Demotion waits because flapping has real cost (label and candidate-set churn).
- **Scale down.** Buffer pods carry a low `pod-deletion-cost`, so when KEDA removes replicas it terminates buffer pods first, preserving serving capacity.

WVA yields when the ScaledObject is paused (via KEDA's `autoscaling.keda.sh/paused[-replicas]` annotations), and enters a signal-degraded state (no promotions or demotions, current floor held) when Prometheus is unreachable.

### What Does NOT Change

- KEDA is still driven by the operator's existing Prometheus trigger; this proposal adds no new scaling metric.
- KEDA still performs the actual scaling via its generated HPA — WVA only patches the `spec.minReplicaCount` floor and labels pods.
- EPP makes no change beyond the optional buffer filter — it writes no labels and needs no extra RBAC.
- Existing deployments without buffer annotations behave identically to today.

---

## Risks and Open Questions

1. **Metric dilution.** If the trigger's PromQL averages across all pods in the Deployment, idle buffer pods drag the average down and KEDA under-scales. **Mitigation: the operator must filter the trigger's PromQL by `llmd.ai/role="active"`.** This is a documented requirement.

2. **EPP filter must be enabled.** If llm-d-router runs without buffer filtering, buffer pods receive traffic and the feature degrades silently. WVA surfaces a status condition asking the operator to confirm the filter is on.

3. **`pod-deletion-cost` is a hint, not a guarantee.** Kubernetes tries to honor it but is not required to. If an active pod is terminated anyway, WVA detects the gap and promotes a buffer pod to refill it — acceptable, self-healing degradation.

4. **CRD-less coordination.** All state lives in pod labels, ScaledObject annotations, and the ScaledObject spec — no new etcd objects. This is the right tradeoff for v1 but limits introspection (no `kubectl get bufferpolicies`). A CRD may follow if usage demands it.

5. **Formula parity with KEDA.** For ScaledObjects that use `scalingModifiers`, WVA re-evaluates the formula itself using the same `expr` library KEDA uses, restricted to Prometheus-only triggers and pure numeric expressions. Non-Prometheus triggers referenced in the formula are unsupported in v1.

---

## Alternatives Considered

1. **Permanent over-provisioning via high `minReplicaCount`.** Simple, but wastes GPUs continuously and does not adapt to where spare capacity is needed. The buffer is bounded, promoted on demand, and terminated first on scale-down.

2. **A dedicated buffer-policy CRD.** Better introspection, but adds a new API surface, RBAC, and versioning burden. Annotations keep policy co-located with the scale target. May be reconsidered post-v1 if usage grows.

3. **Predictive promotion from historical load.** More powerful but far more complex and harder to reason about. The initial version reacts to the live saturation metric the operator already trusts for scaling.

4. **Scale target abstraction (both HPA and KEDA).** Rejected for v1: KEDA already generates and owns the HPA, so supporting a standalone HPA path adds interface cost without matching user demand in the llm-d KEDA-based deployments.

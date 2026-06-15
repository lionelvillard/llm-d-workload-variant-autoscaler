# Proposal: Overprovisioning buffer for fast scale-up

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-06-23
**Last Updated:** 2026-06-23

---

## Problem Statement

Model server cold start is slow — minutes, not seconds. When demand bursts, HPA or KEDA quickly computes the right replica count, but the new pods are not Ready in time. The burst is served by an undersized fleet and SLOs are missed before the scaler's decision can take effect.

The scaler is doing its job; the bottleneck is pod startup latency. By the time a freshly requested replica has pulled its image, loaded model weights, and warmed its KV cache, the burst that justified it may already be over — and the requests that arrived in the meantime were served slowly or dropped.

Operators today work around this by permanently over-provisioning: setting a high `minReplicas` so spare capacity is always running. This wastes GPUs during steady state and still does not adapt to where the spare capacity is actually needed.

What is missing is a way to keep a small pool of **warm, Ready, fully initialized** model servers standing by — real pods that receive no traffic until the system saturates, at which point they can be promoted to serving in milliseconds instead of minutes.

### Where this fits

```
        external metric                  +-------------+   reads same metric
  pods --------------------> HPA / KEDA  |  WVA buffer  | <-- (saturation signal)
        (saturation signal)   |  scale    | controller   |
                              v  target   +-------------+
                          scale Deployment     |
                                               v
                                   patch min floor (A + B),
                                   label pods active/buffer
```

The scale target (HPA or `ScaledObject`) is driven by whatever external metric the operator already configured. This proposal does **not** introduce or depend on any WVA-produced scaling metric. It adds a buffer of warm pods on top of whatever target the scaler computes, and a fast path to put those pods into service the moment they are needed — using the **same** external metric the scaler already reads as its saturation signal.

---

## Goals

1. Keep a configurable number of warm "buffer" pods over and above the scaler's honest target replica count.
2. Hide buffer pods from EPP request routing so they receive no traffic until promoted.
3. Promote a buffer pod to active in milliseconds when the system saturates.
4. Scale down by terminating buffer pods first.
5. Work with both HPA and KEDA `ScaledObject` as the scale target.
6. No new CRDs. No changes to the deprecated `VariantAutoscaling` CRD.
7. No changes to how the operator's scale target is driven — the existing external metric and scaling pipeline keep working unchanged.

## Non-Goals

- Cross-pool / cross-variant orchestration.
- Predictive promotion based on historical load.
- A formal CRD for buffer policy.
- Auto-tuning the buffer policy — the admin sets the min/percent/max bounds.
- Solving metric dilution automatically (it becomes a documented configuration requirement; see Risks).
- Coupling to any WVA scaling engine or analyzer — the scaler's own external metric is sufficient as the saturation signal.

---

## Proposed Solution

The feature adds two cooperating responsibilities and stores all state in pod labels and annotations on the existing scale target — no new Kubernetes objects.

- **WVA** owns all writes: it labels pods `active` or `buffer`, sets their deletion-cost annotations, and patches the scale target's `minReplicas` / `minReplicaCount` floor to keep buffer capacity provisioned.
- **EPP (llm-d-router)** owns one filter: when enabled, it excludes pods labeled `llmd.ai/role=buffer` from routing candidates. Buffer pods stay warm but receive no traffic.

A buffer pod is a real, Ready model server. Promotion is a single label flip — `buffer → active` — which EPP picks up on its next candidate-cache refresh (~50ms), versus minutes for a cold start.

### Concepts

- **Active pod** — labeled `llmd.ai/role=active`. EPP routes requests to it.
- **Buffer pod** — labeled `llmd.ai/role=buffer`. EPP excludes it from candidates.
- **Active count (`A`)** — number of Ready `role=active` pods in the Deployment.
- **Buffer target (`B`)** — number of Ready buffer pods to keep warm, computed from annotations.
- **Scale target** — the HPA or KEDA `ScaledObject` that owns scaling for the Deployment. Buffer annotations and the min-replicas floor live here.

### Configuration Surface

Buffer policy is expressed as annotations on the scale target (HPA or ScaledObject):

| Annotation | Type | Default | Meaning |
|---|---|---|---|
| `llmd.ai/buffer-min` | int | `0` | Absolute floor for buffer count. |
| `llmd.ai/buffer-percent` | non-negative int | `0` | Buffer as a percent of `A`. |
| `llmd.ai/buffer-max` | int | unbounded | Absolute cap. |

The effective buffer is `B = clamp(ceil(A * percent / 100), min, max)`. If all annotations are absent or zero, `B = 0` and the feature is off. A scale target with no buffer annotations is ignored entirely — existing deployments see no behavior change.

WVA also records the operator's original `minReplicas` in an `llmd.ai/user-min-replicas` annotation the first time it observes the target, so it never pushes the floor below the operator's intended minimum once it begins patching `min = max(user-min, A + B)`.

### Behavior

WVA keeps the buffer provisioned and reacts to saturation using the scaler's **own external metric** as the saturation signal — the same metric the operator already wired into HPA/KEDA, polled at WVA's existing fast cadence:

- **Provision.** WVA keeps the scale target's floor at `max(user-min, A + B)`, so the scaler always runs `B` warm pods beyond the active set. New pods are labeled `buffer` when first observed.
- **Promote.** When saturation rises above the metric's target (plus a tolerance) and a Ready buffer pod exists, WVA flips the oldest such pod to `active`. Promotion is immediate — the cost of one extra serving pod is small; the cost of being late is an SLO miss.
- **Demote.** When saturation stays below target (minus a tolerance) for a sustained window, WVA flips the youngest active pod back to `buffer`. Demotion waits because flapping has real cost (label and candidate-set churn).
- **Scale down.** Buffer pods carry a low `pod-deletion-cost`, so when the scaler removes replicas it terminates buffer pods first, preserving serving capacity.

### What Does NOT Change

- The scale target is still driven by the operator's existing external metric; this proposal adds no new scaling metric.
- HPA/KEDA still perform the actual scaling — WVA only patches the `min` floor and labels pods.
- EPP makes no change beyond the optional buffer filter — it writes no labels and needs no extra RBAC.
- Existing deployments without buffer annotations behave identically to today.

---

## Risks and Open Questions

1. **Metric dilution.** If the scaler's metric is averaged across all pods in the Deployment, idle buffer pods drag the average down and the scaler under-scales. **Mitigation: the operator must filter the scaler's metric query by `llmd.ai/role=active`.** HPA external/object metrics support a selector; KEDA queries can include the label match in PromQL. This is a documented requirement.

2. **EPP filter must be enabled.** If llm-d-router runs without buffer filtering, buffer pods receive traffic and the feature degrades silently. WVA surfaces a status condition asking the operator to confirm the filter is on.

3. **`pod-deletion-cost` is a hint, not a guarantee.** Kubernetes tries to honor it but is not required to. If an active pod is terminated anyway, WVA detects the gap and promotes a buffer pod to refill it — acceptable, self-healing degradation.

4. **CRD-less coordination.** All state lives in pod labels, scale-target annotations, and the scale-target spec — no new etcd objects. This is the right tradeoff for v1 but limits introspection (no `kubectl get bufferpolicies`). A CRD may follow if usage demands it.

---

## Alternatives Considered

1. **Permanent over-provisioning via high `minReplicas`.** Simple, but wastes GPUs continuously and does not adapt to where spare capacity is needed. The buffer is bounded, promoted on demand, and terminated first on scale-down.

2. (MAY WANT TO RECONSIDER) **A dedicated buffer-policy CRD.** Better introspection, but adds a new API surface, RBAC, and versioning burden - Annotations keep policy co-located with the scale target.

3. **Predictive promotion from historical load.** More powerful but far more complex and harder to reason about. The initial version reacts to the live saturation metric the operator already trusts for scaling.

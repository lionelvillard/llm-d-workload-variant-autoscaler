# Proposal: Overprovisioning buffer via a buffer variant

**Authors:** [TBD]
**Status:** Draft (revised 2026-07-02 — buffer-variant Deployment)
**Created:** 2026-06-23
**Last Updated:** 2026-07-02

---

## Problem Statement

Model server cold start is slow — minutes, not seconds. When demand bursts, KEDA quickly computes the right replica count, but the new pods are not Ready in time. The burst is served by an undersized fleet and SLOs are missed before KEDA's decision can take effect.

Operators today work around this by permanently over-provisioning with a high `minReplicaCount`. This wastes GPUs during steady state and does not adapt to where the spare capacity is actually needed.

What is missing is a way to keep a small pool of **warm, Ready, fully initialized** model servers standing by — real pods that receive no traffic until the system saturates, at which point they can start serving in milliseconds instead of minutes.

### Where this fits

```
    Primary Deployment       Buffer Deployment
    (KEDA-scaled)            (fixed replicas = N)
        |                        |
        +--- one InferencePool --+
                    |
                    v
                   EPP
             buffer-gate filter
      (drops buffer endpoints unless primary is saturated)
```

The primary variant is scaled by KEDA as it is today. The buffer variant is a **sibling Deployment** with the same PodSpec and a fixed replica count. Both variants share one `InferencePool`. A new **Filter plugin** in EPP excludes buffer endpoints from the candidate set unless the primary fleet is saturated — at which point buffer endpoints start receiving traffic within a single candidate-cache TTL (~50ms).

Nothing in WVA runs at request time. Nothing in KEDA scales the buffer. Buffer refill on pod death is native Kubernetes.

---

## Goals

1. Keep a configurable number of warm buffer pods alongside the primary fleet.
2. Hide buffer pods from routing until the primary fleet is saturated.
3. Fail over in milliseconds when saturation is detected — the mechanism is EPP's routing decision, not a controller-driven label flip.
4. Scale down cleanly: primary scales on its own KEDA metric; buffer holds at `N`.
5. Integrate with KEDA `ScaledObject` on the primary. Buffer needs no scaler.
6. No new CRDs. No changes to the deprecated `VariantAutoscaling` CRD.

## Non-Goals

- Cross-pool / cross-model shared buffer pools.
- Predictive buffer sizing.
- Buffer pods on a different accelerator SKU than primary.
- Auto-tuning `N`; the admin sets it.
- Direct `HorizontalPodAutoscaler` support without KEDA.

---

## Proposed Solution

The feature is expressed in operator YAML and one EPP Filter plugin.

- **Operator** deploys a **buffer Deployment** — same PodSpec as primary, `spec.replicas=N`, no autoscaler — plus keeps a shared `InferencePool` whose selector matches both variants. Pods carry `llm-d.ai/variant=primary|buffer`.
- **EPP** runs a new `buffergate` Filter plugin in the scheduling chain. It partitions endpoints by the variant label and admits buffer endpoints only when EPP's existing `SaturationDetector` (already used by flow-control admission) reports the primary sub-fleet as saturated.
- **WVA has no runtime component** for this feature. Optional advisory webhook may warn on common misconfigurations but is not required.

### Concepts

- **Primary variant** — pods labeled `llm-d.ai/variant=primary` (or unlabeled). Scaled by the primary `ScaledObject`.
- **Buffer variant** — pods labeled `llm-d.ai/variant=buffer`. Fixed `replicas=N` on the buffer Deployment. Never scaled by KEDA.
- **Buffer-gate filter** — new EPP scheduling filter. Drops buffer endpoints unless primary saturation is detected.

### Configuration Surface

There is no controller configuration. The operator authors:

1. A primary Deployment (existing).
2. A primary `ScaledObject` (existing).
3. A buffer Deployment (new, same PodSpec, `replicas=N`, `variant=buffer` label).
4. An `InferencePool` selecting both variants via a shared label (e.g., `model=foo`).
5. EPP config enabling the `buffergate` filter in the default scheduling profile.

### Behavior

- **Provision.** Kubernetes' Deployment controller keeps buffer at `N` pods, native semantics. WVA does not intervene.
- **Route.** For each request, EPP's filter chain runs. Under normal load the buffer-gate filter removes buffer endpoints, so only primary pods receive traffic. When primary saturation is detected (per the existing `SaturationDetector`), the filter admits buffer endpoints and the scheduler routes to whichever pod is least loaded — typically a buffer pod.
- **Refill.** Kubernetes handles it — a dead buffer pod is replaced by the Deployment controller. No demotion event exists.
- **Scale down.** Primary scales down on its own KEDA metric. Buffer never scales.

### Metric-dilution mitigation

Same problem as any variant-based routing: the primary's KEDA trigger PromQL must not include buffer pods. Three mitigations, ranked by generality:

- **(a) PromQL label filter** — add `variant="primary"` to the trigger query. Works for the mainstream Prometheus setups (Prometheus Operator, `honor_labels` + relabel). Recommended default.
- **(b) Separate PodMonitor per variant** — split at scrape time when raw metrics don't preserve pod labels. Backup.
- **(c) Prometheus recording rule** — pre-aggregate to a variant-filtered stream. Requires operator control over Prometheus config.

The spec documents (a) as default with concrete PromQL and (b) as documented backup.

### What Does NOT Change

- KEDA still scales the primary Deployment based on the operator's trigger — no new metrics, no `minReplicaCount` writes.
- No new CRDs. No new controllers in WVA. No new RBAC.
- EPP's datastore stays single-pool. The buffer variant is inside the same pool, distinguished by a pod label — same shape as the existing `role`-based disaggregation.
- Existing deployments without a buffer Deployment behave identically to today.

---

## Risks and Open Questions

1. **EPP filter must be enabled.** Without the `buffergate` filter, buffer endpoints receive traffic normally. Advisory webhook can warn; core feature depends on operator config.

2. **Saturation semantics come from EPP.** The buffer-gate filter reuses whatever `SaturationDetector` the EPP is configured with (utilization or concurrency). WVA does not define its own saturation.

3. **`SaturatedOver(subset)` interface method.** The existing detectors compute over the whole pool. The filter needs to compute over the primary sub-slice specifically — a small refactor extends the detector interface.

4. **Same PodSpec constraint.** Buffer and primary must be interchangeable. Enforced by operator convention in v1.

5. **Two Deployments is a mild UX regression** vs. a single Deployment with role labels. Inherent — pods can't change owners. A future `BufferSpec` CRD could hide the dual-Deployment YAML behind one object.

6. **Metric dilution.** If the operator forgets to filter the primary trigger PromQL by variant, KEDA under-scales. Documented; advisory webhook can warn.

---

## Alternatives Considered

1. **Label-flipping design (previous spec).** WVA labels pods `active`/`buffer`, patches `ScaledObject.spec.minReplicaCount`, runs promotion/demotion loops. Rejected as too complex: a new WVA controller, Prometheus polling inside WVA, a `pod-deletion-cost` state machine, and a `user-min-replicas` snapshot mechanism — all to move a label the operator could avoid by using a distinct Deployment.

2. **Formula-bias design (`scalingModifiers.formula` + N*T).** WVA rewrites the operator's KEDA formula so KEDA naturally provisions `N` extra pods. Removes WVA's Prometheus polling and the min-floor patch — but keeps the pod-labeling and promotion loop (otherwise the extra pods serve traffic and dilute the metric), and introduces a formula-drift failure mode on user edits. Modest simplification over the label-flipping design; strictly less elegant than the buffer-variant design.

3. **Formula-bias without labels.** Same as above but drop the labels — buffer pods serve. Simplest to implement; forfeits the warm-standby property. The "buffer" is just dynamic overprovisioning that always serves traffic and pays the cold-start cost on the *next* burst.

4. **Permanent over-provisioning via high `minReplicaCount`.** Simple, wastes GPUs continuously, no adaptivity.

5. **Buffer as a shared pool across models.** Deferred — reintroduces cross-tenant interference and needs a real design pass on ownership.

6. **Predictive promotion from historical load.** Deferred as more powerful but far more complex.

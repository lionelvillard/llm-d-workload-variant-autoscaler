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

## Use Cases

All of the following reduce to a single condition: **Ready primary capacity falls below demand faster than a cold start (minutes) can restore it.** In each case the buffer bridges the resulting gap within one candidate-cache TTL (~50ms) and disengages once the primary fleet recovers.

1. **Sudden traffic burst.** Demand rises past the primary fleet's capacity before KEDA's newly requested pods reach Ready. The buffer serves the leading edge of the burst immediately, KEDA-provisioned pods assume the load as they become Ready, and the buffer returns to standby. This is the primary case: the interval between a metric crossing its threshold and a new pod becoming Ready is precisely the window the buffer covers.

2. **GPU or node failure.** A hardware fault, a node drain, or an OOM-killed pod removes Ready capacity from the primary fleet immediately, saturating the remaining pods. The Deployment controller reschedules the lost pod, but the replacement must cold-start. The buffer absorbs the displaced traffic during that interval. Loss of a *buffer* pod is handled identically — the Deployment controller refills it, with no promotion event.

3. **Rolling upgrades and node maintenance.** During a primary rollout — an image, configuration, or resource change — existing pods terminate while replacements cold-start. `maxSurge` and `maxUnavailable` bound the disruption, but the surging pods are not Ready for minutes. Node cordon and drain for maintenance follow the same pattern. When load arrives during a rollout, the buffer covers the transient capacity reduction, removing the need to schedule upgrades around low-traffic windows.

4. **Spot / preemptible reclamation.** When the primary runs on preemptible GPUs, the scheduler may reclaim multiple nodes concurrently with only seconds of notice. This resembles a multi-pod failure: capacity drops abruptly while cold replacements lag. The buffer smooths the reclamation. Whether buffer pods are placed on more stable (for example, on-demand) capacity is an operator node-placement decision, constrained in v1 by the same-PodSpec requirement.

5. **Steep, predictable ramps.** A well-tuned KEDA trigger still reacts only to load that has already arrived. On a steep increase in demand — the start of business hours, or a scheduled batch job — the buffer covers the reaction window at the leading edge of each step. The buffer remains reactive: it is gated on measured saturation rather than a forecast. Predictive sizing is a non-goal (see below).

**Scope.** The buffer is a transient bridge, not a capacity floor. Sustained demand growth is KEDA's responsibility: once new primary pods are Ready, they carry the load and the buffer disengages. Continuous buffer engagement indicates that the primary fleet is chronically under-scaled — for example, an incorrect trigger threshold or too low a `maxReplicaCount` — rather than a need to increase `N`.

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

---

## Future Work

### Cheaper standby backends via the llm-d launcher

The v1 buffer is a set of ordinary Ready pods: fully warm, serving in ~50ms, but each holding a full GPU allocation while idle. This is the most responsive standby, and also the most expensive. The routing mechanism, however, does not depend on *how* a buffer endpoint became Ready — only that it is admitted when the primary saturates. That is the seam the FAQ refers to ("different buffer backends"), and [llm-d Fast Model Actuation (FMA)](https://github.com/llm-d-incubation/llm-d-fast-model-actuation) is what lets us exploit it.

The FMA [launcher](https://github.com/llm-d-incubation/llm-d-fast-model-actuation/tree/main/inference_server/launcher) is a daemon that manages a fleet of vLLM instances within a single pod, rather than one vLLM process per pod. Because it controls the lifecycle of each instance, it can hold standby capacity in cheaper representations and bring it up on demand. This turns the buffer from a single point on the cost/latency curve into a spectrum the operator can choose from:

- **Warm pods (v1).** ~50ms to serve, full GPU cost while idle. The baseline, and the only backend that needs no launcher.

- **HOT instances (vLLM level-1 sleep/wake).** The launcher keeps additional vLLM instances resident with their model tensors moved from GPU memory to host (CPU) memory, freeing the bulk of the GPU for active instances — so multiple instances can be multiplexed onto the same devices. A HOT instance cannot serve while asleep but remains a live process; waking it restores tensors to the GPU in a few seconds (FMA reports about 3s for a model with 64 GiB of tensor data). The standby cost is host RAM plus a small residual GPU footprint: the instance keeps its CUDA context resident (~2GB), so a HOT instance never fully releases the device. Suited to bursts where a few seconds of ramp is acceptable in exchange for much lower steady-state GPU cost. This is FMA's Milestone 2 (sleep/wake), reported as finished; launcher-managed model swapping across instances is Milestone 3, under implementation.

- **Snapshot / restore (candidate, not yet in FMA).** A deeper tier could restore a vLLM process from a persisted snapshot. This would be slower than waking a HOT instance — on the order of tens of seconds — but would hold no GPU at all while idle, not even the ~2GB CUDA context a HOT instance retains, so a large reserve would cost essentially nothing on the accelerator until restored. FMA does not implement this today; it is included here as a plausible extension of the same launcher-managed model, not a current capability. Latency and feasibility would need to be established.

The tiers trade time-to-serve against idle cost — the faster a tier can start serving, the more it costs to hold on standby:

```
 idle GPU cost
      full │  ● Warm pods
   (1 GPU) │    full GPU + weights resident, always Ready (~50 ms)
           │
           │
           │           ● HOT instance  (vLLM L1 sleep/wake)
    ~2 GB  │             weights in host RAM, ~2 GB CUDA context on GPU
           │             GPU multiplexed across instances
           │
           │
      none │                          ● Snapshot / restore  (candidate)
   (no GPU)│                            no GPU, no live process while idle
           └────────────────────────────────────────────────────────────►
             ~50 ms            ~3 s                  ~10–20 s
                         time to serve  (log scale, not linear)

           ◄─────────── more responsive        cheaper on standby ──────────►
```

These tiers are not mutually exclusive. A single buffer could be layered — a small tier of warm pods for the first seconds of a burst, backed by HOT instances for the next increment, backed by snapshots for a large reserve — trading responsiveness for cost at each layer. The EPP filter admits whatever is Ready; the launcher decides how much of each tier to keep and when to bring the next tier up.

#### Sharing buffer capacity across variants

The tiers above describe the standby cost of a *single* buffer. A second dimension appears once the cluster runs multiple InferencePools, or multiple variants within a pool: how does buffer cost scale with the number of variants, and can one GPU back the buffer for more than one of them?

A buffer belongs to a specific variant — its pods share that variant's PodSpec and model. So with warm pods the cost scales linearly: two pools, each with one variant and two buffer pods, is four dedicated buffer GPUs, all idle. There is no sharing, because a warm pod for variant A cannot serve variant B.

The launcher-managed tiers break this linear scaling, because a standby instance does not occupy the GPU the way a warm pod does:

- **HOT and snapshot standby can share one GPU across variants.** Since only the *woken* (or restored) instance needs the device, a single GPU can hold the standby capacity for several variants and back whichever one saturates first — as long as they are not all woken at once. The buffer for N variants can then cost far less than N GPUs: it costs the standby footprint of N instances (host RAM for HOT, or storage for snapshots, plus the small per-instance CUDA context for HOT) but only the *active* GPU share of however many are woken concurrently. This trades a scheduling constraint — the shared GPU can satisfy only a bounded number of simultaneous wakes — for a large reduction in idle GPUs, and fits the common case where variants are unlikely to burst at the same instant.

- **Warm pods can share a GPU by using less memory each.** A warm buffer pod need not reserve a whole GPU. Two warm pods can co-locate on one device, each capped at a fraction of GPU memory (via MIG partitions or vLLM's `--gpu-memory-utilization`). Each serves at reduced capacity — smaller KV-cache, so fewer or shorter concurrent requests — but both stay instantly Ready at ~50ms. For buffers whose job is to absorb the leading edge of a burst rather than carry sustained load, reduced-capacity warm pods may be a better cost point than a full-GPU warm pod, and they preserve the warm tier's instant response.

Together these give the operator a second axis to tune alongside the time-to-serve tiers: not just *how fast* standby capacity comes up, but *how many GPUs* the buffer for a whole set of variants consumes. Realizing it needs launcher and scheduler support to place multiple variants' standby instances on shared devices, bound the number of concurrent wakes per GPU, and keep the routing contract honest about which variants can currently be served.

Integrating this requires: a way to express the standby tier(s) and their sizes in the buffer configuration, launcher-side control to wake or restore instances as saturation deepens, and a routing contract that keeps not-yet-Ready tiers out of the candidate set until they are serving. The saturation signal that gates the v1 buffer is the natural trigger for bringing up the next tier as well.

---

## FAQ

### Why not simply lower the KEDA trigger threshold to over-provision?

Lowering the trigger threshold (or raising `minReplicaCount`) does add extra idle capacity, and that capacity is genuinely reserved — so at first glance it looks equivalent. It is not, because the extra pods are indistinguishable from the rest of the primary fleet. The buffer's value is that it is a *separate, identifiable* set of pods, which enables three things a lowered threshold cannot:

- **Separate sizing, clear intent.** A lowered threshold folds two decisions into one number — how big the fleet should be at steady state, and how much spare capacity to hold for bursts. The operator cannot change one without moving the other, and the replica count does not show how much of the fleet is spare. A buffer Deployment states it directly: `N` warm pods on standby, sized on its own, while the primary trigger keeps scaling the fleet as before.

- **Separate handling.** Because buffer pods are their own workload and receive no traffic, the cluster can treat them differently — for example, give them lower priority so they are evicted first under resource pressure. Pods added by a lowered threshold are ordinary primary pods; the cluster cannot tell which are spare, so it cannot reclaim them first.

- **Different buffer backends.** A separate buffer workload can be swapped out. Today it is plain Ready pods; later, the same routing behavior could be served by a cheaper form of standby — for example, snapshotted pods that restore faster than a cold start and use fewer resources while idle. A lowered threshold has no such option: the extra capacity is always full-cost running pods.

- **Separate placement.** A buffer Deployment can have its own scheduling rules — for instance, run on stable on-demand nodes while the primary runs on preemptible capacity, or spread across zones for resilience. A lowered threshold cannot control where the extra pods land, because they are just more primary pods.

Note that the buffer does not remove cold start entirely: once the buffer is in use, the pods scaling in behind it still cold-start. What the buffer adds is a reserved, separately-managed slice of Ready capacity that covers the reaction window — with the freedom in sizing, handling, backend, and placement that a single scaling threshold cannot give.

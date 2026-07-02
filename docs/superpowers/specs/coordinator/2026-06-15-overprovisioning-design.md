# Overprovisioning buffer via a buffer variant (KEDA)

**Status:** Draft (revised 2026-07-02 — direction 2, buffer-variant Deployment)
**Date:** 2026-06-15
**Audience:** WVA contributors, llm-d-router (EPP) contributors

## Problem

Model server cold start is slow (minutes). When demand bursts, KEDA computes
the right replica count quickly, but new pods are not Ready in time — the
burst is served by an undersized fleet and SLOs are missed.

We want a way to keep `N` extra warm pods on standby. Extra pods are real,
Ready, fully initialized model servers; they receive no traffic until the
primary fleet is saturated, at which point EPP fails over to them
instantaneously — no cold start, no controller-mediated promotion.

## Goals

- Keep a configurable number of warm buffer pods alongside the primary fleet.
- Hide buffer pods from routing until the primary fleet is saturated.
- Fail over to buffer pods in milliseconds when saturation is detected —
  the mechanism is EPP's own routing decision, not a controller-issued
  label flip.
- Scale down cleanly: buffer stays at `N` regardless of primary demand;
  primary scales on its own metric.
- Integrate with KEDA `ScaledObject` on the primary. Buffer replica count is
  fixed on the buffer Deployment and does not depend on KEDA at all.
- No new CRDs.
- No changes to the deprecated `VariantAutoscaling` CRD.

## Non-goals (v1)

- Cross-model / cross-pool buffer sharing (one buffer serves multiple
  primaries).
- Predictive buffer sizing based on historical load.
- Buffer pods on a different accelerator SKU than the primary. (Same PodSpec
  in v1; different-SKU buffers are a follow-up.)
- Auto-tuning `N`. The admin sets it.
- Direct `HorizontalPodAutoscaler` support without KEDA.

## Concepts

- **Primary variant.** The Deployment already being scaled by KEDA. Pods
  carry `llm-d.ai/variant=primary` (or no `variant` label — the filter treats
  an absent label as `primary`).
- **Buffer variant.** A sibling Deployment with the same PodSpec, fixed
  `spec.replicas=N`, no autoscaler. Pods carry `llm-d.ai/variant=buffer`.
- **InferencePool.** The two Deployments share a pool via a common label
  (e.g., `model=foo`). One `InferencePool.spec.selector` selects both
  variants; EPP loads both into its datastore as normal endpoints.
- **Buffer gate filter.** A new EPP scheduling `Filter` plugin that drops
  `variant=buffer` endpoints from candidates unless the primary variant is
  saturated (per EPP's existing `SaturationDetector`).

## High-level design

```
   +-------------------------+           +--------------------------+
   |  Primary Deployment     |           |  Buffer Deployment       |
   |  labels: variant=primary|           |  labels: variant=buffer  |
   |  replicas: managed by   |           |  replicas: fixed N       |
   |  ScaledObject 'foo'     |           |  (no ScaledObject)       |
   +-----------+-------------+           +------------+-------------+
               |                                      |
               +--------------+     +-----------------+
                              |     |
                              v     v
                        +---------------+
                        | InferencePool |
                        |  selector:    |
                        |    model=foo  |  (unions both variants)
                        +-------+-------+
                                |
                                v
                        +---------------+
                        |     EPP       |
                        | scheduler:    |
                        |  buffer-gate  |  <- new Filter plugin
                        |    filter     |
                        +---------------+
```

- **WVA has no controller in this design.** Nothing to reconcile beyond
  what the operator authors declaratively. Optionally we ship a small
  admission validator (see "Optional guardrails" below) but no reconciliation
  loop.
- **EPP change is one Filter plugin** plus registration. The Filter reuses
  the existing `SaturationDetector` interface for the saturation signal.
- **KEDA behavior is unchanged.** The primary `ScaledObject` scales the
  primary variant. The buffer Deployment is scaled by nothing — its
  `spec.replicas=N` is stable; Kubernetes' Deployment controller refills any
  pod that dies.

## Configuration surface

There is no controller-managed configuration surface. The operator authors:

1. A primary Deployment (existing).
2. A primary `ScaledObject` targeting the primary Deployment (existing).
3. A **buffer Deployment** — same PodSpec as primary, `spec.replicas=N`,
   `variant=buffer` label on template.
4. A shared `InferencePool` whose selector matches both variants.
5. **In the EPP config**, enable the `buffer-gate` filter in the default
   scheduling profile.

The primary's KEDA trigger PromQL should be scoped to `variant=primary` to
avoid metric dilution (see Risks).

### Optional guardrails

WVA can ship a validating admission webhook (v1 or a follow-up) that:

- Warns if a Deployment carrying `variant=buffer` has an autoscaler
  attached (`ScaledObject` or `HorizontalPodAutoscaler` referencing it).
- Warns if a primary `ScaledObject.spec.triggers[*].metadata.query` does
  not filter by `variant=primary` (best-effort static check).

These are advisory. The core feature does not depend on them.

## EPP integration (the load-bearing component)

The change is a new Filter plugin under
`pkg/epp/framework/plugins/scheduling/filter/buffergate/`. It implements the
`scheduling.Filter` interface
(`pkg/epp/framework/interface/scheduling/plugins.go`).

### Contract

```go
package buffergate

// Filter admits endpoints labeled variant=buffer only when the primary
// variant is saturated. Endpoints not labeled variant=buffer (i.e., primary)
// always pass.
type Filter struct {
    // Injected via plugin config
    labelKey       string          // default "llm-d.ai/variant"
    bufferValue    string          // default "buffer"
    saturationDet  fwkfc.SaturationDetector
}

// Filter is called once per request by the scheduler with the current
// endpoint candidate set for the pool.
func (f *Filter) Filter(ctx context.Context, endpoints []fwkdl.Endpoint) []fwkdl.Endpoint {
    // Partition into primary and buffer sub-slices.
    primary, buffer := partition(endpoints, f.labelKey, f.bufferValue)

    // If there are no buffer endpoints, nothing to gate.
    if len(buffer) == 0 {
        return primary
    }

    // Reuse EPP's existing SaturationDetector over the primary sub-slice.
    if f.saturationDet.SaturatedOver(ctx, primary) {
        return endpoints // admit all — primary + buffer
    }
    return primary
}
```

### Why a Filter (not a ProfileHandler or a dual-pool datastore)

- Filters get the pool's endpoint slice as input and can inspect all of them
  before returning a narrower set. This is exactly the shape needed.
- The existing `bylabel` filter
  (`pkg/epp/framework/plugins/scheduling/filter/bylabel/filter.go`) is the
  precedent for label-based per-endpoint decisions.
- A ProfileHandler would need two profiles and a decision on which to run;
  simpler to keep one profile and let the filter narrow the set.
- The datastore does NOT need to change. A single `InferencePool` selecting
  both variants is what already happens for role-based disaggregation
  (`llm-d.ai/role=decode|prefill`) — the codebase treats sub-fleets within
  one pool as normal.

### Saturation signal — reuse `SaturationDetector`

EPP already computes aggregate pool saturation in two places:

- `pkg/epp/framework/plugins/flowcontrol/saturationdetector/utilization/detector.go`
  — KV cache utilization and queue depth thresholds.
- `pkg/epp/framework/plugins/flowcontrol/saturationdetector/concurrency/detector.go`
  — concurrency limits.

Both implement the `flowcontrol.SaturationDetector` interface
(`pkg/epp/framework/interface/flowcontrol/plugins.go`). Today it's used only
in the admission control path
(`pkg/epp/flowcontrol/controller/internal/processor.go`).

**Change required.** Expose the same detector to scheduling filters. Two
options:

1. **Extract a small `SaturatedOver(ctx, endpoints)` method** onto the
   detector interface. Reuse the same threshold config, applied over a
   caller-supplied subset. Small refactor; filters import the interface.
2. **Register the detector as a scheduling-side plugin dependency.** The
   scheduler framework already supports plugin composition; the filter
   declares a dependency on the same `SaturationDetector` instance the flow
   controller uses.

Option 1 is cleaner. Either way, the semantics of "saturated" are unchanged
and stay in one place — the buffer feature does not introduce a second
threshold or a second signal.

Cache: EPP's `CachedEndpointCandidates` already caches Filter output for
~50ms, so the saturation check runs at most once per 50ms per pool. Buffer
endpoints appear or disappear from the candidate set within one cache TTL.

### Failure mode: SaturationDetector unavailable

If the filter cannot resolve a `SaturationDetector`, it fails **closed** —
does not admit buffer endpoints. Primary continues to serve; buffer sits
idle. This preserves the invariant that buffer receives no traffic unless
saturation is explicitly detected.

## Data model

No CRDs. No pod labels written by any controller. The `llm-d.ai/variant`
label is authored by the operator on the buffer Deployment's PodTemplate,
same as `llm-d.ai/role` is authored today for disaggregation.

## Lifecycles

### Cold start (KEDA lifts primary from zero)

```
t=0   Primary Deployment idle (0 replicas via KEDA scale-to-zero).
      Buffer Deployment at N replicas — Ready and warm.
t=Δ   First request arrives. Primary has no endpoints; buffer is
      variant=buffer so the gate filter must decide.
      SaturatedOver(primary=[]) → true (empty set is trivially saturated).
      Filter admits buffer endpoints; the request is served by a buffer pod.
t=2Δ  KEDA activates primary (its trigger fires on the incoming load).
      Primary pod starts, becomes Ready. Filter now sees primary=[p1] and
      re-evaluates saturation.
```

Note: buffer's role during cold start is different from during a burst.
This is a real benefit — the same mechanism handles scale-from-zero.

### Burst

```
t=0   A=5 primary pods, N=2 buffer pods. Traffic climbs.
t=Δ   Primary approaches queue-depth / KV-cache threshold. Next incoming
      request: SaturatedOver(primary=[p1..p5]) → true.
      Filter returns all 7 endpoints; the scheduler routes to one — likely
      a buffer pod (its queue is empty).
t=2Δ  Buffer traffic climbs. Primary KEDA metric (filtered by variant=primary)
      also rises. KEDA scales primary to 7.
t=3Δ  New primary pods become Ready. Primary saturation eases;
      SaturatedOver(primary) → false. Filter goes back to
      excluding buffer endpoints.
```

### Scale down

Automatic:

- Primary follows its KEDA trigger down. KEDA terminates primary pods
  normally; buffer is untouched.
- Buffer stays at `N`. No "demotion" event exists.

### Buffer pod dies

Kubernetes Deployment controller replaces it. No controller involvement.

## Metric dilution mitigations

The **primary variant's KEDA trigger must not include buffer pods in its
metric**. Otherwise buffer pods (which normally serve nothing) drag the
average down, and KEDA under-scales the primary. Three mitigations, ranked
by generality:

### (a) PromQL label filter — recommended default

Author the trigger query with an explicit `variant="primary"` predicate:

```
avg(vllm:kv_cache_usage{job="model-server", model="foo", variant="primary"})
```

**Requires.** Prometheus scrape metadata includes the pod's labels as metric
labels (standard behavior with `PodMonitor` / `ServiceMonitor` under the
Prometheus Operator; standard with `honor_labels` + label relabel_configs
in vanilla Prometheus).

**Applicability.** Covers the large majority of real deployments. Failing
cases: metrics ingested through pipelines that strip pod labels (some OTel
collector configs, aggregation gateways).

### (b) Separate PodMonitor per variant — backup

Create two `PodMonitor`s. One matches `variant=primary`, one matches
`variant=buffer`. KEDA's trigger reads only the primary stream. Buffer
metrics are still available for observability under a distinct scrape job.

**Applicability.** Works even when raw metrics don't preserve pod labels,
because the split happens at scrape config, not query time.

**Cost.** Doubles the scrape config. More YAML for operators to maintain.

### (c) Prometheus recording rule — advanced

```
groups:
- name: llm-d
  rules:
  - record: vllm:kv_cache_usage_primary
    expr: avg(vllm:kv_cache_usage{variant="primary"})
```

KEDA's trigger reads the recording rule. Filter is authored once in
Prometheus config; ScaledObjects reference the pre-filtered metric by name.

**Applicability.** Requires operator control over Prometheus configuration.
Not always available in managed-Prometheus environments.

Not recommended (documented for completeness):

- Turning off scrape on buffer pods entirely (loses observability).
- Reworking the KEDA formula to divide by an "active count" query (adds
  complexity for no net benefit over (a)).

**The spec recommends (a) as the default and (b) as the documented backup.**
Both should appear in user-facing docs with concrete PromQL / PodMonitor
snippets.

## Failure modes

| Failure | Behavior |
|---|---|
| Buffer Deployment deleted | Feature silently off. Filter finds no `variant=buffer` endpoints; primary carries all traffic. No harm beyond loss of buffering. |
| Primary Deployment deleted | Buffer stays. Filter finds primary=[]; `SaturatedOver([])→true`; buffer serves all traffic. Documented as intended fallback for admin errors. |
| ScaledObject deleted | Primary Deployment stops scaling. Same as today; buffer unaffected. |
| InferencePool selector misconfigured (excludes buffer) | Filter never sees buffer endpoints; feature off. |
| EPP filter not enabled | Buffer endpoints receive traffic normally (no gate). Feature degrades to "extra always-serving pods" — safe but not warm-standby. Detected by admission webhook (guardrails) if enabled. |
| SaturationDetector unavailable to filter | Filter fails closed: buffer endpoints stay excluded. No harm. |
| Primary trigger PromQL includes buffer pods | KEDA under-scales primary; buffer sees more traffic than intended. Metric-dilution mitigation section applies. Documented; guardrail can warn. |
| Prometheus down / scaler in fallback | KEDA holds primary at fallback replicas; buffer unaffected; EPP continues routing via the filter. Independent of Prometheus availability. |
| Node failure kills buffer pod | Deployment controller replaces it. No controller involvement. |

## Risks and open questions

1. **Filter must be enabled in EPP config.** Without it, buffer pods serve
   traffic. Advisory webhook can warn, but v1 puts the responsibility on the
   operator's EPP config.

2. **`SaturationDetector` semantics come from EPP.** WVA has no say in what
   "saturated" means; it's whatever the deployed EPP instance is configured
   to compute (utilization vs concurrency detector). This is a feature, not
   a bug: routing decisions stay in the router.

3. **`SaturatedOver(subset)` is a new interface method.** Existing
   detectors compute over the whole pool. The refactor to accept a
   caller-supplied subset is straightforward for `utilization` and
   `concurrency` detectors but must be part of the change.

4. **Filter output caching.** With the 50ms `CachedEndpointCandidates` TTL,
   buffer endpoints appear or disappear at up to 20 Hz — plenty fast for
   burst response. Documented so operators know the ceiling.

5. **"Two Deployments" is a mild UX regression** compared to a single
   Deployment with role labels (as done for disaggregation). This is
   inherent to the design — pods can't change owner, so buffer pods must
   belong to a distinct Deployment. Optional future work: a WVA-owned
   `BufferSpec` CRD that hides the dual-Deployment YAML behind one object.

6. **Same PodSpec constraint.** Buffer and primary must be identical for
   traffic to be interchangeable. Enforced by convention in v1; future work
   could add an admission check.

7. **`InferencePool` selector must match both variants.** If the operator
   uses a fine-grained selector that excludes buffer pods (e.g., matches
   only `variant=primary`), the buffer is invisible to EPP. Documented.

## Integration points

### llm-d-workload-variant-autoscaler (this repo)

- **No new reconciler for v1.** WVA does not participate at runtime.
- Optional admission webhook under `internal/webhook/buffervariant/`:
  - Warns on unsupported buffer configurations.
  - Warns on primary trigger PromQL that doesn't filter by variant.
- No new RBAC.
- No new dependencies.
- Documentation additions under `docs/user-guide/`.

### llm-d-router

- **New Filter plugin** at
  `pkg/epp/framework/plugins/scheduling/filter/buffergate/`.
- **`SaturationDetector.SaturatedOver(ctx, endpoints)` method** added to the
  interface at
  `pkg/epp/framework/interface/flowcontrol/plugins.go`, implemented by the
  existing utilization and concurrency detectors.
- Plugin registration in the scheduler config schema.
- No RBAC changes.
- Datastore unchanged.

### KEDA / Prometheus / operator YAML

- Buffer Deployment (see YAML example below).
- Primary trigger PromQL filtered by `variant=primary`
  (or backup: PodMonitor split).
- EPP config includes `buffergate` filter in the default scheduling profile.

## Example YAML

```yaml
# Primary Deployment — existing, unchanged
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foo-primary
  labels: {model: foo, llm-d.ai/variant: primary}
spec:
  selector:
    matchLabels: {model: foo, llm-d.ai/variant: primary}
  template:
    metadata:
      labels: {model: foo, llm-d.ai/variant: primary}
    spec: {containers: [...]}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: foo-primary}
spec:
  scaleTargetRef: {name: foo-primary}
  minReplicaCount: 1
  maxReplicaCount: 20
  triggers:
  - type: prometheus
    metadata:
      serverAddress: http://prometheus:9090
      # Metric dilution mitigation (a): explicit variant=primary filter
      query: 'avg(vllm:kv_cache_usage{model="foo", variant="primary"})'
      threshold: "0.7"
---
# Buffer Deployment — same PodSpec, fixed replicas, no autoscaler
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foo-buffer
  labels: {model: foo, llm-d.ai/variant: buffer}
spec:
  replicas: 2                     # N buffer pods
  selector:
    matchLabels: {model: foo, llm-d.ai/variant: buffer}
  template:
    metadata:
      labels: {model: foo, llm-d.ai/variant: buffer}
    spec: {containers: [...]}     # same containers as primary
---
# One InferencePool selects both variants
apiVersion: inference.networking.x-k8s.io/v1
kind: InferencePool
metadata: {name: foo}
spec:
  selector: {model: foo}          # matches primary AND buffer pods
  targetPortNumber: 8000
  extensionRef: {name: foo-epp}
---
# EPP config enables the buffer-gate filter
apiVersion: v1
kind: ConfigMap
metadata: {name: foo-epp-config}
data:
  config.yaml: |
    schedulingProfiles:
    - name: default
      filters:
      - name: buffergate
        config:
          labelKey: llm-d.ai/variant
          bufferValue: buffer
      - name: prefix-cache-affinity
      # ... other filters
```

## Out of scope (explicitly)

- Cross-pool / cross-model shared buffer pools.
- CRD for `BufferSpec` (a follow-up if the dual-Deployment YAML proves
  awkward).
- Automatic authoring of the buffer Deployment from the primary
  Deployment's spec.
- Buffer pods on a cheaper accelerator SKU than primary.
- Prediction-based buffer sizing.
- Direct HPA support without KEDA.

## Testing strategy

Unit (llm-d-router):

- `buffergate` filter: primary saturated → admits buffer; not saturated →
  drops buffer; empty primary → admits buffer; empty buffer → no-op.
- `SaturationDetector.SaturatedOver(subset)` correctness with utilization
  and concurrency detectors.
- Filter respects the `CachedEndpointCandidates` TTL.

Unit (WVA, if guardrails are shipped):

- Admission webhook flags buffer Deployment with an attached scaler.
- Webhook flags primary trigger PromQL without a variant filter.

Integration (envtest with KEDA CRDs):

- Full YAML from the example applies cleanly; buffer Deployment reaches
  `Available` at N replicas.

E2E (Make target):

- Buffer pods idle while primary serves. Trigger saturation via a load
  generator; verify traffic shifts to buffer within one candidate-cache TTL.
- Kill a buffer pod; verify Deployment controller replaces it and EPP picks
  the replacement up.
- Scale primary via KEDA; verify buffer stays at `N`.

## Migration / rollout

- Existing deployments without a buffer Deployment behave identically —
  no `variant=buffer` endpoints exist, filter is a no-op.
- Adding buffering to an existing deployment:
  1. Author the buffer Deployment.
  2. Ensure the primary trigger PromQL filters by `variant=primary` (or
     split PodMonitors).
  3. Enable the `buffergate` filter in EPP config; restart EPP.
- Removing buffering: delete the buffer Deployment. Filter has no effect;
  primary continues to serve.

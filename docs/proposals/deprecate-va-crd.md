# Proposal: Deprecate VariantAutoscaling CRD in Favor of HPA/KEDA

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-05-05
**Last Updated:** 2026-05-05

---

## Problem Statement

The Workload Variant Autoscaler (WVA) introduces a custom VariantAutoscaling CRD and a dedicated controller that acts as an intermediary between metrics sources (vLLM, EPP) and the actual scaling mechanism (HPA/KEDA). Today the flow is:

```
vLLM/EPP metrics → Prometheus → WVA controller → wva_desired_replicas metric → HPA/KEDA → scale
```

This indirection adds operational complexity:
- A custom CRD that operators must learn and manage
- A dedicated controller with its own failure modes, status management, and actuator logic
- Tight coupling between optimization logic and the CRD lifecycle
- Duplication of what HPA/KEDA already does (metrics → scaling decision → actuation)

Much of the WVA logic can be expressed as Prometheus recording rules consumed directly by HPA/KEDA, reducing the architecture to:

```
vLLM/EPP metrics → Prometheus recording rules → HPA/KEDA → scale
```

---

## Goals

1. Eliminate the VariantAutoscaling CRD as the primary user-facing API
2. Make HPA or KEDA ScaledObject the entry point for autoscaling configuration
3. Represent variant cost as an annotation on HPA/KEDA rather than a CRD field
4. Enable simple deployments (single variant per model) to work without any custom controller
5. Retain advanced features (multi-variant cost optimization, queueing model) in a simplified WVA that operates on standard HPA/KEDA objects
6. Reassess existing analyzers for fitness in the new architecture

## Non-Goals

- Rewriting HPA or KEDA — we use them as-is
- Building a general-purpose autoscaling framework
- Changing how vLLM exposes metrics (though we may request upstream improvements)
- Replacing the EPP — we consume its existing metrics

---

## Background

### Current WVA Architecture

The WVA system consists of:

1. **VariantAutoscaling CRD** (`llmd.ai/v1alpha1`): Fields include `scaleTargetRef`, `modelID`, `minReplicas`, `maxReplicas`, `variantCost`
2. **Saturation Engine** (30s optimization loop): Collects vLLM metrics, runs analyzers (V1/V2 saturation, queueing model), runs optimizers (cost-aware, GPU-limited fair-share), applies enforcers (scale-to-zero)
3. **Scale-from-Zero Engine**: Monitors EPP `inference_extension_flow_control_queue_size` to wake scaled-to-zero deployments
4. **Metrics Emission**: Emits `wva_desired_replicas`, `wva_current_replicas`, `wva_desired_ratio`
5. **External Actuation**: HPA (via prometheus-adapter) or KEDA ScaledObject reads `wva_desired_replicas` and performs actual scaling

### Metrics Consumed

| Source | Metrics |
|--------|---------|
| vLLM pods | `vllm:kv_cache_usage_perc`, `vllm:num_requests_waiting`, `vllm:cache_config_info`, `vllm:request_prompt_tokens_*`, `vllm:request_generation_tokens_*`, `vllm:request_success_total` |
| EPP (scheduler) | `inference_extension_flow_control_queue_size`, `inference_extension_scheduler_attempts_total`, TTFT/ITL metrics |

---

## Feature Inventory & Porting Assessment

| # | Feature | Complexity | Strategy |
|---|---------|-----------|----------|
| 1 | Basic saturation scaling (KV cache + queue threshold) | Simple | Prometheus recording rules + HPA/KEDA |
| 2 | Token-based capacity (V2 analyzer) | Medium | Needs reassessment (see below) |
| 3 | Cost-aware multi-variant optimization | Hard | WVA coordinator adjusting HPA bounds |
| 4 | GPU-limited fair-share | Hard | Same coordinator with GPU inventory awareness |
| 5 | Scale-to-zero | Simple | KEDA `idleReplicaCount: 0` / HPA `minReplicas: 0` |
| 6 | Scale-from-zero | Medium | KEDA trigger on EPP flow control queue > 0 |
| 7 | P/D disaggregation | Medium | Separate HPA/ScaledObject per role |
| 8 | Queueing model (Kalman filter + M/G/1) | Hard | Needs reassessment (see below) |
| 9 | Safety net metrics | Eliminated | No intermediary = no intermediary failure |
| 10 | ConfigMap-based configuration | Simple | Moves to recording rule params + annotations |

---

## Analyzer Reassessment

This migration is an opportunity to re-evaluate whether the existing analyzers are the right approach for an HPA/KEDA-native model.

### V1 Saturation Analyzer

Already deprecated. Will be removed — not ported.

### V2 Token-Based Analyzer

**Current approach:** Computes per-replica capacity in tokens using `cache_config_info` labels (`num_gpu_blocks`, `block_size`), average input/output tokens, and prefix cache hit rate. Derives k1 (memory-bound) and k2 (compute-bound) capacity.

**Problem:** `cache_config_info` exposes capacity as labels on an info metric, not numeric gauges. The k1/k2 formula cannot be expressed in PromQL (you cannot do arithmetic on label values).

**Options:**
1. **Simplify to utilization-based:** Use `kv_cache_usage_perc` directly as a utilization signal. Recording rule: `desired = current_replicas * (kv_usage / target_utilization)`. No absolute token capacity needed.
2. **Request upstream vLLM change:** Expose `num_gpu_blocks` and `block_size` as numeric gauge metrics. Then the formula is expressible in PromQL.
3. **Keep in WVA:** WVA computes token capacity and emits `llmd:variant_per_replica_capacity` as a metric for recording rules to reference.

### Queueing Model Analyzer

**Current approach:** Online Kalman filter learning service parameters (alpha, beta) from TTFT/ITL. M/G/1 model computes max throughput per replica under SLO constraints.

**Questions:**
1. **Is the Kalman filter justified?** If EPP provides stable TTFT/ITL metrics, a simpler exponential moving average or Prometheus `rate()` may suffice.
2. **Is M/G/1 the right model?** vLLM uses continuous batching, not queue-then-serve. A throughput-ceiling model (observed max RPS at acceptable latency) may be more practical.
3. **Should SLO targets drive scaling?** Alternatively, scale on utilization and let the scheduler handle SLO routing entirely in EPP.

### Recommendation

Start with **utilization-based scaling** (KV cache usage + queue depth) for Phase 0-1. This is expressible in recording rules today. Defer token-capacity and queueing-model refinements to Phase 2-3 where WVA computes what PromQL cannot.

---

## Proposed Solution

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│ Simple deployments (Phase 1): No WVA needed                     │
│                                                                 │
│ vLLM/EPP → Prometheus → Recording Rules → HPA/KEDA → Scale     │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│ Advanced deployments (Phase 2-3): WVA as coordinator            │
│                                                                 │
│ vLLM/EPP → Prometheus → Recording Rules → HPA/KEDA → Scale     │
│                                               ↑                 │
│                              WVA adjusts min/maxReplicas        │
│                              based on cost + queueing model     │
└─────────────────────────────────────────────────────────────────┘
```

### Annotation Schema

Variant cost and model identity move to annotations on HPA or KEDA ScaledObject:

```yaml
metadata:
  annotations:
    llmd.ai/model-id: "meta/llama-3.1-70b"
    llmd.ai/variant-cost: "40.0"
    llmd.ai/gpus-per-replica: "4"
    llmd.ai/role: "decode"                  # "prefill" | "decode" | "both"
    llmd.ai/priority: "1.0"                 # Fair-share priority
    llmd.ai/scale-to-zero-retention: "10m"  # Idle period before scale-to-zero
```

### Prometheus Recording Rules

**Tier 1 — Basic Saturation Signals:**
```yaml
groups:
- name: llmd_autoscaling
  interval: 30s
  rules:
  - record: llmd:variant_kv_saturation
    expr: |
      max by (namespace, model_name, deployment) (
        max_over_time(vllm:kv_cache_usage_perc[1m])
      )

  - record: llmd:variant_queue_depth
    expr: |
      max by (namespace, model_name, deployment) (
        max_over_time(vllm:num_requests_waiting[1m])
      )
```

**Tier 2 — Utilization-Based Desired Replicas:**
```yaml
  - record: llmd:variant_desired_replicas
    expr: |
      ceil(
        count by (namespace, model_name, deployment) (vllm:kv_cache_usage_perc)
        * max by (namespace, model_name, deployment) (
            max_over_time(vllm:kv_cache_usage_perc[1m])
          )
        / 0.80
      )
```

Note: This is the simplified utilization-based approach. Full token-capacity formulas, if needed, are computed by WVA and emitted as metrics.

**Tier 3 — Scale-from-Zero Signal:**
```yaml
  - record: llmd:model_has_pending_requests
    expr: |
      (sum by (target_model_name) (inference_extension_flow_control_queue_size) > 0)
        or vector(0)
```

### What Still Requires WVA (Without the CRD)

| Logic | WVA needed? | Why |
|-------|------------|-----|
| Basic saturation, scale-to/from-zero, P/D | **No** | Pure recording rules + HPA/KEDA |
| Cost-aware multi-variant | **Yes** | Cross-deployment comparison |
| GPU-limited fair-share | **Yes** | Global constraint enforcement |
| Queueing model | **Yes** | Stateful parameter learning |

WVA packages both the cost coordinator and queueing model as components of a single binary. It is much simpler than today's WVA: no CRD, no full optimization engine pipeline, no actuator/status management. It only adjusts HPA min/max bounds and emits computed metrics.

---

## Implementation Phases

### Phase 0: Dual-Mode Foundation (non-breaking)

**Goal:** Deploy recording rules alongside WVA; validate correctness by comparing outputs.

**Deliverables:**
- Publish `PrometheusRule` resource with Tier 1-3 recording rules
- Add annotations (`llmd.ai/model-id`, `llmd.ai/variant-cost`) to existing KEDA/HPA samples
- Ensure vLLM ServiceMonitor (in `config/prometheus/`) adds `deployment` label via relabeling
- Compare `llmd:variant_desired_replicas` recording rule vs `wva_desired_replicas` to validate

**Success Criteria:** Recording rule output converges within 1 replica of WVA output across multiple scale events.

---

### Phase 1: Single-Variant Direct HPA/KEDA

**Goal:** Users with one variant per model use HPA/KEDA directly — no WVA needed.

**Deliverables:**
- New kustomize overlay in `config/direct-hpa/` deploying:
  - PrometheusRule (recording rules)
  - HPA or KEDA ScaledObject pointing at `llmd:variant_desired_replicas`
  - No VA CRD, no WVA controller
- KEDA ScaledObject sample:
  ```yaml
  triggers:
  - type: prometheus
    metadata:
      query: llmd:variant_desired_replicas{deployment="X",namespace="Y"}
      threshold: '1'
      activationThreshold: '0'
  ```
- Scale-to-zero: `idleReplicaCount: 0` + activation on EPP queue size
- P/D: Separate ScaledObjects per role with role-filtered recording rules

**Success Criteria:** Full scale-up, steady-state, scale-down, scale-to-zero, scale-from-zero lifecycle works without WVA under load (`hack/burst_load_generator.sh`).

---

### Phase 2: Multi-Variant Cost Coordinator

**Goal:** Multi-variant cost optimization without the VA CRD.

**Deliverables:**

WVA evolves into a lightweight coordinator (no CRD) that:
- Discovers work by watching HPA/ScaledObjects annotated with `llmd.ai/model-id` and `llmd.ai/variant-cost`
- Groups HPAs by model-id
- Reads `llmd:variant_desired_replicas` and per-replica capacity from Prometheus
- Runs cost-aware allocation (cheap variants get higher maxReplicas, expensive get lower)
- **Does NOT do scaling** — only adjusts HPA `spec.minReplicas` / `spec.maxReplicas` bounds
- GPU-limited mode: reads node accelerator inventory, enforces global GPU ceiling

**Reuses code from:** `internal/engines/pipeline/cost_aware_optimizer.go` and `greedy_score_optimizer.go`

**Success Criteria:** Under load, cheap variant scales first; on cooldown, expensive variant scales down first.

---

### Phase 3: Queueing Model

**Goal:** SLO-driven scaling without the VA CRD.

**Deliverables:**
- WVA packages both the cost coordinator (Phase 2) and the queueing model analyzer as components of the same binary
- EPP already exposes TTFT/ITL metrics; WVA reads these from Prometheus alongside arrival rate (`inference_extension_scheduler_attempts_total`)
- WVA runs parameter estimation and computes `max_rps_per_replica`
- WVA emits `llmd:model_max_rps_per_replica` as a Prometheus metric
- Recording rule derives desired replicas: `ceil(total_arrival_rate / llmd:model_max_rps_per_replica)`
- HPA/KEDA consumes this recording rule

**Success Criteria:** Queueing-model-driven desired replicas match expected capacity for known traffic patterns.

---

### Phase 4: Deprecation & Removal

**Deliverables:**
- Mark VA CRD deprecated
- Provide `va-to-hpa` migration tool (converts VA resources to annotated HPA/ScaledObjects)
- Remove CRD from kustomize base (`config/crd/`)
- Remove old controller code; retain reusable optimizer logic for the coordinator

---

## EPP Changes Required

| Change | Reason | Phase |
|--------|--------|-------|
| Add `namespace` label to `inference_extension_flow_control_queue_size` | Prevent cross-namespace collision (tracked as TODO #2309) | 0 |
| Add `deployment` label to scheduler dispatch metrics | Per-variant arrival rate for recording rules | 1 |
| Ensure TTFT/ITL metrics are exposed with model-level labels | WVA reads EPP latency metrics for queueing model | 3 |

---

## Alternatives Considered

1. **Keep the VA CRD but simplify the controller** — Rejected because the CRD itself is the operational burden. Users already have HPA/KEDA; adding another object to manage provides marginal value over annotations.

2. **Pure recording rules for everything (no controller at all)** — Not feasible for multi-variant cost optimization (cross-deployment logic) and queueing model (stateful Kalman filter). PromQL cannot express "compare costs across deployments and scale the cheapest."

3. **Move all logic into EPP** — EPP is a request-routing component; making it responsible for scaling decisions conflates concerns. EPP provides metrics; a separate component (or HPA directly) should act on them.

4. **KEDA formula-based multi-trigger** — KEDA supports multiple triggers and can select max/min/average across them, but cannot express "scale deployment A because it's cheaper than deployment B."

---

## Open Questions

1. Should the utilization-based approach (Tier 2 recording rule) use `kv_cache_usage_perc` alone, or combine it with queue depth in a weighted formula?
2. For the queueing model reassessment: should we prototype a simpler throughput-ceiling model (observed max RPS at target P99 latency) before committing to the Kalman filter approach?
3. Is adjusting `spec.minReplicas` / `spec.maxReplicas` on HPAs the right actuation for the coordinator, or should it adjust metric thresholds instead?
4. Should the coordinator emit `llmd:variant_desired_replicas` per-variant (overriding the recording rule) rather than adjusting HPA bounds?

---

## Key Files

- `internal/engines/pipeline/cost_aware_optimizer.go` — Cost-aware logic to retain in coordinator
- `internal/engines/pipeline/greedy_score_optimizer.go` — GPU-limited logic to retain
- `internal/collector/registration/saturation.go` — PromQL queries to convert to recording rules
- `internal/engines/analyzers/saturation_v2/analyzer.go` — V2 formulas for reference
- `config/samples/prometheus-adapter-values.yaml` — Update for new recording rule metrics
- `config/prometheus/` — Add PrometheusRule resources
- `config/direct-hpa/` — New kustomize overlay for direct HPA/KEDA mode

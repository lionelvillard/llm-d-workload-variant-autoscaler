# Saturation Analyzer

## Overview

The Saturation Analyzer is a **fast, reactive, and safe saturation guardrail** that prevents capacity exhaustion by monitoring live vLLM metrics.

**Key Features:**
- ✅ Operates from live vLLM metrics (no offline profiling required)
- ✅ Detects imminent capacity exhaustion (KV-cache or request queue)
- ✅ Makes **per-variant** target replica calculations with cost-awareness
- ✅ Uses ready replicas (those reporting metrics) to avoid excessive scale-up
- ✅ **Prevents cascade scaling** by blocking scale-up when replicas are pending
- ✅ Preserves desired replicas from previous runs
- ✅ Analyzes capacity across all variants of the same model

## Table of Contents

- [Overview](#overview)
- [Analysis Algorithm](#analysis-algorithm)
  - [Identify Non-Saturated Replicas](#identify-non-saturated-replicas)
  - [Calculate Spare Capacity](#calculate-spare-capacity)
  - [Average Spare Capacity](#average-spare-capacity)
  - [Scale-Up Decision](#scale-up-decision)
  - [Scale-Down Safety Simulation](#scale-down-safety-simulation)
- [Decision Logic](#decision-logic)
  - [Calculate Capacity Targets](#calculate-capacity-targets)
- [Multi-Variant Analysis](#multi-variant-analysis)
- [Configuration](#configuration)
  - [Cascade Scaling Prevention](#cascade-scaling-prevention)
- [Architecture](#architecture)
  - [Components](#components)
  - [Data Flow](#data-flow)
- [References](#references)

## Analysis Algorithm

### Identify Non-Saturated Replicas

A replica is **non-saturated** if:
```
kv_cache_usage < kvCacheThreshold AND queue_length < queueLengthThreshold
```

**Default thresholds:**
- `kvCacheThreshold`: 0.80 (80%)
- `queueLengthThreshold`: 5

### Calculate Spare Capacity

For each non-saturated replica:
```
spare_kv_i = kvCacheThreshold - kv_cache_usage_i
spare_queue_i = queueLengthThreshold - queue_length_i
```

### Average Spare Capacity

Across all non-saturated replicas:
```
avg_spare_kv = Σ spare_kv_i / N_non_sat
avg_spare_queue = Σ spare_queue_i / N_non_sat
```

### Scale-Up Decision

Trigger scale-up if:
```
avg_spare_kv < kvSpareTrigger OR avg_spare_queue < queueSpareTrigger
```

**Default triggers:**
- `kvSpareTrigger`: 0.1 (10%)
- `queueSpareTrigger`: 3

### Scale-Down Safety Simulation

Before allowing scale-down, simulate total load redistribution across remaining replicas:

```
remaining_replicas = N_non_sat - 1

// Calculate total load across all non-saturated replicas
total_kv_load = Σ kv_cache_usage_i (for all non-saturated replicas)
total_queue_load = Σ queue_length_i (for all non-saturated replicas)

// Simulate removing one replica: redistribute total load
avg_kv_after_removal = total_kv_load / remaining_replicas
avg_queue_after_removal = total_queue_load / remaining_replicas

// Calculate remaining spare capacity
remaining_spare_kv = kvCacheThreshold - avg_kv_after_removal
remaining_spare_queue = queueLengthThreshold - avg_queue_after_removal
```

**Scale-down is safe if:**
```
remaining_spare_kv >= kvSpareTrigger AND
remaining_spare_queue >= queueSpareTrigger AND
N_non_sat >= 2
```

## Decision Logic

### Calculate Capacity Targets

`CalculateCapacityTargets(capacityAnalysis, variantStates) → map[variantName]targetReplicas`

For each variant, determines target replicas based on **capacity needs only**:

| Condition | Target Replicas | Rationale |
|-----------|----------------|-----------|
| **desired ≠ 0 AND desired ≠ current** | target = **desired** | Preserve previous decision (from CRD status) |
| Capacity needs scale-up | **Cheapest** non-preserved variant: readyReplicas + 1 | Cost-optimized capacity expansion (deterministic: alphabetically first variant on tie) |
| Capacity allows scale-down | **Most expensive** non-preserved variant: readyReplicas - 1 | Cost-optimized capacity reduction (deterministic: alphabetically last variant on tie) |
| Otherwise | target = readyReplicas | No capacity action needed |

**Note:** `readyReplicas` = number of replicas reporting capacity metrics (from `VariantCapacityAnalysis.ReplicaCount`). This prevents excessive scale-up when replicas are still starting up.

**Cascade Scaling Prevention:** Variants with pending replicas (pods that exist but are not yet ready) are skipped during scale-up selection. This prevents the controller from repeatedly scaling up the same variant while previous scale-up operations are still in progress. Pod startup can take 2-7 minutes depending on model size and hardware (container initialization, model loading, health checks).

**Example Output:**
```
Model: llama-70b
Variants:
  - v1-l4 (cost=$5): current=2, ready=2, desired=0 → target=3 (cheapest, scaled up for capacity)
  - v2-a100 (cost=$20): current=4, ready=3, desired=4 → target=4 (preserved desired)

Note: v2-a100 has 4 current replicas but only 3 are ready (reporting metrics).
      Target is set to desired=4 because desired ≠ current.
```

**Key Principles:**
1. **Ready replicas only**: Use replicas reporting metrics to avoid scaling up for not-yet-ready pods
2. **Preserve desired replicas**: When desired ≠ current, always use desired as capacity target
3. **Cost-aware selection**: Cheapest variant for scale-up, most expensive for scale-down
4. **Deterministic tie-breaking**: When variants have equal costs, alphabetically first for scale-up, last for scale-down
5. **Pending replica awareness**: Skip variants with pending replicas during scale-up to prevent cascade scaling

## Configuration

### Cascade Scaling Prevention

**Problem:** Without pending replica awareness, the saturation analyzer could repeatedly trigger scale-up for the same variant before previous scale-up operations complete, leading to excessive replica counts.

**Timeline Example (Without Protection):**
```
T+0s:  Saturation detected → Scale up variant-1 from 2 to 3 replicas
T+30s: New pod created but not ready yet (still loading model)
       Saturation still detected (only 2 ready replicas) → Scale up to 4 replicas
T+60s: Both new pods still starting, saturation persists → Scale up to 5 replicas
T+90s: All 5 pods now ready, but we have 3 extra replicas (over-provisioned)
```

**Solution:** WVA tracks **pending replicas** (`CurrentReplicas - ReadyReplicas`) per variant and skips variants with pending replicas during scale-up selection.

**How It Works:**
1. **Replica State Tracking**: Controller maintains `VariantReplicaState` with:
   - `CurrentReplicas`: Total pods (from Deployment)
   - `DesiredReplicas`: Target from previous run (from CRD status)
   - `PendingReplicas`: Pods that exist but aren't ready (`CurrentReplicas - ReadyReplicas`)

2. **Scale-Up Selection**: When saturation triggers scale-up:
   ```go
   // Pseudo-code from internal/saturation/analyzer.go
   for each variant:
       if variant has preserved desired replicas:
           skip  // Already has decision from previous run
       if variant.PendingReplicas > 0:
           skip  // Wait for pending pods to become ready
       if variant.Cost < cheapest.Cost:
           cheapest = variant

   scale_up(cheapest)  // Only if no pending replicas
   ```

3. **Per-Variant Tracking**: Each variant is tracked independently. If variant-1 has pending replicas, variant-2 can still scale up if it's the cheapest eligible variant.

**Timeline Example (With Protection):**
```
T+0s:  Saturation detected → Scale up variant-1 from 2 to 3 (PendingReplicas=1)
T+30s: Saturation still detected, but variant-1 skipped (has 1 pending replica)
       If variant-2 is cheaper and has no pending replicas → Scale up variant-2
T+90s: variant-1 pod becomes ready (PendingReplicas=0), now eligible for scale-up again
```

**Benefits:**
- ✅ Prevents excessive scale-up during model loading periods (2-7 minutes)
- ✅ Reduces infrastructure costs by avoiding over-provisioning
- ✅ Maintains cost-optimized scaling across multiple variants

**Note:** Scale-down operations are not affected by pending replicas, as removing capacity is always safe when replicas are starting up.

Saturation scaling thresholds are configured via ConfigMap (see [../saturation-scaling-config.md](../saturation-scaling-config.md)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: capacity-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

**Per-model overrides:**
```yaml
  llama-70b-prod: |
    model_id: meta/llama-70b
    namespace: production
    kvCacheThreshold: 0.85
    kvSpareTrigger: 0.15
```

## Architecture

### Components

**1. Saturation Analyzer (`internal/capacity/analyzer.go`)**
- Core analysis logic for saturation-based scaling decisions
- Implements spare capacity calculations
- Performs worst-case scale-down safety simulation
- Makes **per-variant** scaling decisions with cost-awareness

**2. Metrics Collector (`internal/collector/capacity_metrics.go`)**
- Collects vLLM metrics from Prometheus using `max_over_time[1m]` queries
- Queries `constants.VLLMKvCacheUsagePerc` and `constants.VLLMNumRequestsWaiting`
- Uses peak values over 1 minute for safety-first capacity analysis
- Enriches metrics with pod metadata (variant name, accelerator type)

**3. Interfaces (`internal/interfaces/capacity_analyzer.go`)**
- Defines data structures for replica metrics (including variant cost)
- Defines analysis results and per-variant decision types
- Provides interface for capacity analysis
- Defines `VariantDecision` for per-variant scaling decisions
- Defines `VariantReplicaState` for current/desired replica tracking

### Data Flow

```
┌─────────────┐
│  Prometheus │
└──────┬──────┘
       │ vLLM metrics (KV cache, queue length)
       ↓
┌──────────────────┐
│ MetricsCollector │
└────────┬─────────┘
         │ ReplicaMetrics[] (with cost)
         ↓
┌──────────────────────────┐
│ AnalyzeModelCapacity     │  ← CapacityScalingConfig
└────────┬─────────────────┘
         │ ModelCapacityAnalysis (with per-variant breakdown)
         ↓
┌─────────────────────────────┐
│ CalculateCapacityTargets    │  ← VariantReplicaState[] (current/desired from CRD)
│ - Preserves desired replicas      │
│ - Cost-aware variant selection    │
└────────┬──────────────────────────┘
         │ Capacity Targets: map[variantName]targetReplicas
         ↓
┌──────────────────┐
│    Controller    │
└──────────────────┘
```


## References
- Related: [Saturation Scaling Configuration](../saturation-scaling-config.md)

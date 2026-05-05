# Plan: KEDA Direct vLLM Saturation Scaling (V1 Equivalent)

## Context

The WVA V1 saturation engine (in `internal/saturation/analyzer.go`) analyzes per-pod vLLM metrics to make scale-up decisions. Currently, KEDA integration (`config/samples/keda/`) works by reading WVA's *output* metric (`wva_desired_replicas`), requiring the full WVA controller to run.

This plan creates a KEDA-based autoscaling solution that bypasses WVA entirely by using Prometheus recording rules to pre-compute per-model saturation signals, and KEDA ScaledObjects with simple metric selectors and per-model thresholds.

---

## Parameter Mapping

| WVA V1 Parameter | Purpose | KEDA Mapping |
|-----------------|---------|--------------|
| kvCacheThreshold (T₁) | Pod saturated if kv ≥ T₁ | Used in recording rule filtering (Phase 2 only) |
| queueLengthThreshold (T₂) | Pod saturated if queue ≥ T₂ | Used in recording rule filtering (Phase 2 only) |
| kvSpareTrigger (S₁) | Scale-up if avg spare kv < S₁ | KEDA threshold = T₁ - S₁ |
| queueSpareTrigger (S₂) | Scale-up if avg spare queue < S₂ | KEDA threshold = T₂ - S₂ |

The spare triggers (S₁, S₂) are essential. Without them (threshold = T₁), KEDA would never scale up because the average of pods is typically below T₁. The spare trigger lowers the KEDA threshold to T₁-S₁, creating a proactive scaling band:

```
|-----------|--------------|----------|
0         T₁-S₁          T₁         1.0
            ↑              ↑
     KEDA fires here    pods saturated here
     (proactive)        (too late)
```

---

## Phase 1: Simple Recording Rules (No Filtering)

### Approach

Average ALL pods (no saturated-pod filtering). This is a conservative approximation of WVA V1 — it scales up at least as aggressively, and often identically.

### Recording Rules

Two generic rules, zero configuration, work for all models automatically:

```yaml
groups:
- name: wva-saturation-signals
  interval: 15s
  rules:
  # Average KV cache usage per model (all pods)
  - record: wva:avg_kv_cache_usage
    expr: |
      avg by (model_name, namespace) (
        max by (pod, model_name, namespace) (max_over_time(vllm:kv_cache_usage_perc[1m]))
      )

  # Average queue length per model (all pods)
  - record: wva:avg_queue_length
    expr: |
      avg by (model_name, namespace) (
        max by (pod, model_name, namespace) (max_over_time(vllm:num_requests_waiting[1m]))
      )
```

### KEDA ScaledObject

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: PLACEHOLDER-saturation-scaler
  namespace: default
  labels:
    app: PLACEHOLDER
  annotations:
    llm-d.ai/kvCacheThreshold: "0.80"
    llm-d.ai/queueLengthThreshold: "5"
    llm-d.ai/kvSpareTrigger: "0.10"
    llm-d.ai/queueSpareTrigger: "3"
    llm-d.ai/derived-kv-threshold: "0.70"
    llm-d.ai/derived-queue-threshold: "2"
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: PLACEHOLDER
  pollingInterval: 15
  cooldownPeriod: 60
  initialCooldownPeriod: 60
  minReplicaCount: 1
  maxReplicaCount: 10
  fallback:
    failureThreshold: 3
    replicas: 2
    behavior: "currentReplicasIfHigher"
  advanced:
    restoreToOriginalReplicaCount: false
    horizontalPodAutoscalerConfig:
      name: keda-saturation-PLACEHOLDER
      behavior:
        scaleDown:
          stabilizationWindowSeconds: 120
          policies:
          - type: Pods
            value: 1
            periodSeconds: 60
        scaleUp:
          stabilizationWindowSeconds: 0
          policies:
          - type: Pods
            value: 1
            periodSeconds: 15
  triggers:
  - type: prometheus
    name: kv-cache-saturation
    metadata:
      serverAddress: http://prometheus.monitoring.svc.cluster.local:9090
      query: wva:avg_kv_cache_usage{model_name="PLACEHOLDER",namespace="default"}
      # threshold = kvCacheThreshold - kvSpareTrigger = 0.80 - 0.10 = 0.70
      threshold: '0.70'
      activationThreshold: '0'
      metricType: "Value"
      unsafeSsl: "true"
  - type: prometheus
    name: queue-length-saturation
    metadata:
      serverAddress: http://prometheus.monitoring.svc.cluster.local:9090
      query: wva:avg_queue_length{model_name="PLACEHOLDER",namespace="default"}
      # threshold = queueLengthThreshold - queueSpareTrigger = 5 - 3 = 2
      threshold: '2'
      activationThreshold: '0'
      metricType: "Value"
      unsafeSsl: "true"
```

### Advantages

- **Zero prerequisites** — no config metrics exporter needed
- **Truly generic** — recording rules auto-discover all models via label aggregation
- **All-saturated handled naturally** — saturated pods raise the average above threshold, KEDA fires
- **Conservative** — scales up at least as aggressively as WVA V1 (never misses a needed scale-up)

### Behavioral Difference from WVA V1

Phase 1 is slightly more aggressive than WVA V1 because saturated pods inflate the average. In most scenarios (all pods healthy, or very high saturation) the behavior is identical. The divergence occurs only when a mix of saturated and healthy pods exists AND the healthy pods still have adequate headroom.

---

## Phase 2: Filtered Recording Rules (Exact WVA V1 Parity)

### Motivation

Phase 1 averages all pods, including saturated ones. This causes unnecessary scale-ups when a few saturated pods inflate the average even though the remaining healthy pods have sufficient spare capacity.

**Example of divergence**:

| Pod | kv_usage | queue | Saturated? |
|-----|----------|-------|-----------|
| A   | 0.90     | 2     | Yes (kv≥0.80) |
| B   | 0.65     | 2     | No |
| C   | 0.65     | 2     | No |

Parameters: T₁=0.80, S₁=0.10, threshold=0.70

**Phase 1 (no filtering)**:
- avg = (0.90 + 0.65 + 0.65) / 3 = 0.733
- Ratio = 0.733 / 0.70 = 1.047 > 1 → **scale-up**

**Phase 2 (with filtering) / WVA V1**:
- Non-saturated = {B, C}
- avg = (0.65 + 0.65) / 2 = 0.65
- Ratio = 0.65 / 0.70 = 0.929 < 1 → **no scale-up**
- WVA V1: avgSpare = 0.80 - 0.65 = 0.15 ≥ 0.10 → **no scale-up**

In this case, pods B and C have 15% spare KV capacity each (above the 10% trigger). The saturated pod A is already overloaded, but the system has enough headroom — scaling up isn't necessary yet. Phase 1 scales up anyway because A inflates the average.

This matters when:
- GPU resources are expensive and unnecessary scale-ups have real cost
- Saturated pods are expected (e.g., a pod processing a very long context) and shouldn't trigger cluster-wide scale-up
- You need exact behavioral parity with WVA V1 for migration validation

### Approach

Filter out saturated pods before averaging, matching WVA V1's definition: `saturated(r) = kv(r) >= T₁ OR queue(r) >= T₂`.

**Prerequisite**: Per-model saturation thresholds exposed as Prometheus metrics:
- `wva:config_kv_cache_threshold{model_name, namespace}` — e.g., 0.85
- `wva:config_queue_length_threshold{model_name, namespace}` — e.g., 5

These should be exposed by the EPP (Endpoint Picker), which already scrapes vLLM metrics and has access to the saturation detector configuration (SD_KV_CACHE_UTIL_THRESHOLD, SD_QUEUE_DEPTH_THRESHOLD). The EPP is the natural place to publish these as Prometheus gauges since it already owns the per-model threshold configuration and runs a metrics endpoint.

### Recording Rules

```yaml
groups:
- name: wva-saturation-signals
  interval: 15s
  rules:
  # Average KV cache usage across non-saturated pods only (per model)
  - record: wva:avg_kv_nonsaturated
    expr: |
      avg by (model_name, namespace) (
        (
          max by (pod, model_name, namespace) (max_over_time(vllm:kv_cache_usage_perc[1m]))
            < on (model_name, namespace) group_left
          wva:config_kv_cache_threshold
        )
        and on (pod, model_name, namespace)
        (
          max by (pod, model_name, namespace) (max_over_time(vllm:num_requests_waiting[1m]))
            < on (model_name, namespace) group_left
          wva:config_queue_length_threshold
        )
      )

  # Average queue length across non-saturated pods only (per model)
  - record: wva:avg_queue_nonsaturated
    expr: |
      avg by (model_name, namespace) (
        (
          max by (pod, model_name, namespace) (max_over_time(vllm:num_requests_waiting[1m]))
            < on (model_name, namespace) group_left
          wva:config_queue_length_threshold
        )
        and on (pod, model_name, namespace)
        (
          max by (pod, model_name, namespace) (max_over_time(vllm:kv_cache_usage_perc[1m]))
            < on (model_name, namespace) group_left
          wva:config_kv_cache_threshold
        )
      )
```

### KEDA Triggers (Phase 2)

```yaml
triggers:
- type: prometheus
  name: kv-cache-saturation
  metadata:
    query: wva:avg_kv_nonsaturated{model_name="ibm/granite-13b",namespace="production"}
    # threshold = kvCacheThreshold - kvSpareTrigger = 0.85 - 0.15 = 0.70
    threshold: '0.70'
    metricType: "Value"
- type: prometheus
  name: queue-length-saturation
  metadata:
    query: wva:avg_queue_nonsaturated{model_name="ibm/granite-13b",namespace="production"}
    # threshold = queueLengthThreshold - queueSpareTrigger = 5 - 3 = 2
    threshold: '2'
    metricType: "Value"
```

### Edge Case: All Pods Saturated

When all pods are saturated, the filtering produces an empty set and the recording rule returns no data. KEDA sees no metric and does not fire. WVA V1 would scale up in this case (avgSpare=0 < trigger).

Mitigation: keep the Phase 1 rules deployed alongside Phase 2 as a fallback trigger, or accept this as a known divergence (if all pods are saturated, the system is already in trouble and other mechanisms like KEDA's `fallback` config will maintain minimum replicas).

---

## File Organization (Kustomize Overlays)

One overlay per model:

```
config/samples/keda-vllm-saturation/
  base/
    kustomization.yaml
    scaledobject.yaml
    prometheusrule.yaml
  overlays/
    granite-13b/
      kustomization.yaml
    llama-3-70b/
      kustomization.yaml
```

### Example: `overlays/granite-13b/kustomization.yaml`

Model: `ibm/granite-13b` in namespace `production`
Parameters: T₁=0.85, T₂=5, S₁=0.15, S₂=3 → kv_threshold=0.70, queue_threshold=2

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../base
namespace: production
patches:
- target:
    kind: ScaledObject
    name: PLACEHOLDER-saturation-scaler
  patch: |-
    - op: replace
      path: /metadata/name
      value: granite-13b-saturation-scaler
    - op: replace
      path: /metadata/namespace
      value: production
    - op: replace
      path: /metadata/labels/app
      value: granite-13b
    - op: replace
      path: /metadata/annotations/llm-d.ai~1kvCacheThreshold
      value: "0.85"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1kvSpareTrigger
      value: "0.15"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1derived-kv-threshold
      value: "0.70"
    - op: replace
      path: /spec/scaleTargetRef/name
      value: granite-13b
    - op: replace
      path: /spec/maxReplicaCount
      value: 8
    - op: replace
      path: /spec/advanced/horizontalPodAutoscalerConfig/name
      value: keda-saturation-granite-13b
    - op: replace
      path: /spec/triggers/0/metadata/query
      value: wva:avg_kv_cache_usage{model_name="ibm/granite-13b",namespace="production"}
    - op: replace
      path: /spec/triggers/0/metadata/threshold
      value: "0.70"
    - op: replace
      path: /spec/triggers/1/metadata/query
      value: wva:avg_queue_length{model_name="ibm/granite-13b",namespace="production"}
    - op: replace
      path: /spec/triggers/1/metadata/threshold
      value: "2"
```

### Example: `overlays/llama-3-70b/kustomization.yaml`

Model: `meta/llama-3.1-70b` in namespace `llm-inference`
Parameters: T₁=0.75, T₂=3, S₁=0.10, S₂=1 → kv_threshold=0.65, queue_threshold=2

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../../base
namespace: llm-inference
patches:
- target:
    kind: ScaledObject
    name: PLACEHOLDER-saturation-scaler
  patch: |-
    - op: replace
      path: /metadata/name
      value: llama-3-70b-saturation-scaler
    - op: replace
      path: /metadata/namespace
      value: llm-inference
    - op: replace
      path: /metadata/labels/app
      value: llama-3-70b
    - op: replace
      path: /metadata/annotations/llm-d.ai~1kvCacheThreshold
      value: "0.75"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1queueLengthThreshold
      value: "3"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1kvSpareTrigger
      value: "0.10"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1queueSpareTrigger
      value: "1"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1derived-kv-threshold
      value: "0.65"
    - op: replace
      path: /metadata/annotations/llm-d.ai~1derived-queue-threshold
      value: "2"
    - op: replace
      path: /spec/scaleTargetRef/name
      value: llama-3-70b
    - op: replace
      path: /spec/minReplicaCount
      value: 2
    - op: replace
      path: /spec/maxReplicaCount
      value: 6
    - op: replace
      path: /spec/advanced/horizontalPodAutoscalerConfig/name
      value: keda-saturation-llama-3-70b
    - op: replace
      path: /spec/triggers/0/metadata/query
      value: wva:avg_kv_cache_usage{model_name="meta/llama-3.1-70b",namespace="llm-inference"}
    - op: replace
      path: /spec/triggers/0/metadata/threshold
      value: "0.65"
    - op: replace
      path: /spec/triggers/1/metadata/query
      value: wva:avg_queue_length{model_name="meta/llama-3.1-70b",namespace="llm-inference"}
    - op: replace
      path: /spec/triggers/1/metadata/threshold
      value: "2"
```

### Usage

```bash
kustomize build config/samples/keda-vllm-saturation/overlays/granite-13b/ | kubectl apply -f -
kustomize build config/samples/keda-vllm-saturation/overlays/llama-3-70b/ | kubectl apply -f -
```

---

## Mathematical Proof of Equivalence

### WVA V1 Scale-Up Condition

From [analyzer.go:206-207](internal/saturation/analyzer.go#L206-L207):

```
shouldScaleUp = (avgSpareKv < kvSpareTrigger) OR (avgSpareQueue < queueSpareTrigger)
```

Where (from [analyzer.go:171-172](internal/saturation/analyzer.go#L171-L172)):
```
spareKv(r) = kvCacheThreshold - kv_usage(r)       for r in NonSaturated
avgSpareKv = (1/|N|) * Σ spareKv(r)
```

### Algebraic Derivation

Let N = set of non-saturated replicas, T₁ = kvCacheThreshold, S₁ = kvSpareTrigger.

```
avgSpareKv < S₁
⟺ (1/|N|) * Σᵣ∈N (T₁ - kv(r)) < S₁
⟺ T₁ - (1/|N|) * Σᵣ∈N kv(r) < S₁
⟺ avg(kv, N) > T₁ - S₁
```

### KEDA Trigger Equivalence

KEDA fires when `queryResult / threshold > 1`, i.e., `queryResult > threshold`.

- **Phase 1**: Query = avg(kv, ALL pods). Threshold = T₁ - S₁. Conservative approximation (avg(all) ≥ avg(non-saturated) when saturated pods exist).
- **Phase 2**: Query = avg(kv, non-saturated only). Threshold = T₁ - S₁. Exact equivalence: KEDA fires ⟺ WVA fires. **QED.**

KEDA uses OR across multiple triggers, matching WVA's OR logic.

---

## Four Worked Examples

### Example 1: Scale-Up Triggered by KV Cache

**Parameters**: T₁=0.80, T₂=5, S₁=0.10, S₂=3

| Pod | kv_usage | queue | Saturated? |
|-----|----------|-------|-----------|
| A   | 0.75     | 2     | No |
| B   | 0.76     | 2     | No |

**WVA V1**: avgSpareKv = ((0.80-0.75)+(0.80-0.76))/2 = 0.045 < 0.10 → **scale-up**
**Phase 1**: avg(0.75, 0.76) = 0.755. Ratio = 0.755/0.70 = 1.079 > 1 → **scale-up**
**Phase 2**: same (no saturated pods to filter)

### Example 2: No Scale-Up (Healthy Capacity)

| Pod | kv_usage | queue | Saturated? |
|-----|----------|-------|-----------|
| A   | 0.50     | 1     | No |
| B   | 0.50     | 1     | No |

**WVA V1**: avgSpareKv = 0.30 ≥ 0.10 → **no scale-up**
**Phase 1**: avg = 0.50. Ratio = 0.50/0.70 = 0.714 ≤ 1 → **no scale-up**
**Phase 2**: same

### Example 3: Phase 1 vs Phase 2 Divergence

| Pod | kv_usage | queue | Saturated? |
|-----|----------|-------|-----------|
| A   | 0.90     | 2     | Yes (kv≥0.80) |
| B   | 0.65     | 2     | No |
| C   | 0.65     | 2     | No |

**WVA V1**: N={B,C}. avgSpare = 0.80-0.65 = 0.15 ≥ 0.10 → **no scale-up**
**Phase 1**: avg(0.90, 0.65, 0.65) = 0.733. Ratio = 0.733/0.70 = 1.047 > 1 → **scale-up** (diverges!)
**Phase 2**: avg(0.65, 0.65) = 0.65. Ratio = 0.65/0.70 = 0.929 ≤ 1 → **no scale-up** (matches WVA V1)

### Example 4: Scale-Down Safe

| Pod | kv_usage | queue | Saturated? |
|-----|----------|-------|-----------|
| A   | 0.40     | 1     | No |
| B   | 0.35     | 1     | No |
| C   | 0.45     | 1     | No |

**WVA V1**: avgKvLoad = 0.40, scaleFactor = 3/2 = 1.5, afterRemoval = 0.60, remainingSpare = 0.20 ≥ 0.10 → **safe**
**KEDA**: avg = 0.40. Ratio = 0.40/0.70 = 0.571 < 1. HPA wants scale-down, policy limits to -1 → **scale-down to 2**

---

## Scope Limitations (vs. full WVA V1)

| Aspect | WVA V1 | KEDA (Phase 1) | KEDA (Phase 2) |
|--------|--------|----------------|----------------|
| Scale-up trigger | Exact spare analysis | Conservative (more aggressive) | Exact parity |
| Scale-down | N/(N-1) simulation | HPA policy (-1/min) | HPA policy (-1/min) |
| All-saturated | Scales up | Scales up (natural) | No data (needs fallback) |
| Multi-variant cost | Scales cheapest | One ScaledObject per deployment | Same |
| Prerequisites | None (built-in) | Recording rules only | Recording rules + config exporter |

---

## Verification

1. **PromQL validation**: Query recording rules against test Prometheus, verify output matches hand calculation
2. **KEDA dry-run**: Deploy ScaledObject, check `kubectl get hpa` shows correct metric values
3. **Phase 1 vs Phase 2**: Compare scaling decisions under mixed-saturation scenarios
4. **Edge cases**: All-saturated, zero pods (minReplicaCount), single pod

---

## Future Work: Managing KEDA Triggers Dynamically

The Kustomize overlay approach requires manual threshold computation per model. Two alternatives for tighter integration:

### Alternative 1: WVA Partially Manages ScaledObjects via Server-Side Apply

WVA watches existing ScaledObjects and patches their triggers using SSA. Configuration driven by annotations.

```yaml
metadata:
  annotations:
    llm-d.ai/managed-triggers: "true"
    llm-d.ai/model-id: "ibm/granite-13b"
    llm-d.ai/prometheus-address: "http://prometheus.monitoring.svc:9090"
    llm-d.ai/kvCacheThreshold: "0.85"
    llm-d.ai/queueLengthThreshold: "5"
    llm-d.ai/kvSpareTrigger: "0.15"
    llm-d.ai/queueSpareTrigger: "3"
spec:
  triggers: []  # WVA populates via SSA with field manager "wva-trigger-manager"
```

- User owns structure/policy, WVA owns trigger computation
- Threshold arithmetic computed in Go
- Formula changes require only controller update
- Opt-in per ScaledObject via annotation

### Alternative 2: KEDA External Scaler (gRPC Plugin)

A lightweight Go service implementing the KEDA external scaler gRPC interface, sharing the `internal/saturation` package.

```yaml
triggers:
- type: external
  metadata:
    scalerAddress: "wva-keda-scaler.wva-system.svc.cluster.local:6000"
    modelName: "ibm/granite-13b"
    namespace: "production"
```

- Full algorithmic parity (same Go code)
- Zero PromQL in YAML
- Adding a model requires zero YAML changes

### Comparison

| Approach | Config per model | Prerequisites | WVA dependency |
|----------|-----------------|---------------|----------------|
| Phase 1 (this plan) | Overlay with threshold | Recording rules only | None |
| Phase 2 (this plan) | Overlay with threshold | Recording rules + config exporter | Exporter only |
| WVA manages via SSA | Annotation + ConfigMap | WVA running | Full |
| External scaler gRPC | ConfigMap entry only | Scaler service | Shared library |

---

## Critical Files

- [internal/saturation/analyzer.go](internal/saturation/analyzer.go) — V1 logic (ground truth)
- [internal/collector/registration/saturation.go](internal/collector/registration/saturation.go) — PromQL templates and metric labels
- [config/manager/configmap-saturation-scaling.yaml](config/manager/configmap-saturation-scaling.yaml) — Parameter structure
- [config/samples/keda/scaledobject.yaml](config/samples/keda/scaledobject.yaml) — Existing KEDA sample (structural reference)

# Overprovisioning: warm buffer pods

Keep `N` warm standby pods that absorb traffic bursts instantly, without
waiting for cold start. Buffer pods stay out of routing until the primary
fleet is saturated, then the router fails over to them per-request.

## How it works

- A **buffer Deployment** runs alongside your primary Deployment with the same
  PodSpec, a fixed replica count, and no autoscaler. Its pods carry the label
  `llm-d.ai/buffer: "true"`.
- Both Deployments are selected by one **InferencePool**.
- The EPP **`buffer-gate-filter`** hides buffer endpoints from scheduling
  until the primary sub-fleet's saturation (from the configured
  `SaturationDetector`) reaches a threshold (default `1.0`).

## Setup

### 1. Buffer Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foo-buffer
  labels: {model: foo, llm-d.ai/buffer: "true"}
spec:
  replicas: 2                      # N warm pods
  selector:
    matchLabels: {model: foo, app: foo-buffer}
  template:
    metadata:
      labels: {model: foo, app: foo-buffer, llm-d.ai/buffer: "true"}
    spec: {containers: [ /* identical to primary */ ]}
```

### 2. Keep buffer pods out of the KEDA metric

The primary `ScaledObject` trigger must exclude buffer pods, or they dilute
the metric and KEDA under-scales the primary. Filter on the sanitized label
(`llm-d.ai/buffer` becomes `llm_d_ai_buffer` in Prometheus):

```yaml
query: 'avg(vllm:kv_cache_usage{model="foo", llm_d_ai_buffer=""})'
```

If your scrape pipeline strips pod labels, use a separate `PodMonitor` per
variant so KEDA reads only the primary stream.

### 3. Enable the filter in the EPP config

```yaml
apiVersion: llm-d.ai/v1alpha1
kind: EndpointPickerConfig
plugins:
- type: utilization-detector
- type: buffer-gate-filter
  parameters: {detectorRef: utilization-detector, saturationThreshold: 1.0}
- type: max-score-picker
- type: prefix-cache-scorer
flowControl:
  saturationDetector: {pluginRef: utilization-detector}
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: buffer-gate-filter
  - pluginRef: max-score-picker
  - pluginRef: prefix-cache-scorer
    weight: 2
```

Lower `saturationThreshold` (e.g. `0.9`) to admit buffer pods slightly before
the primary is fully saturated, trading buffer idle-time for earlier burst
absorption.

## Disabling

Delete the buffer Deployment. With no `llm-d.ai/buffer` endpoints the filter is
a no-op; the primary continues to serve.

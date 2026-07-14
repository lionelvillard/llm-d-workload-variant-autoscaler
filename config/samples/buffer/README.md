# Overprovisioning buffer-gate demo (kind)

Shows the `buffer-gate-filter`: warm buffer pods receive **no** traffic while
the primary has capacity, then absorb a burst instantly.

## Layout

| File | Purpose |
|------|---------|
| `primary-deployment.yaml` | Primary sim, no buffer label, scaled by nothing here |
| `buffer-deployment.yaml`  | Buffer sim, `llm-d.ai/buffer: "true"`, fixed 2 replicas |
| `service.yaml`            | ClusterIP Service selecting both primary and buffer pods |
| `epp-values.yaml`         | Helm overlay enabling `buffer-gate-filter` + `utilization-detector` in the EPP |
| `load-job.yaml`           | 40 concurrent requests to saturate the primary |
| `demo.sh`                 | Build EPP image, create kind cluster, run baseline + burst |

> **Note:** The chart (`llm-d-router-standalone`) creates the InferencePool
> automatically, naming it `buffer-demo` and selecting pods with
> `model: foo` (set via `router.modelServers.matchLabels.model=foo`).
> The EPP is `buffer-demo-epp`; traffic enters on port **8081** (the
> sidecar proxy listener).  There is no separate `inferencepool.yaml` in
> this sample — the chart owns that resource.

## Prerequisites

- `kind`, `kubectl`, Docker.
- A local **llm-d-router** checkout on the buffer-gate branch (the filter is
  unreleased). The script builds its EPP image and loads it into kind.

## Run

```bash
# From the repo root. Point at your llm-d-router checkout if not ../llm-d-router.
LLM_D_ROUTER_DIR=../llm-d-router ./config/samples/buffer/demo.sh
```

Expected output: buffer pods report ~0 requests after the light baseline, and
`> 0` after the burst.

## Tear down

```bash
./config/samples/buffer/demo.sh teardown
```

## Notes

- `max-num-seqs=2` and `queueDepthThreshold: 1` make the primary saturate fast
  so the demo is quick; raise them for a more realistic feel.
- The demo does not use KEDA — the buffer gate is a pure router-side routing
  decision and needs only live pod metrics. KEDA (scaling the primary on a
  buffer-excluded metric) is orthogonal and covered in the developer guide.

# Install WVA with the Deployment Script

Use this method when you want end-to-end setup (WVA + supporting components) with project-supported `make` targets.

The script entrypoint is `deploy/install.sh`. The environment-specific behavior is loaded from:

- `deploy/kubernetes/install.sh`
- `deploy/openshift/install.sh`
- `deploy/kind-emulator/install.sh`

## Prerequisites

- Kubernetes v1.32.0 or later (or OpenShift equivalent)
- Helm 3.x
- `kubectl` configured to access your cluster
- Optional for OpenShift: `oc`
- Optional for local emulation: `kind`
- Hugging Face token (`HF_TOKEN`) when deploying llm-d model components

## Quick Start

Set a token when deploying llm-d components:

```bash
export HF_TOKEN="hf_xxxxx"
```

Choose one environment:

```bash
# Kubernetes
make deploy-wva-on-k8s

# OpenShift
make deploy-wva-on-openshift

# Local Kind emulator
make deploy-wva-emulated-on-kind
```

These targets invoke `deploy/install.sh` and apply the environment-specific defaults maintained by this repository.

## Quick Lookup

- [Command-Line Options](#command-line-options)
- [Option Precedence and Interactions](#option-precedence-and-interactions)
- [Environment Variables](#environment-variables)
	- [Core Project and Namespacing](#core-project-and-namespacing)
	- [Image, Release, and Controller Settings](#image-release-and-controller-settings)
	- [Model, SLO, and Workload Tuning](#model-slo-and-workload-tuning)
	- [Deployment Flow Toggles](#deployment-flow-toggles)
	- [HPA and Saturation Parameters](#hpa-and-saturation-parameters)
	- [Scaler Backend and KEDA](#scaler-backend-and-keda)
	- [llm-d and Gateway Variables](#llm-d-and-gateway-variables)
	- [Prometheus and TLS Variables](#prometheus-and-tls-variables)
	- [Internal llm-d Path/Name Variables](#internal-llm-d-pathname-variables)
	- [Multi-Model Testing Variables](#multi-model-testing-variables)
	- [Undeploy and Cleanup Variables](#undeploy-and-cleanup-variables)
	- [Kind Emulator-Specific Variables](#kind-emulator-specific-variables)
	- [Additional Script Inputs](#additional-script-inputs)
- [Platform Overrides](#platform-overrides)
- [Examples](#examples)
- [Known Caveats](#known-caveats)

## Command-Line Options

`deploy/install.sh` supports the following CLI options:

| Option | Description | Default |
|---|---|---|
| `-i`, `--wva-image IMAGE` | WVA image (`repo:tag`) | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest` |
| `-m`, `--model MODEL` | Model ID | `unsloth/Meta-Llama-3.1-8B` |
| `-a`, `--accelerator TYPE` | Accelerator name (for VA labels/config) | `H100` (or auto-detected on non-emulated envs) |
| `-r`, `--release-name NAME` | Helm release name for WVA | `workload-variant-autoscaler` |
| `--infra-only` | Deploy infrastructure only (skips VA/HPA) | `false` |
| `-u`, `--undeploy` | Undeploy mode | `false` |
| `-e`, `--environment ENV` | Environment | `kubernetes` |
| `-h`, `--help` | Print help | - |

Accepted environment values are `kubernetes`, `openshift`, and `kind-emulator`.

## Option Precedence and Interactions

The effective configuration is built in this order:

1. Script defaults from `deploy/install.sh`
2. Caller-provided environment variables
3. `IMG` handling (if set)
4. CLI flag overrides (for example, `--wva-image` overrides `IMG`)
5. Mode mutations (`--infra-only`, `SCALER_BACKEND=keda`)
6. Runtime auto-settings (`SKIP_TLS_VERIFY`, `WVA_LOG_LEVEL`)
7. Environment script overrides (`deploy/<environment>/install.sh`)
8. Interactive prompt for `INSTALL_GATEWAY_CTRLPLANE` unless test mode bypasses prompts

Important interactions:

- `INFRA_ONLY=true` (or `--infra-only`) forces `DEPLOY_VA=false` and `DEPLOY_HPA=false`.
- `SCALER_BACKEND=keda` forces `DEPLOY_PROMETHEUS_ADAPTER=false` and is supported only on `kind-emulator`.
- `HF_TOKEN` is required for non-emulated llm-d deployments.

## Environment Variables

### Core Project and Namespacing

| Variable | Default | Description |
|---|---|---|
| `ENVIRONMENT` | `kubernetes` | Target environment (`kubernetes`, `openshift`, `kind-emulator`) |
| `WVA_PROJECT` | `$PWD` | Repository root used by script paths |
| `WELL_LIT_PATH_NAME` | `inference-scheduling` | llm-d guide path suffix |
| `NAMESPACE_SUFFIX` | `inference-scheduler` | Suffix used for llm-d release names/namespaces |
| `WVA_NS` | `workload-variant-autoscaler-system` | WVA namespace |
| `LLMD_NS` | `llm-d-$NAMESPACE_SUFFIX` | llm-d namespace |
| `MONITORING_NAMESPACE` | `workload-variant-autoscaler-monitoring` | Monitoring namespace |
| `PROMETHEUS_SECRET_NS` | `$MONITORING_NAMESPACE` | Namespace containing Prometheus TLS secret |

### Image, Release, and Controller Settings

| Variable | Default | Description |
|---|---|---|
| `IMG` | unset | Make-style image override (`repo:tag`) |
| `WVA_IMAGE_REPO` | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` | WVA image repository |
| `WVA_IMAGE_TAG` | `latest` | WVA image tag |
| `WVA_IMAGE_PULL_POLICY` | `Always` | WVA image pull policy |
| `WVA_RELEASE_NAME` | `workload-variant-autoscaler` | WVA Helm release name |
| `VALUES_FILE` | `charts/workload-variant-autoscaler/values.yaml` | Helm values file path (can be overridden by env scripts) |
| `WVA_METRICS_SECURE` | `true` | Enables auth on WVA `/metrics` endpoint |
| `WVA_LOG_LEVEL` | `info` | WVA log level (runtime/platform may override) |
| `NAMESPACE_SCOPED` | `true` | Scope controller behavior to namespace when supported |
| `CONTROLLER_INSTANCE` | empty | Multi-controller isolation label |

### Model, SLO, and Workload Tuning

| Variable | Default | Description |
|---|---|---|
| `MODEL_ID` | `unsloth/Meta-Llama-3.1-8B` | Model identifier used in llm-d and VA |
| `DEFAULT_MODEL_ID` | `Qwen/Qwen3-0.6B` | Fallback model identifier in script logic |
| `ACCELERATOR_TYPE` | `H100` | Accelerator label/type |
| `SLO_TPOT` | `10` | Time-per-output-token SLO (ms) |
| `SLO_TTFT` | `1000` | Time-to-first-token SLO (ms) |
| `VLLM_MAX_NUM_SEQS` | empty | vLLM max concurrent sequences per replica |
| `DECODE_REPLICAS` | empty | Optional decode deployment replica override |
| `VLLM_SVC_ENABLED` | `true` | Create/maintain vLLM service |
| `VLLM_SVC_PORT` | `8200` | vLLM service target port |
| `VLLM_SVC_NODEPORT` | `30000` | vLLM NodePort (if NodePort service is used) |

### Deployment Flow Toggles

| Variable | Default | Description |
|---|---|---|
| `DEPLOY_PROMETHEUS` | `true` | Deploy monitoring stack (k8s only; OpenShift overrides) |
| `DEPLOY_WVA` | `true` | Deploy WVA controller/chart |
| `DEPLOY_LLM_D` | `true` | Deploy llm-d infrastructure |
| `DEPLOY_PROMETHEUS_ADAPTER` | `true` | Deploy Prometheus Adapter |
| `DEPLOY_VA` | `true` | Deploy VariantAutoscaling resource |
| `DEPLOY_HPA` | `true` | Deploy HPA resource |
| `INFRA_ONLY` | `false` | Infrastructure-only mode (disables VA/HPA deployment) |
| `SKIP_CHECKS` | `false` | Skip prerequisite checks |
| `E2E_TESTS_ENABLED` | `false` | Test mode behavior (suppresses prompts) |

### HPA and Saturation Parameters

| Variable | Default | Description |
|---|---|---|
| `HPA_STABILIZATION_SECONDS` | `240` | HPA stabilization window |
| `HPA_MIN_REPLICAS` | `1` | HPA min replicas (set `0` for scale-to-zero testing) |
| `ENABLE_SCALE_TO_ZERO` | `true` | Enables llm-d scale-to-zero behavior in values path |
| `KV_SPARE_TRIGGER` | empty | Saturation analyzer KV spare trigger override |
| `QUEUE_SPARE_TRIGGER` | empty | Saturation analyzer queue spare trigger override |

### Scaler Backend and KEDA

| Variable | Default | Description |
|---|---|---|
| `SCALER_BACKEND` | `prometheus-adapter` | `prometheus-adapter` or `keda` |
| `KEDA_NAMESPACE` | `keda-system` | KEDA namespace when KEDA backend is used |

### llm-d and Gateway Variables

| Variable | Default | Description |
|---|---|---|
| `HF_TOKEN` | unset | Hugging Face token (required for non-emulated model downloads) |
| `LLM_D_OWNER` | `llm-d` | llm-d GitHub owner |
| `LLM_D_PROJECT` | `llm-d` | llm-d local directory name |
| `LLM_D_RELEASE` | `v0.3.0` | llm-d git ref |
| `LLM_D_INFERENCE_SCHEDULER_IMG` | `ghcr.io/llm-d/llm-d-inference-scheduler:v0.5.0-rc.1` | Scheduler image override |
| `GATEWAY_PROVIDER` | `istio` | Gateway provider (`istio` or `kgateway`) |
| `INSTALL_GATEWAY_CTRLPLANE` | prompt-driven | Whether the script installs gateway control plane |
| `BENCHMARK_MODE` | `false` | Gateway benchmark mode switch |
| `ITL_AVERAGE_LATENCY_MS` | `20` | Emulator/simulator average inter-token latency |
| `TTFT_AVERAGE_LATENCY_MS` | `200` | Emulator/simulator average TTFT |
| `GATEWAY_API_INFERENCE_EXTENSION_CRD_REVISION` | env-specific | CRD revision override for gateway API extension |

### Prometheus and TLS Variables

| Variable | Default | Description |
|---|---|---|
| `PROM_CA_CERT_PATH` | `/tmp/prometheus-ca.crt` | Local path where script writes CA cert used by WVA |
| `PROMETHEUS_SECRET_NAME` | `prometheus-web-tls` | Secret name used to obtain Prometheus/Thanos cert |
| `PROMETHEUS_URL` | env-specific | Prometheus/Thanos URL used by WVA |
| `PROMETHEUS_BASE_URL` | env-specific | Base service URL before port is appended |
| `PROMETHEUS_PORT` | env-specific | Prometheus/Thanos port |
| `PROMETHEUS_SVC_NAME` | env-specific | Prometheus/Thanos service name |
| `SKIP_TLS_VERIFY` | runtime/env-specific | Toggle TLS verification for Prometheus access |

### Internal llm-d Path/Name Variables

| Variable | Default | Description |
|---|---|---|
| `LLM_D_MODELSERVICE_NAME` | `ms-$WELL_LIT_PATH_NAME-llm-d-modelservice` | llm-d modelservice release/resource name |
| `LLM_D_EPP_NAME` | `gaie-$WELL_LIT_PATH_NAME-epp` | Endpoint picker release/resource name |
| `CLIENT_PREREQ_DIR` | `$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/client-setup` | Path to llm-d client prereq helmfiles |
| `GATEWAY_PREREQ_DIR` | `$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/gateway-provider` | Path to gateway prereq helmfiles |
| `EXAMPLE_DIR` | `$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME` | Path to llm-d example deployment files |
| `LLM_D_MODELSERVICE_VALUES` | `$EXAMPLE_DIR/ms-$WELL_LIT_PATH_NAME/values.yaml` | Base values file for model service deployment |

### Multi-Model Testing Variables

| Variable | Default | Description |
|---|---|---|
| `MULTI_MODEL_TESTING` | `false` | Deploy second model/inference pool for tests |
| `MODEL_ID_2` | `unsloth/Llama-3.2-1B` | Secondary model identifier |

### Undeploy and Cleanup Variables

| Variable | Default | Description |
|---|---|---|
| `UNDEPLOY` | `false` | Undeploy mode switch (same effect as `--undeploy`) |
| `DELETE_NAMESPACES` | `false` | Delete namespaces after undeploy |
| `DELETE_CLUSTER` | `false` | Kind-specific cluster deletion during undeploy |

### Kind Emulator-Specific Variables

These variables are primarily consumed by `deploy/kind-emulator/install.sh`.

| Variable | Default | Description |
|---|---|---|
| `CREATE_CLUSTER` | `false` | Create/recreate kind cluster before deployment |
| `CLUSTER_NAME` | `kind-wva-gpu-cluster` | kind cluster name |
| `CLUSTER_NODES` | `3` | Number of kind nodes |
| `CLUSTER_GPUS` | `4` | Emulated GPUs per cluster setup |
| `CLUSTER_GPU_TYPE` | `mix` | GPU vendor profile (`nvidia`, `amd`, `intel`, `mix`) |
| `KIND_IMAGE_PLATFORM` | auto-detected | Pull platform for image load (`linux/amd64` or `linux/arm64`) |
| `LLM_D_INFERENCE_SIM_IMG_REPO` | `ghcr.io/llm-d/llm-d-inference-sim` | Simulator image repository |
| `LLM_D_INFERENCE_SIM_IMG_TAG` | `latest` | Simulator image tag |
| `DEPLOY_LLM_D_INFERENCE_SIM` | `true` in kind flow | Enables inference simulator deployment |
| `WVA_RECONCILE_INTERVAL` | `60s` in kind flow | Controller reconcile interval override |

### Additional Script Inputs

| Variable | Default | Description |
|---|---|---|
| `CLUSTER_TYPE` | unset | If set to `kind`, script remaps environment to `kind-emulator` |

## Platform Overrides

Environment scripts can override values after base defaults are set.

### Kubernetes (`deploy/kubernetes/install.sh`)

- `PROMETHEUS_SVC_NAME=kube-prometheus-stack-prometheus`
- `PROMETHEUS_PORT=9090`
- `DEPLOY_PROMETHEUS=true`
- `SKIP_TLS_VERIFY=true` (dev-friendly default)
- Uses `values-dev.yaml` when TLS verify is skipped

### OpenShift (`deploy/openshift/install.sh`)

- `PROMETHEUS_SVC_NAME=thanos-querier`
- `PROMETHEUS_PORT=9091`
- `MONITORING_NAMESPACE=openshift-user-workload-monitoring`
- `PROMETHEUS_SECRET_NS=openshift-monitoring`
- `DEPLOY_PROMETHEUS=false` (built-in monitoring)
- `INSTALL_GATEWAY_CTRLPLANE=false`
- `SKIP_TLS_VERIFY=false`

### Kind Emulator (`deploy/kind-emulator/install.sh`)

- Forces emulator-focused defaults (for example `SKIP_TLS_VERIFY=true`, debug logging)
- Sets `INSTALL_GATEWAY_CTRLPLANE=true`
- Supports create/delete cluster workflow via `CREATE_CLUSTER` and `DELETE_CLUSTER`

## Examples

### Full deploy on Kubernetes

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-k8s
```

### Full deploy on OpenShift

```bash
export HF_TOKEN="hf_xxxxx"
oc login ...
make deploy-wva-on-openshift
```

### Infrastructure-only deploy (no VA/HPA)

```bash
export HF_TOKEN="hf_xxxxx"
INFRA_ONLY=true ENVIRONMENT=kubernetes ./deploy/install.sh
```

### Kind emulator deploy with cluster creation

```bash
CREATE_CLUSTER=true ENVIRONMENT=kind-emulator ./deploy/install.sh
```

### Kind deploy with KEDA backend

```bash
SCALER_BACKEND=keda ENVIRONMENT=kind-emulator ./deploy/install.sh
```

### Undeploy and remove namespaces

```bash
DELETE_NAMESPACES=true ENVIRONMENT=kubernetes ./deploy/install.sh --undeploy
```

## Common Configuration

```bash
# Optional image override via Make-style variable
export IMG="ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest"

# Optional model selection
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"

# Optional scaling behavior
export HPA_STABILIZATION_SECONDS=240
```

To see all script options:

```bash
./deploy/install.sh --help
```

## Known Caveats

- The help text mentions `kind-emulated`, but the accepted value is `kind-emulator`.
- Undeploy logs may mention `--delete-namespaces`, but cleanup is controlled by environment variables (`DELETE_NAMESPACES=true`, and for kind optionally `DELETE_CLUSTER=true`).
- `SCALER_BACKEND=keda` is enforced for `kind-emulator` only.
- Some Make targets pass `NAMESPACE`, but `deploy/install.sh` uses `WVA_NS` for controller namespace configuration.
- Some Make/e2e wrappers pass variables such as `USE_SIMULATOR` and `SCALE_TO_ZERO_ENABLED`; these are not parsed directly by `deploy/install.sh`.

## Verify

```bash
kubectl get deployment -n workload-variant-autoscaler-system
kubectl get crd variantautoscalings.llmd.ai
kubectl get hpa -A
```

## Uninstall

```bash
# Kubernetes
make undeploy-wva-on-k8s

# OpenShift
make undeploy-wva-on-openshift

# Kind emulator
make undeploy-wva-emulated-on-kind
```

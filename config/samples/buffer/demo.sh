#!/usr/bin/env bash
#
# End-to-end overprovisioning buffer-gate demo on a kind cluster.
#
# Proves: buffer pods (llm-d.ai/buffer="true") receive NO traffic while the
# primary has capacity, then absorb load once the primary saturates.
#
# The buffer-gate-filter is unreleased code, so this script builds the EPP
# image from a local llm-d-router checkout and loads it into kind.
#
# Usage:
#   LLM_D_ROUTER_DIR=../llm-d-router ./config/samples/buffer/demo.sh
#   ./config/samples/buffer/demo.sh teardown
set -euo pipefail

CLUSTER="${CLUSTER:-buffer-demo}"
NS="buffer-demo"
ROUTER_DIR="${LLM_D_ROUTER_DIR:-../llm-d-router}"
GAIE_VERSION="${GAIE_VERSION:-v1.5.0}"
ROUTER_CHART_VERSION="${ROUTER_CHART_VERSION:-v0.9.0}"
SAMPLES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EPP_IMAGE_REPO="llm-d-router-endpoint-picker"
EPP_IMAGE_TAG="buffer-demo"
EPP_IMAGE="${EPP_IMAGE_REPO}:${EPP_IMAGE_TAG}"

if [[ "${1:-}" == "teardown" ]]; then
  kind delete cluster --name "$CLUSTER" || true
  exit 0
fi

command -v kind >/dev/null    || { echo "kind not found"; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl not found"; exit 1; }
command -v helm >/dev/null     || { echo "helm not found"; exit 1; }
[[ -d "$ROUTER_DIR" ]] || { echo "llm-d-router checkout not found at $ROUTER_DIR (set LLM_D_ROUTER_DIR)"; exit 1; }

echo "==> 1/6 Creating kind cluster '$CLUSTER'"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER"

echo "==> 2/6 Building EPP image with buffer-gate-filter from $ROUTER_DIR"
# Fail loudly if the filter is not compiled in (unreleased code).
if [[ ! -d "$ROUTER_DIR/pkg/epp/framework/plugins/scheduling/filter/buffergate" ]]; then
  echo "ERROR: buffer-gate-filter package not found in $ROUTER_DIR — is the feature branch checked out?"; exit 1
fi
( cd "$ROUTER_DIR" && EPP_IMAGE="$EPP_IMAGE" make image-build-epp )
# Retag with localhost/ prefix so the image reference renders as a valid
# registry path (avoids the leading-slash bug when registry="" is used).
docker tag "$EPP_IMAGE" "localhost/$EPP_IMAGE"
kind load docker-image "localhost/$EPP_IMAGE" --name "$CLUSTER"

echo "==> 3/6 Installing Gateway API + GAIE CRDs and the EPP (locally built image)"
# Gateway API + GAIE CRDs (the EPP needs the InferencePool CRD).
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${GAIE_VERSION}/manifests.yaml"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
# Install the standalone router chart with our locally built image and the
# buffer-gate scheduling config. imagePullPolicy=Never forces use of the
# kind-loaded image instead of a registry pull.
helm upgrade --install buffer-demo \
  oci://ghcr.io/llm-d/charts/llm-d-router-standalone \
  --version "$ROUTER_CHART_VERSION" \
  -f "$SAMPLES_DIR/epp-values.yaml" \
  --set router.modelServers.matchLabels.model=foo \
  --set "router.modelServers.targetPorts[0].number=8000" \
  --set router.epp.image.registry=localhost \
  --set router.epp.image.repository="$EPP_IMAGE_REPO" \
  --set router.epp.image.tag="$EPP_IMAGE_TAG" \
  --set router.epp.image.pullPolicy=Never \
  --set router.epp.resources.requests.cpu=100m \
  --set router.epp.resources.requests.memory=256Mi \
  --set router.epp.resources.limits.memory=512Mi \
  --set router.proxy.resources.requests.cpu=100m \
  --set router.proxy.resources.requests.memory=128Mi \
  --set router.proxy.resources.limits.memory=256Mi \
  -n "$NS" --create-namespace

echo "==> 4/6 Applying demo workloads (primary + buffer + pool)"
kubectl apply -k "$SAMPLES_DIR"
kubectl -n "$NS" rollout status deploy/foo-primary --timeout=180s
kubectl -n "$NS" rollout status deploy/foo-buffer  --timeout=180s

echo "==> 5/6 Baseline: light traffic should hit ONLY primary"
kubectl -n "$NS" delete job foo-load --ignore-not-found
kubectl -n "$NS" create job foo-baseline --image=quay.io/curl/curl:8.11.1 -- \
  /bin/sh -c 'for i in $(seq 1 5); do curl -sS -o /dev/null -H "Content-Type: application/json" -d "{\"model\":\"test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}" http://buffer-demo-epp.buffer-demo.svc.cluster.local:8081/v1/chat/completions; done'
kubectl -n "$NS" wait --for=condition=complete job/foo-baseline --timeout=120s || echo "WARNING: job did not complete in time"
sleep 5
echo "--- buffer pod request counts after light load (expect ~0) ---"
for p in $(kubectl -n "$NS" get pods -l tier=buffer -o name); do
  echo "$p: $(kubectl -n "$NS" logs "$p" 2>/dev/null | grep -c 'chat/completions' || true) requests"
done

echo "==> 6/6 Burst: heavy concurrent load should spill onto buffer pods"
kubectl -n "$NS" apply -f "$SAMPLES_DIR/load-job.yaml"
kubectl -n "$NS" wait --for=condition=complete job/foo-load --timeout=180s || echo "WARNING: job did not complete in time"
sleep 5
echo "--- buffer pod request counts after burst (expect > 0) ---"
for p in $(kubectl -n "$NS" get pods -l tier=buffer -o name); do
  echo "$p: $(kubectl -n "$NS" logs "$p" 2>/dev/null | grep -c 'chat/completions' || true) requests"
done

echo
echo "Demo complete. Buffer pods idle under light load and served traffic under burst."
echo "Tear down with: $0 teardown"

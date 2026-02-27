# Installation Guide

This is the canonical installation entry point for Workload Variant Autoscaler (WVA).

## Prerequisites

- Kubernetes v1.32.0 or later
- Helm 3.x
- `kubectl` configured to access your cluster
- Cluster permissions to install CRDs and controller resources

## Choose an Installation Method

Use one of the following methods based on your environment and goals:

| Method | When to Use | Guide |
|--------|-------------|-------|
| Helm chart | You already have llm-d/model-serving infrastructure and want to install WVA controller resources | [Install WVA with Helm](installation/helm.md) |
| Deployment script (`make` + `deploy/install.sh`) | You want repository-supported automation for Kubernetes, OpenShift, or Kind emulator workflows | [Install WVA with the Deployment Script](installation/scripted-deploy.md) |
| Platform-specific notes | You need Kubernetes/OpenShift/Kind-specific caveats and environment notes | [Platform Notes](installation/platform-notes.md) |

## Verify Installation

### Helm chart

Set the same variables you used during installation (defaults shown):

```bash
WVA_NS="${WVA_NS:-workload-variant-autoscaler-system}"
RELEASE_NAME="${RELEASE_NAME:-workload-variant-autoscaler}"
```

Then run:

```bash
kubectl get deployment -n "$WVA_NS"
kubectl get crd variantautoscalings.llmd.ai
kubectl logs -n "$WVA_NS" \
  deployment/"${RELEASE_NAME}-controller-manager"
```

### Deployment script

The script defaults to the `workload-variant-autoscaler-system` namespace and the `workload-variant-autoscaler` release name. If you overrode `WVA_NS` or `WVA_RELEASE_NAME`, substitute those values below:

```bash
kubectl get deployment -n workload-variant-autoscaler-system
kubectl get crd variantautoscalings.llmd.ai
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager
```

## Uninstall

The uninstall command depends on your installation method:

- Helm: see [Install WVA with Helm](installation/helm.md#uninstall)
- Scripted deployment: see [Install WVA with the Deployment Script](installation/scripted-deploy.md#uninstall)

## Next Steps

- [Configuration Guide](configuration.md)
- [CRD Reference](crd-reference.md)
- [Troubleshooting Guide](troubleshooting.md)
- [Multi-Controller Isolation](multi-controller-isolation.md)

# Platform Notes

This page captures platform-specific installation notes. Use it with:

- [Install WVA with Helm](helm.md)
- [Install WVA with the Deployment Script](scripted-deploy.md)

## Kubernetes

- Use `make deploy-wva-on-k8s` for scripted deployment.
- Ensure your kubeconfig points to the intended cluster context before running install or uninstall commands.

## OpenShift

- Use `make deploy-wva-on-openshift` for scripted deployment.
- Authenticate first with `oc login`.
- OpenShift deployments typically use user workload monitoring (Thanos) instead of installing a standalone Prometheus stack.

## Kind Emulator

- Use `make deploy-wva-emulated-on-kind` for local development and emulated GPU workflows.
- Use `make create-kind-cluster` and `make destroy-kind-cluster` for explicit cluster lifecycle management.
- Kind emulator scripts are in `deploy/kind-emulator/`.

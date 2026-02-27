# Kind Emulator Deployment Reference

Canonical installation instructions are in:

- [docs/user-guide/installation.md](../../docs/user-guide/installation.md)
- [docs/user-guide/installation/scripted-deploy.md](../../docs/user-guide/installation/scripted-deploy.md)
- [docs/user-guide/installation/platform-notes.md](../../docs/user-guide/installation/platform-notes.md)

For local emulator deployment, use:

```bash
make deploy-wva-emulated-on-kind
```

For explicit cluster lifecycle management:

```bash
make create-kind-cluster
make destroy-kind-cluster
```

This directory is retained for Kind helper scripts (`setup.sh`, `teardown.sh`) and emulator assets.

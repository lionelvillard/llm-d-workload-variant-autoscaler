# VA Deprecation — Phase 2 Design

**Date:** 2026-06-12
**Issue:** https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1234
**Scope:** Tasks 2.1, 2.2, 2.3, 2.4

---

## 2.1 — CRD Deprecation Marker

**File:** `api/v1alpha1/variantautoscaling_types.go`

Add the kubebuilder deprecation marker above the `VariantAutoscaling` struct:

```go
// +kubebuilder:deprecatedversion:warning="VariantAutoscaling is deprecated and will be removed in a future release. Use the annotation-based path (llm-d.ai/managed=true on HPA or ScaledObject) instead. See docs/developer-guide/migrating-from-va-crd.md."
```

Then run `make manifests` to regenerate `config/crd/bases/llmd.ai_variantautoscalings.yaml`. The CRD's `versions[0]` entry gains `deprecated: true` and `deprecationWarning: "..."`.

No chart copy exists (Helm chart is deprecated per 2.5 being N/A), so only the `config/crd/bases/` file needs updating.

---

## 2.2 — Reconciler Deprecation Warning

**File:** `internal/controller/variantautoscaling_controller.go`

After the deletion-timestamp check and before namespace tracking, add a "once per VA" deprecation notice:

1. Check if the annotation `llm-d.ai/deprecation-warned: "true"` is present on the VA.
2. If absent: emit `logger.Info("VariantAutoscaling is deprecated...", "name", va.Name, "namespace", va.Namespace)`, fire `r.Recorder.Event(&va, corev1.EventTypeWarning, "Deprecated", "...")`, then patch the annotation onto the VA (metadata-only patch) to avoid repeating on every reconcile.
3. If present: skip.

The annotation approach is preferred over an in-memory set because it survives controller restarts and scales across multiple replicas.

Annotation key: `llm-d.ai/deprecation-warned`
Event reason: `Deprecated`
Event message: `VariantAutoscaling is deprecated. Migrate to the annotation-based path (llm-d.ai/managed=true on HPA or ScaledObject). See docs/developer-guide/migrating-from-va-crd.md.`

---

## 2.3 — Migration Documentation

**New file:** `docs/developer-guide/migrating-from-va-crd.md`

Sections:
1. **Why migrate** — brief context, link to issue.
2. **Before/After — HPA path** — side-by-side YAML: `VariantAutoscaling` + plain HPA → HPA with `llm-d.ai/managed: "true"` annotation.
3. **Before/After — KEDA/ScaledObject path** — same pattern.
4. **Migration recipes** — `kubectl get va -A` to list existing VAs; `kubectl annotate hpa <name> llm-d.ai/managed=true llm-d.ai/model-id=<id>` recipe; optionally a `jq` snippet to extract modelID from VA and build the annotation commands.
5. **Validation** — check controller logs for the `wva_desired_replicas` metric being emitted, verify Events on the HPA/ScaledObject.

**Updated file:** `docs/integrations/prometheus.md`

Add a note near the top pointing to the annotation path as the primary approach, with a link to the migration guide.

---

## 2.4 — Sample Cleanup

Move legacy VA samples to `config/samples/legacy/`:

**New directory:** `config/samples/legacy/`

- Move `config/samples/hpa/va.yaml` → `config/samples/legacy/hpa-va.yaml`
- Move `config/samples/keda/va.yaml` → `config/samples/legacy/keda-va.yaml`
- Create `config/samples/legacy/kustomization.yaml` referencing both files, with a comment marking them deprecated.

**Updated kustomizations:**
- `config/samples/hpa/kustomization.yaml`: remove `va.yaml` from resources; keep `hpa.yaml`; update comment to say the annotation overlay is preferred.
- `config/samples/keda/kustomization.yaml`: remove `va.yaml` from resources; keep `scaledobject.yaml`; same comment update.

The annotation overlays (`config/samples/hpa/annotations/`, `config/samples/keda/annotations/`) remain unchanged.

---

## Out of Scope

- **2.5 (Chart Default Flip):** N/A — Helm chart is deprecated.
- **2.6 (Release Notes):** Done manually.

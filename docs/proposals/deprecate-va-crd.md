# Proposal: Remove the VariantAutoscaling CRD

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-05-05
**Last Updated:** 2026-05-06

---

## Problem Statement

The Workload Variant Autoscaler (WVA) introduces a custom VariantAutoscaling CRD that operators must create and manage for each model variant. This CRD duplicates information already present on the scaling target (Deployment, ScaledObject, HPA) and adds operational burden:

- A custom CRD that operators must learn, create, and keep in sync with their deployments
- Status management and reconciliation logic tied to the CRD lifecycle
- Tight coupling between WVA's internal optimization logic and a user-facing API surface
- Additional RBAC, validation webhooks, and CRD versioning concerns

Beyond operational burden, the CRD creates an architectural bottleneck: every team that wants to scale a model variant must go through WVA's full pipeline, even when simpler approaches (a raw Prometheus trigger, a custom algorithm, a Kustomize-managed ScaledObject) would suffice. The CRD makes WVA the only possible integration point.

The core value of WVA — computing `wva_desired_replicas` from vLLM/EPP metrics — does not require a dedicated CRD. WVA can discover what to scale from annotations on existing Kubernetes objects (KEDA ScaledObjects or HPAs), and teams that don't need WVA's advanced features can use those objects directly without involving WVA at all.

### Current Flow (unchanged)

```
vLLM/EPP metrics → Prometheus → WVA controller → wva_desired_replicas → KEDA/HPA → scale
```

This flow remains the same. The only change is how WVA discovers which deployments to manage: annotations replace the CRD.

### Enabled by This Change

Decoupling discovery from the CRD makes the ScaledObject/HPA the stable integration point, not the VA CRD. Any metric producer can drive scaling by writing a compatible Prometheus metric:

```
Simple:   Prometheus recording rules ─────────────────────────→ KEDA trigger → scale
Advanced: vLLM/EPP → Prometheus → WVA → wva_desired_replicas → KEDA trigger → scale
Custom:   any metric producer ───────────────────────────────→ KEDA trigger → scale
```

---

## Design Philosophy: Pluggable, Modular Scaling

By removing the CRD, WVA shifts from being a mandatory control plane component to being one of several possible scaling engines. This lowers the entry barrier, fosters experimentation, and lets teams adopt exactly as much complexity as their scenario requires.

### Progressive Complexity

Operators choose the level of sophistication they need:

| Scenario | What to deploy | WVA needed? |
|----------|----------------|-------------|
| Single model, load-based scaling | ScaledObject with Prometheus trigger | No |
| Single model, saturation-aware | ScaledObject + WVA annotations | Yes |
| Multi-variant, cost-aware | ScaledObject + WVA annotations + saturation config | Yes |
| SLO-based with GPU-limited fair-share | ScaledObject + WVA annotations + full config | Yes |
| Custom algorithm | Any service writing to Prometheus | No |

A team getting started does not need to understand WVA's analyzers, the saturation ConfigMap, or multi-variant optimization. They write a KEDA Prometheus trigger and scale. When requirements grow, they annotate the ScaledObject and WVA takes over those dimensions.

### WVA as a Pluggable Engine

WVA is now a named, opt-in engine for advanced scenarios — not the only path. This opens the door for:

- **Simpler direct-metric triggers**: raw saturation metrics via Prometheus recording rules, no WVA required
- **Custom engines**: any service that implements a scaling algorithm and writes the result as a Prometheus metric integrates identically
- **Gradual adoption**: start with a raw KEDA trigger, add WVA management when multi-variant optimization becomes necessary

### Granular Feature Opt-In

Features within WVA remain modular and annotation-driven. Omitting an annotation means the feature is skipped or defaults are used:

| Feature | Annotation required |
|---------|-------------------|
| WVA management | `llm-d.ai/managed: "true"` |
| Multi-variant grouping | `llm-d.ai/model-id` |
| Cost-aware optimization | `llm-d.ai/variant-cost` |
| GPU-limited fair-share | `llm-d.ai/gpus-per-replica` |
| Scale-to-zero | `llm-d.ai/scale-to-zero-retention` |
| P/D disaggregation role | `llm-d.ai/role` |

This granularity means a single-model deployment that only needs saturation-based scaling pays no configuration cost for features it doesn't use.

---

## Goals

1. Remove the VariantAutoscaling CRD as the user-facing API
2. Remove the VariantAutoscaling CRD reconciler from the WVA controller
3. WVA discovers work by watching annotated ScaledObjects (or HPAs)
4. Variant metadata (cost, model ID, GPU count) moves to annotations on the ScaledObject/HPA
5. Reduce operational surface: fewer objects to create, no CRD versioning
6. KEDA/HPA continues consuming `wva_desired_replicas` exactly as today
7. Enable pluggable scaling: teams can use WVA, plain Prometheus triggers, or custom engines — all through the same ScaledObject/HPA interface

## Non-Goals

- Changing WVA's internal analyzers or optimization logic
- Removing the WVA controller process (it still runs, collects metrics, and exposes `wva_desired_replicas`)
- Changing how KEDA/HPA consume `wva_desired_replicas`
- Replacing vLLM or EPP metrics

---

## Proposed Solution

### Discovery via Annotations

WVA watches ScaledObjects (or HPAs) with the `llm-d.ai/managed: "true"` annotation. A ScaledObject without this annotation is invisible to WVA and can use any trigger it wants. Adding the annotation opts into WVA management; additional annotations enable specific features. Configuration currently in the VA CRD moves to annotations on the ScaledObject:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: granite-13b-scaler
  namespace: production
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-13b"
    llm-d.ai/variant-cost: "40.0"
spec:
  scaleTargetRef:
    kind: Deployment
    name: granite-13b
  triggers:
  - type: prometheus
    name: wva-desired-replicas
    metadata:
      query: wva_desired_replicas{variant_name="granite-13b",exported_namespace="production"}
      threshold: '1'
      activationThreshold: '0'
      metricType: "AverageValue"
```

### Annotation Schema

Annotations are placed on the ScaledObject (or HPA if not using KEDA):

| Annotation | Required | Default | Description |
|-----------|----------|---------|-------------|
| `llm-d.ai/managed` | Yes | — | Opt-in for WVA management |
| `llm-d.ai/model-id` | Yes | — | Model identifier (used for metric queries and multi-variant grouping) |
| `llm-d.ai/variant-cost` | No | `"1.0"` | Cost per replica (for cost-aware optimization) |

### WVA Behavior (Unchanged Internally)

WVA's internal pipeline remains the same:
1. **Discovery**: Watch annotated ScaledObjects/HPAs instead of VA CRDs
2. **Collection**: Query Prometheus for vLLM/EPP metrics (same queries, same collector)
3. **Analysis**: Run saturation engine (V1, V2, queueing model — unchanged)
4. **Optimization**: Cost-aware, GPU-limited fair-share (unchanged)
5. **Expose**: Expose `wva_desired_replicas{variant_name="granite-13b", exported_namespace="production"}` (unchanged)
6. **Actuation**: None — KEDA/HPA reads the metric and scales (unchanged)

The ScaledObject now serves dual duty: it carries WVA annotations (discovery + configuration) and defines the KEDA trigger (unchanged). WVA reads the annotations; KEDA reads `spec.triggers`. No additional objects needed.

### Saturation Configuration

The saturation scaling ConfigMap (`saturation-scaling-config`) continues to work as-is. Per-model overrides use the same `{modelID}#{namespace}` key format. WVA resolves the model ID from the `llm-d.ai/model-id` annotation on the ScaledObject/HPA instead of from the VA CRD's `spec.modelID` field.

---

## Migration Path

### What Changes for Users

| Before (with CRD) | After (annotations) |
|-------------------|-------------------|
| Create Deployment + VariantAutoscaling + ScaledObject | Create Deployment + ScaledObject (with annotations) |
| 3 objects per variant | 2 objects per variant |
| `spec.modelID` on VA | `llm-d.ai/model-id` annotation on ScaledObject |
| `spec.variantCost` on VA | `llm-d.ai/variant-cost` annotation on ScaledObject |
| `spec.minReplicas` / `spec.maxReplicas` on VA | `spec.minReplicas` / `spec.maxReplicas` on ScaledObject |
| `spec.scaleTargetRef` on VA | Not needed (WVA resolves the target from ScaledObject's `spec.scaleTargetRef`) |

### Migration Tool

A `va-to-annotations` CLI tool converts existing VA resources to annotations:

```bash
# Reads VA CRDs, adds annotations to their target ScaledObjects
va-to-annotations migrate --namespace production --dry-run
va-to-annotations migrate --namespace production --apply
```

---

## Implementation Phases

### Phase 1: Dual-Mode (Non-Breaking)

**Goal:** WVA supports both VA CRDs and annotated ScaledObjects/HPAs simultaneously.

**Deliverables:**
- Add annotation-based discovery (watching ScaledObjects/HPAs) alongside existing CRD reconciler
- WVA exposes `wva_desired_replicas` for both VA-discovered and annotation-discovered variants
- Validation: annotated ScaledObject produces same metric output as equivalent VA CRD
- Documentation for annotation-based setup

**Success Criteria:** Users can run either mode; existing VA CRD deployments continue working unchanged.

### Phase 2: Deprecation

**Goal:** Mark VA CRD deprecated, guide users to annotations.

**Deliverables:**
- VA CRD marked deprecated (print warning on reconciliation)
- `va-to-annotations` migration tool
- Updated samples in `config/samples/` using annotation-based approach
- Deprecation notice in release notes

**Success Criteria:** All sample configurations and documentation use annotations. CI tests pass without VA CRDs.

### Phase 3: CRD Removal

**Deliverables:**
- Remove VA CRD from `config/crd/`
- Remove CRD reconciler code (`internal/controller/variantautoscaling_controller.go`)
- Remove API types (`api/v1alpha1/`)
- Remove RBAC for VA resources
- Retain all analyzer, optimizer, and collector code (unchanged)

**Success Criteria:** WVA binary no longer registers the CRD. All functionality works via annotations.

---

## What Does NOT Change

- WVA controller still runs as a Deployment
- WVA still queries Prometheus for vLLM/EPP metrics
- WVA still runs saturation analysis (V1, V2, queueing model)
- WVA still runs cost-aware optimization and GPU-limited fair-share
- WVA still exposes `wva_desired_replicas`, `wva_current_replicas`, `wva_desired_ratio`
- KEDA/HPA still consumes `wva_desired_replicas` to perform actual scaling
- Saturation scaling ConfigMap still works with same per-model override format
- Scale-to-zero and scale-from-zero logic still works

---

## Alternatives Considered

1. **Keep the CRD but make it optional** — Adds complexity (two discovery paths permanently) without ever removing the CRD burden. The dual-mode in Phase 1 is temporary.

2. **Move everything to a ConfigMap** — ConfigMaps don't have per-object identity or namespace locality. Annotations on the ScaledObject/HPA keep configuration co-located with the scaling object.

3. **Use labels instead of annotations** — Labels have character restrictions and are indexed (performance cost). Annotations are the Kubernetes convention for non-identifying metadata.

4. **Annotate Deployments instead of ScaledObjects** — Deployments don't know about scaling policy; the ScaledObject/HPA is the natural place for scaling-related metadata since WVA's output feeds into it directly.

---

## Open Questions

1. Should WVA watch only KEDA ScaledObjects, or also plain HPAs? Proposal: support both — watch any object with the `llm-d.ai/managed` annotation that has a `scaleTargetRef`.
1. For the `variant_name` label in `wva_desired_replicas`, should it use the scale target name or a separate `llm-d.ai/variant-name` annotation? Proposal: default to scale target name (from `scaleTargetRef`), allow override via annotation.

---

## Key Files

- `api/v1alpha1/variantautoscaling_types.go` — CRD types to remove (Phase 3)
- `internal/controller/variantautoscaling_controller.go` — CRD reconciler to replace with ScaledObject/HPA annotation watcher
- `internal/controller/configmap_bootstrap.go` — ConfigMap discovery (unchanged)
- `internal/engines/saturation/engine.go` — Saturation engine (unchanged)
- `internal/engines/pipeline/cost_aware_optimizer.go` — Cost optimizer (unchanged)
- `config/crd/` — CRD manifests to remove (Phase 3)
- `config/samples/keda/` — Update to annotation-based examples

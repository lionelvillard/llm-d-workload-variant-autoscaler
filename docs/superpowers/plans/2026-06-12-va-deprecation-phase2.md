# VA Deprecation Phase 2 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mark the `VariantAutoscaling` CRD as deprecated at all layers — API marker, controller warning event, migration docs, and sample cleanup — to guide users to the annotation-based path.

**Architecture:** Four independent tasks: (1) add kubebuilder deprecation marker + regenerate CRD manifest, (2) emit a once-per-VA `Warning` event + log in the reconciler, (3) write a migration guide in `docs/developer-guide/` and update integration docs, (4) move legacy VA samples to `config/samples/legacy/`.

**Tech Stack:** Go 1.23, controller-runtime, kubebuilder markers, Ginkgo/Gomega tests, kustomize sample manifests.

---

### Task 1: CRD Deprecation Marker (2.1)

**Files:**
- Modify: `api/v1alpha1/variantautoscaling_types.go` (above the `VariantAutoscaling` struct, ~line 103)
- Regenerated: `config/crd/bases/llmd.ai_variantautoscalings.yaml` (via `make manifests`)
- Regenerated: `charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml` (copied by `make manifests`)

- [ ] **Step 1: Add the kubebuilder deprecation marker**

In `api/v1alpha1/variantautoscaling_types.go`, add the marker directly above the `// +kubebuilder:object:root=true` comment block (before line 92):

```go
// +kubebuilder:deprecatedversion:warning="VariantAutoscaling is deprecated and will be removed in a future release. Migrate to the annotation-based path (add llm-d.ai/managed=true to your HPA or ScaledObject). See docs/developer-guide/migrating-from-va-crd.md for migration steps."
```

The block around it should look like:

```go
// +kubebuilder:deprecatedversion:warning="VariantAutoscaling is deprecated and will be removed in a future release. Migrate to the annotation-based path (add llm-d.ai/managed=true to your HPA or ScaledObject). See docs/developer-guide/migrating-from-va-crd.md for migration steps."
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=va
```

- [ ] **Step 2: Regenerate CRD manifests**

```bash
make manifests
```

Expected: exits 0. `config/crd/bases/llmd.ai_variantautoscalings.yaml` and `charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml` now contain under `spec.versions[0]`:

```yaml
deprecated: true
deprecationWarning: "VariantAutoscaling is deprecated..."
```

Verify:
```bash
grep -A2 "deprecated:" config/crd/bases/llmd.ai_variantautoscalings.yaml
```

Expected output includes:
```
    deprecated: true
    deprecationWarning: VariantAutoscaling is deprecated and will be removed...
```

- [ ] **Step 3: Run unit tests**

```bash
make test
```

Expected: all tests pass (0 failures).

- [ ] **Step 4: Commit**

```bash
git add api/v1alpha1/variantautoscaling_types.go \
        config/crd/bases/llmd.ai_variantautoscalings.yaml \
        charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml
git commit -m "feat(api): mark VariantAutoscaling CRD as deprecated

Add +kubebuilder:deprecatedversion:warning marker so the API server
prints a warning on every GET/LIST/WATCH of v1alpha1 VariantAutoscaling.

Closes part of #1234."
```

---

### Task 2: Reconciler Deprecation Warning (2.2)

**Files:**
- Modify: `internal/controller/variantautoscaling_controller.go` (around line 155, after deletion-timestamp check)
- Modify: `internal/controller/variantautoscaling_controller_test.go` (add test in the existing `"When reconciling a resource"` context)

- [ ] **Step 1: Write the failing test**

In `internal/controller/variantautoscaling_controller_test.go`, add a new `It` block inside the `"When reconciling a resource"` → `Context("When reconciling a resource", ...)` describe block, after the existing `"should successfully reconcile the resource"` test (around line 149):

```go
It("should emit a Deprecation warning event and annotate the VA on first reconcile", func() {
    By("Setting up reconciler with a fake recorder")
    fakeRecorder := record.NewFakeRecorder(10)
    controllerReconciler := &VariantAutoscalingReconciler{
        Client:    k8sClient,
        Scheme:    k8sClient.Scheme(),
        Recorder:  fakeRecorder,
        Datastore: datastore.NewDatastore(config.NewTestConfig()),
    }

    By("Reconciling the created resource (first time)")
    _, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
        NamespacedName: typeNamespacedName,
    })
    Expect(err).NotTo(HaveOccurred())

    By("Verifying the Deprecated warning event was emitted")
    select {
    case event := <-fakeRecorder.Events:
        Expect(event).To(ContainSubstring("Warning"))
        Expect(event).To(ContainSubstring("Deprecated"))
    case <-time.After(2 * time.Second):
        Fail("Expected Deprecated event but none received")
    }

    By("Verifying the deprecation-warned annotation was set on the VA")
    updated := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
    Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
    Expect(updated.Annotations).To(HaveKeyWithValue("llm-d.ai/deprecation-warned", "true"))

    By("Reconciling a second time — no new event should be emitted")
    _, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
        NamespacedName: typeNamespacedName,
    })
    Expect(err).NotTo(HaveOccurred())
    Consistently(fakeRecorder.Events, 1*time.Second).ShouldNot(Receive(ContainSubstring("Deprecated")))
})
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
make test 2>&1 | grep -E "FAIL|deprecation-warned|Deprecated"
```

Expected: test fails because the annotation is never set and no event is emitted.

- [ ] **Step 3: Implement the deprecation notice in the reconciler**

In `internal/controller/variantautoscaling_controller.go`, after the deletion-timestamp check block (around line 155, before `r.Datastore.NamespaceTrack`), add:

```go
// Emit a once-per-VA deprecation notice. The annotation persists across
// controller restarts, ensuring the warning fires exactly once per VA object.
if va.Annotations == nil || va.Annotations["llm-d.ai/deprecation-warned"] != "true" {
    logger.Info("VariantAutoscaling is deprecated; migrate to the annotation-based path",
        "name", va.Name,
        "namespace", va.Namespace,
        "migration", "docs/developer-guide/migrating-from-va-crd.md")
    r.Recorder.Event(&va, corev1.EventTypeWarning, "Deprecated",
        "VariantAutoscaling is deprecated and will be removed in a future release. "+
            "Migrate to the annotation-based path (add llm-d.ai/managed=true to your HPA or ScaledObject). "+
            "See docs/developer-guide/migrating-from-va-crd.md.")
    patch := client.MergeFrom(va.DeepCopy())
    if va.Annotations == nil {
        va.Annotations = map[string]string{}
    }
    va.Annotations["llm-d.ai/deprecation-warned"] = "true"
    if err := r.Patch(ctx, &va, patch); err != nil {
        logger.Error(err, "Failed to patch deprecation-warned annotation",
            "name", va.Name, "namespace", va.Namespace)
        return ctrl.Result{}, err
    }
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
make test 2>&1 | grep -E "FAIL|PASS|deprecation"
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/variantautoscaling_controller.go \
        internal/controller/variantautoscaling_controller_test.go
git commit -m "feat(controller): emit once-per-VA Deprecated warning event

On first reconcile of any VariantAutoscaling, the reconciler logs a
deprecation message, emits a Kubernetes Warning event with reason
'Deprecated', and patches the llm-d.ai/deprecation-warned annotation
onto the VA to prevent repeat notices on subsequent reconciles.

Closes part of #1234."
```

---

### Task 3: Migration Documentation (2.3)

**Files:**
- Create: `docs/developer-guide/migrating-from-va-crd.md`
- Modify: `docs/integrations/prometheus.md` (add a note near the top)

- [ ] **Step 1: Create the migration guide**

Create `docs/developer-guide/migrating-from-va-crd.md` with the following content:

````markdown
# Migrating from VariantAutoscaling CRD

The `VariantAutoscaling` (VA) CRD is deprecated and will be removed in a future release.
The preferred path is to add WVA discovery annotations directly to your existing HPA or KEDA `ScaledObject`.

## Why migrate

The annotation-based path removes the need for a separate CRD and aligns WVA with standard Kubernetes autoscaling primitives.

## Before / After — HPA path

**Before (deprecated):** two objects

```yaml
# VariantAutoscaling (deprecated)
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
  maxReplicas: 10
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: sample-deployment-hpa
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: sample-deployment
            exported_namespace: llm-d-sim
      target:
        type: AverageValue
        averageValue: "1"
```

**After (recommended):** one object

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: sample-deployment-hpa
  namespace: llm-d-sim
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "default/default"
    llm-d.ai/variant-cost: "10.0"   # optional, defaults to 10.0
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: sample-deployment
            exported_namespace: llm-d-sim
      target:
        type: AverageValue
        averageValue: "1"
```

## Before / After — KEDA ScaledObject path

**Before (deprecated):** two objects

```yaml
# VariantAutoscaling (deprecated)
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
  maxReplicas: 10
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: sample-deployment-scaler
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  maxReplicaCount: 10
  triggers:
  - type: prometheus
    name: wva-desired-replicas
    metadata:
      serverAddress: https://prometheus.example.com:9090
      query: |
        wva_desired_replicas{
          variant_name="sample-deployment",
          namespace="llm-d-sim"
        }
      threshold: '1'
      activationThreshold: '0'
      metricType: "Value"
```

**After (recommended):** one object

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: sample-deployment-scaler
  namespace: llm-d-sim
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "default/default"
    llm-d.ai/variant-cost: "10.0"   # optional, defaults to 10.0
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  maxReplicaCount: 10
  triggers:
  - type: prometheus
    name: wva-desired-replicas
    metadata:
      serverAddress: https://prometheus.example.com:9090
      query: |
        wva_desired_replicas{
          variant_name="sample-deployment",
          namespace="llm-d-sim"
        }
      threshold: '1'
      activationThreshold: '0'
      metricType: "Value"
```

## Migration steps

1. **List existing VariantAutoscalings:**

   ```bash
   kubectl get variantautoscalings -A
   ```

2. **For each VA, extract its `modelID` and scale-target name:**

   ```bash
   kubectl get variantautoscaling <va-name> -n <namespace> \
     -o jsonpath='{.spec.modelID} {.spec.scaleTargetRef.name}{"\n"}'
   ```

3. **Annotate your HPA (or ScaledObject) with the values from step 2:**

   ```bash
   # Replace <hpa-name>, <namespace>, <modelID> with actual values
   kubectl annotate hpa <hpa-name> -n <namespace> \
     llm-d.ai/managed=true \
     llm-d.ai/model-id=<modelID>
   ```

   For KEDA ScaledObject:

   ```bash
   kubectl annotate scaledobject <so-name> -n <namespace> \
     llm-d.ai/managed=true \
     llm-d.ai/model-id=<modelID>
   ```

4. **Verify WVA is picking up the annotated resource:**

   Check controller logs for a line containing `"Discovered annotated HPA"` or `"Discovered annotated ScaledObject"` with your resource name.

   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager \
     | grep -E "Discovered annotated|<hpa-or-so-name>"
   ```

   Alternatively, check that the `wva_desired_replicas` metric is emitted:

   ```bash
   kubectl exec -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager -- \
     wget -qO- http://localhost:9090/metrics | grep wva_desired_replicas
   ```

5. **Delete the legacy VariantAutoscaling once validated:**

   ```bash
   kubectl delete variantautoscaling <va-name> -n <namespace>
   ```

## Validation checklist

- [ ] Controller logs show the annotated HPA/ScaledObject being discovered.
- [ ] `wva_desired_replicas{variant_name="<name>", namespace="<ns>"}` metric is present and non-zero under load.
- [ ] HPA/ScaledObject is scaling as expected.
- [ ] No `Warning Deprecated` events on the deleted VA (it no longer exists).

## Sample manifests

Ready-to-use samples are in:
- `config/samples/hpa/annotations/` — annotation-based HPA
- `config/samples/keda/annotations/` — annotation-based ScaledObject

Legacy VA samples (for reference only) are archived in `config/samples/legacy/`.
````

- [ ] **Step 2: Add a deprecation note to the Prometheus integration doc**

In `docs/integrations/prometheus.md`, insert the following block immediately after the first `# Prometheus Integration` heading (before the `WVA integrates with Prometheus` line):

```markdown
> **Note:** The `VariantAutoscaling` CRD is deprecated. The recommended approach is to add
> `llm-d.ai/managed: "true"` and `llm-d.ai/model-id: "<id>"` annotations directly to your HPA or
> KEDA ScaledObject — no VA object needed. See
> [Migrating from VariantAutoscaling CRD](migrating-from-va-crd.md) for step-by-step instructions.

```

- [ ] **Step 3: Commit**

```bash
git add docs/developer-guide/migrating-from-va-crd.md \
        docs/integrations/prometheus.md
git commit -m "docs(developer-guide): add VA CRD migration guide

Creates docs/developer-guide/migrating-from-va-crd.md with before/after
YAML comparisons, kubectl recipes, and a validation checklist for users
moving from VariantAutoscaling objects to the annotation-based path.

Also adds a deprecation notice at the top of docs/integrations/prometheus.md.

Closes part of #1234."
```

---

### Task 4: Move Legacy VA Samples to `legacy/` (2.4)

**Files:**
- Create: `config/samples/legacy/kustomization.yaml`
- Create: `config/samples/legacy/hpa-va.yaml` (moved from `config/samples/hpa/va.yaml`)
- Create: `config/samples/legacy/keda-va.yaml` (moved from `config/samples/keda/va.yaml`)
- Delete: `config/samples/hpa/va.yaml`
- Delete: `config/samples/keda/va.yaml`
- Modify: `config/samples/hpa/kustomization.yaml`
- Modify: `config/samples/keda/kustomization.yaml`

- [ ] **Step 1: Create `config/samples/legacy/` with both VA files**

Create `config/samples/legacy/hpa-va.yaml`:

```yaml
# DEPRECATED: VariantAutoscaling CRD is deprecated.
# This sample is kept for reference only.
# Use config/samples/hpa/annotations/ for the recommended annotation-based approach.
# See docs/developer-guide/migrating-from-va-crd.md.
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
  labels:
    inference.optimization/acceleratorName: A100
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
```

Create `config/samples/legacy/keda-va.yaml`:

```yaml
# DEPRECATED: VariantAutoscaling CRD is deprecated.
# This sample is kept for reference only.
# Use config/samples/keda/annotations/ for the recommended annotation-based approach.
# See docs/developer-guide/migrating-from-va-crd.md.
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: sample-deployment
  namespace: llm-d-sim
  labels:
    inference.optimization/acceleratorName: A100
spec:
  scaleTargetRef:
    kind: Deployment
    name: sample-deployment
  modelID: default/default
```

Create `config/samples/legacy/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
metadata:
  name: legacy-va-samples
# DEPRECATED: VariantAutoscaling CRD is deprecated and will be removed.
# These samples are kept as a migration reference only.
# For the annotation-based approach (no CRD needed), use:
#   config/samples/hpa/annotations/
#   config/samples/keda/annotations/
resources:
- hpa-va.yaml
- keda-va.yaml
```

- [ ] **Step 2: Update `config/samples/hpa/kustomization.yaml`**

Replace its entire content with:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
metadata:
  name: hpa-sample
# Annotation-based WVA discovery (recommended): use the annotations/ overlay — no VariantAutoscaling CRD needed.
# Apply with: kubectl kustomize config/samples/hpa/annotations | kubectl apply -f -
# Legacy VariantAutoscaling samples are archived in config/samples/legacy/.
resources:
- hpa.yaml
```

- [ ] **Step 3: Update `config/samples/keda/kustomization.yaml`**

Replace its entire content with:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
metadata:
  name: keda-sample
# Annotation-based WVA discovery (recommended): use the annotations/ overlay — no VariantAutoscaling CRD needed.
# Apply with: kubectl kustomize config/samples/keda/annotations | kubectl apply -f -
# Legacy VariantAutoscaling samples are archived in config/samples/legacy/.
resources:
- scaledobject.yaml
```

- [ ] **Step 4: Delete the old VA sample files**

```bash
git rm config/samples/hpa/va.yaml config/samples/keda/va.yaml
```

- [ ] **Step 5: Verify kustomize builds cleanly**

```bash
kubectl kustomize config/samples/hpa/annotations
kubectl kustomize config/samples/keda/annotations
kubectl kustomize config/samples/legacy
```

Expected: each prints valid YAML with no errors.

- [ ] **Step 6: Commit**

```bash
git add config/samples/legacy/ \
        config/samples/hpa/kustomization.yaml \
        config/samples/keda/kustomization.yaml
git commit -m "chore(samples): move legacy VA samples to config/samples/legacy/

VariantAutoscaling YAML samples are archived under config/samples/legacy/
for migration reference. The hpa/ and keda/ kustomizations now point to
their annotation-based overlays as the recommended approach.

Closes part of #1234."
```

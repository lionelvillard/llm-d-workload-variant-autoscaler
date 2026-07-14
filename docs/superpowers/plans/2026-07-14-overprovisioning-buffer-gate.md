# Overprovisioning Buffer-Gate Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an EPP scheduling filter to llm-d-router that hides `llm-d.ai/buffer="true"` endpoints from routing until the primary sub-fleet is saturated, so operators can keep `N` warm standby pods that absorb bursts instantly.

**Architecture:** A new scheduling `Filter` plugin (`buffer-gate-filter`) partitions the candidate endpoints into primary (no buffer label) and buffer (`llm-d.ai/buffer="true"`) sub-fleets. It reuses the already-configured `SaturationDetector` — resolved by name through the plugin `Handle` — to score the *primary* subset, and admits buffer endpoints only when that score meets a configurable threshold (default `1.0`). No new interface, no duplicated saturation math.

**Tech Stack:** Go, the in-tree EPP framework at `github.com/llm-d/llm-d-router/pkg/epp/...` (GIE forked in-tree), testify.

## Global Constraints

- **Implementation repo:** `github.com/llm-d/llm-d-router` (the worktree directory is `llm-d-router`). All code Tasks 1–4 land there. Task 5 (docs + constant) lands in `llm-d-workload-variant-autoscaler`.
- **Go style:** gofmt; no license headers in new source files; exported names get doc comments starting with the name.
- **No new dependencies.** Use only what the `bylabel` filter and the saturation detectors already import.
- **Buffer label:** key `llm-d.ai/buffer`, value `"true"`. Presence = buffer; absence = primary. Do NOT reuse `llm-d.ai/variant` (WVA owns it for a different purpose).
- **Saturation semantics come from the configured detector.** The filter only thresholds the existing `Saturation() float64` gradient; it never redefines "saturated".
- **Unit test command (fast, per-package):** `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -v` run from the router repo root. (The `make test-unit` target builds a builder image and is too heavy for per-task iteration.)

---

## File Structure

New package in llm-d-router, mirroring the sibling `bylabel` filter:

- `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go` — the `BufferGate` filter: type, constants, constructor, `TypedName`/`WithName`, `Filter`, factory, and the primary/buffer partition + datalayer bridge helpers. One responsibility: gate the buffer sub-fleet on primary saturation.
- `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go` — table tests for the factory and the filter behavior, using a fake `SaturationDetector`.
- `pkg/epp/framework/plugins/scheduling/filter/buffergate/doc.go` — package doc.
- `cmd/epp/runner/runner.go` — MODIFY: one `fwkplugin.Register(...)` line next to the other scheduling-filter registrations.

In llm-d-workload-variant-autoscaler:

- `internal/constants/labels.go` — MODIFY: add `BufferLabelKey` constant.
- `docs/developer-guide/overprovisioning-buffer.md` — CREATE: operator-facing how-to.
- `config/samples/buffer/` — CREATE: demo manifests (namespace, primary + buffer Deployments, Service, InferencePool, load generator Job) + `kustomization.yaml` + `epp-values.yaml` (Helm overlay carrying the buffer-gate EPP config).
- `config/samples/buffer/README.md` — CREATE: demo run instructions.
- `config/samples/buffer/demo.sh` — CREATE: end-to-end kind demo script.

---

## Reference: exact types this plan depends on

Copy these signatures; they are verified against llm-d-router `main` (commit `b5f81591`).

```go
// pkg/epp/framework/interface/scheduling/plugins.go
type Filter interface {
    plugin.Plugin // TypedName() plugin.TypedName
    Filter(ctx context.Context, request *InferenceRequest, pods []Endpoint) []Endpoint
}

// pkg/epp/framework/interface/scheduling/types.go
type Endpoint interface {
    GetMetadata() *datalayer.EndpointMetadata // .Labels map[string]string
    GetMetrics()  *datalayer.Metrics
    // ...Get/Put/Keys/Clone/String
}
func NewEndpoint(meta *datalayer.EndpointMetadata, metrics *datalayer.Metrics, attr datalayer.AttributeMap) Endpoint

// pkg/epp/framework/interface/flowcontrol/plugins.go
type SaturationDetector interface {
    plugin.Plugin
    Saturation(ctx context.Context, endpoints []datalayer.Endpoint) float64 // >= 1.0 == saturated
}

// pkg/epp/framework/interface/datalayer/endpoint.go
func NewEndpoint(meta *EndpointMetadata, metrics *Metrics) *ModelServer // *ModelServer satisfies datalayer.Endpoint

// pkg/epp/framework/interface/plugin
type FactoryFunc func(name string, parameters *json.Decoder, handle Handle) (Plugin, error)
type TypedName struct { Type string; Name string }
func Register(pluginType string, factory FactoryFunc)
func StrictDecoder(raw json.RawMessage) *json.Decoder // for tests
func NewEppHandle(ctx context.Context, podList PodListFunc, opts ...HandleOption) Handle
//   handle.Plugin(name) Plugin ; handle.AddPlugin(name, plugin) ; handle.Context() context.Context

// pkg/epp/framework/interface/flowcontrol/mocks/mocks.go
type MockSaturationDetector struct { TypedNameV plugin.TypedName; SaturationV float64; /* ... */ }
```

**The Endpoint-type bridge (important):** the filter receives `[]scheduling.Endpoint`, but `Saturation` takes `[]datalayer.Endpoint` — these are *different* interfaces. Bridge each endpoint with `datalayer.NewEndpoint(ep.GetMetadata(), ep.GetMetrics())`, which returns a `*ModelServer` (a `datalayer.Endpoint`). `Saturation` only reads metadata/metrics, so this faithful shallow rewrap is sufficient.

---

## Task 1: Package skeleton, constants, and the label partition helper

**Files:**
- Create: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go`
- Create: `pkg/epp/framework/plugins/scheduling/filter/buffergate/doc.go`
- Test: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go`

**Interfaces:**
- Consumes: `scheduling.Endpoint` (`GetMetadata().Labels`), from the framework.
- Produces: `const BufferGateType = "buffer-gate-filter"`, `const BufferLabel = "llm-d.ai/buffer"`, `const BufferValue = "true"`; `func partition(endpoints []scheduling.Endpoint) (primary, buffer []scheduling.Endpoint)`.

- [ ] **Step 1: Write the failing test for `partition`**

Create `filter_test.go`:

```go
package buffergate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	k8stypes "k8s.io/apimachinery/pkg/types"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

func newEndpoint(name string, labels map[string]string) scheduling.Endpoint {
	return scheduling.NewEndpoint(
		&fwkdl.EndpointMetadata{
			NamespacedName: k8stypes.NamespacedName{Namespace: "default", Name: name},
			Labels:         labels,
		},
		&fwkdl.Metrics{},
		nil,
	)
}

func names(eps []scheduling.Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.GetMetadata().NamespacedName.Name)
	}
	return out
}

func TestPartition(t *testing.T) {
	eps := []scheduling.Endpoint{
		newEndpoint("p1", map[string]string{"model": "foo"}),
		newEndpoint("b1", map[string]string{"model": "foo", BufferLabel: BufferValue}),
		newEndpoint("p2", nil),
		newEndpoint("b2", map[string]string{BufferLabel: "true"}),
		newEndpoint("weird", map[string]string{BufferLabel: "false"}), // not "true" => primary
	}

	primary, buffer := partition(eps)

	assert.ElementsMatch(t, []string{"p1", "p2", "weird"}, names(primary))
	assert.ElementsMatch(t, []string{"b1", "b2"}, names(buffer))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run TestPartition -v`
Expected: FAIL — compile error, `partition`/`BufferLabel`/`BufferValue` undefined.

- [ ] **Step 3: Write `doc.go` and the minimal `filter.go`**

Create `doc.go`:

```go
// Package buffergate provides a scheduling filter that hides warm "buffer"
// endpoints from routing until the primary sub-fleet is saturated.
package buffergate
```

Create `filter.go`:

```go
package buffergate

import (
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

const (
	// BufferGateType is the plugin type string used in EndpointPickerConfig.
	BufferGateType = "buffer-gate-filter"

	// BufferLabel marks buffer endpoints. Primary endpoints omit it.
	BufferLabel = "llm-d.ai/buffer"

	// BufferValue is the value of BufferLabel on buffer endpoints.
	BufferValue = "true"
)

// partition splits endpoints into the primary sub-fleet (no buffer label, or a
// buffer label whose value is not BufferValue) and the buffer sub-fleet
// (BufferLabel == BufferValue).
func partition(endpoints []scheduling.Endpoint) (primary, buffer []scheduling.Endpoint) {
	for _, ep := range endpoints {
		if ep.GetMetadata().Labels[BufferLabel] == BufferValue {
			buffer = append(buffer, ep)
		} else {
			primary = append(primary, ep)
		}
	}
	return primary, buffer
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run TestPartition -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/epp/framework/plugins/scheduling/filter/buffergate/
git commit -m "feat(buffergate): package skeleton and label partition helper"
```

---

## Task 2: The datalayer bridge helper

**Files:**
- Modify: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go`
- Test: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go`

**Interfaces:**
- Consumes: `scheduling.Endpoint`, `datalayer.NewEndpoint`.
- Produces: `func toDatalayer(endpoints []scheduling.Endpoint) []datalayer.Endpoint`.

- [ ] **Step 1: Write the failing test**

Append to `filter_test.go`:

```go
func TestToDatalayer(t *testing.T) {
	eps := []scheduling.Endpoint{
		newEndpoint("p1", map[string]string{"model": "foo"}),
		newEndpoint("p2", nil),
	}

	dl := toDatalayer(eps)

	assert.Len(t, dl, 2)
	// The bridge must preserve identifying metadata so the detector scores the
	// right pods.
	assert.Equal(t, "p1", dl[0].GetMetadata().NamespacedName.Name)
	assert.Equal(t, "p2", dl[1].GetMetadata().NamespacedName.Name)
	assert.NotNil(t, dl[0].GetMetrics())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run TestToDatalayer -v`
Expected: FAIL — `toDatalayer` undefined.

- [ ] **Step 3: Add `toDatalayer` to `filter.go`**

Add the import and function:

```go
import (
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

// toDatalayer rewraps scheduling endpoints as datalayer endpoints so they can
// be passed to a SaturationDetector, whose Saturation method reads only
// metadata and metrics. It is a shallow rewrap, not a metrics copy.
func toDatalayer(endpoints []scheduling.Endpoint) []fwkdl.Endpoint {
	out := make([]fwkdl.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		out = append(out, fwkdl.NewEndpoint(ep.GetMetadata(), ep.GetMetrics()))
	}
	return out
}
```

(Adjust the existing single-import block into the grouped form shown.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run TestToDatalayer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go
git commit -m "feat(buffergate): scheduling-to-datalayer endpoint bridge"
```

---

## Task 3: The `BufferGate` filter type and `Filter` method

**Files:**
- Modify: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go`
- Test: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go`

**Interfaces:**
- Consumes: `flowcontrol.SaturationDetector`, `plugin.TypedName`, `scheduling.Filter`, `scheduling.InferenceRequest`.
- Produces: `type BufferGate struct{...}`; `func NewBufferGate(name string, detector flowcontrol.SaturationDetector, threshold float64) *BufferGate`; `func (f *BufferGate) TypedName() plugin.TypedName`; `func (f *BufferGate) WithName(name string) *BufferGate`; `func (f *BufferGate) Filter(ctx, *scheduling.InferenceRequest, []scheduling.Endpoint) []scheduling.Endpoint`.

- [ ] **Step 1: Write the failing tests for `Filter`**

Append to `filter_test.go`:

```go
import (
	"context"
	// ...existing imports...
	fcmocks "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/flowcontrol/mocks"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
)

func fakeDetector(saturation float64) *fcmocks.MockSaturationDetector {
	return &fcmocks.MockSaturationDetector{
		TypedNameV:  fwkplugin.TypedName{Type: "fake", Name: "fake"},
		SaturationV: saturation,
	}
}

func TestFilter(t *testing.T) {
	p1 := newEndpoint("p1", map[string]string{"model": "foo"})
	b1 := newEndpoint("b1", map[string]string{"model": "foo", BufferLabel: BufferValue})

	tests := []struct {
		name       string
		saturation float64
		threshold  float64
		endpoints  []scheduling.Endpoint
		want       []string
	}{
		{
			name:       "primary saturated admits buffer",
			saturation: 1.0,
			threshold:  1.0,
			endpoints:  []scheduling.Endpoint{p1, b1},
			want:       []string{"p1", "b1"},
		},
		{
			name:       "primary not saturated drops buffer",
			saturation: 0.5,
			threshold:  1.0,
			endpoints:  []scheduling.Endpoint{p1, b1},
			want:       []string{"p1"},
		},
		{
			name:       "no buffer endpoints is a no-op",
			saturation: 0.0, // detector must not gate away primary
			threshold:  1.0,
			endpoints:  []scheduling.Endpoint{p1},
			want:       []string{"p1"},
		},
		{
			name:       "empty primary admits buffer (cold start)",
			saturation: 1.0, // detector returns >=1.0 for empty candidate set
			threshold:  1.0,
			endpoints:  []scheduling.Endpoint{b1},
			want:       []string{"b1"},
		},
		{
			name:       "threshold below 1.0 admits buffer early",
			saturation: 0.9,
			threshold:  0.85,
			endpoints:  []scheduling.Endpoint{p1, b1},
			want:       []string{"p1", "b1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewBufferGate("gate", fakeDetector(tt.saturation), tt.threshold)
			got := f.Filter(context.Background(), nil, tt.endpoints)
			assert.ElementsMatch(t, tt.want, names(got))
		})
	}
}

func TestTypedNameAndWithName(t *testing.T) {
	f := NewBufferGate("gate", fakeDetector(0), 1.0)
	assert.Equal(t, BufferGateType, f.TypedName().Type)
	assert.Equal(t, "gate", f.TypedName().Name)
	assert.Equal(t, "renamed", f.WithName("renamed").TypedName().Name)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run 'TestFilter|TestTypedName' -v`
Expected: FAIL — `NewBufferGate`, `BufferGate` undefined.

- [ ] **Step 3: Add the type and methods to `filter.go`**

Add imports (`context`, `flowcontrol`, `plugin`) and:

```go
// DefaultSaturationThreshold is the primary-saturation level at or above which
// buffer endpoints are admitted. 1.0 means "primary fully saturated".
const DefaultSaturationThreshold = 1.0

// BufferGate admits endpoints labeled llm-d.ai/buffer="true" only when the
// primary sub-fleet's saturation is at or above saturationThreshold. Endpoints
// without the buffer label always pass.
type BufferGate struct {
	typedName           plugin.TypedName
	detector            flowcontrol.SaturationDetector
	saturationThreshold float64
}

var _ scheduling.Filter = &BufferGate{}

// NewBufferGate creates a BufferGate that gates the buffer sub-fleet on the
// saturation reported by detector over the primary sub-fleet.
func NewBufferGate(name string, detector flowcontrol.SaturationDetector, threshold float64) *BufferGate {
	return &BufferGate{
		typedName:           plugin.TypedName{Type: BufferGateType, Name: name},
		detector:            detector,
		saturationThreshold: threshold,
	}
}

// TypedName returns the typed name of the plugin.
func (f *BufferGate) TypedName() plugin.TypedName {
	return f.typedName
}

// WithName sets the name of the plugin instance.
func (f *BufferGate) WithName(name string) *BufferGate {
	f.typedName.Name = name
	return f
}

// Filter drops buffer endpoints unless the primary sub-fleet is saturated. When
// there are no buffer endpoints it returns the primary set unchanged. An empty
// or fully saturated primary set yields a saturation >= threshold, so the
// buffer is admitted (this covers scale-from-zero cold start).
func (f *BufferGate) Filter(ctx context.Context, _ *scheduling.InferenceRequest,
	endpoints []scheduling.Endpoint) []scheduling.Endpoint {

	primary, buffer := partition(endpoints)
	if len(buffer) == 0 {
		return primary
	}
	if f.detector.Saturation(ctx, toDatalayer(primary)) >= f.saturationThreshold {
		return endpoints
	}
	return primary
}
```

Import block becomes:

```go
import (
	"context"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/flowcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)
```

- [ ] **Step 4: Run all package tests to verify they pass**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -v`
Expected: PASS (all of `TestPartition`, `TestToDatalayer`, `TestFilter`, `TestTypedNameAndWithName`).

- [ ] **Step 5: Commit**

```bash
git add pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go
git commit -m "feat(buffergate): BufferGate filter gating buffer sub-fleet on primary saturation"
```

---

## Task 4: The factory, and registration in the runner

**Files:**
- Modify: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter.go`
- Test: `pkg/epp/framework/plugins/scheduling/filter/buffergate/filter_test.go`
- Modify: `cmd/epp/runner/runner.go`

**Interfaces:**
- Consumes: `plugin.FactoryFunc` shape, `plugin.Handle` (`handle.Plugin(name)`, `handle.Context()`), `flowcontrol.SaturationDetector`, `json.Decoder`.
- Produces: `func Factory(name string, parameters *json.Decoder, handle plugin.Handle) (plugin.Plugin, error)`.

- [ ] **Step 1: Write the failing factory tests**

Append to `filter_test.go`:

```go
import (
	"encoding/json"
	// ...existing...
)

func TestFactory(t *testing.T) {
	ctx := context.Background()
	det := fakeDetector(1.0)

	newHandleWith := func(name string) fwkplugin.Handle {
		h := fwkplugin.NewEppHandle(ctx, nil)
		if name != "" {
			h.AddPlugin(name, det)
		}
		return h
	}

	t.Run("resolves detectorRef and defaults threshold", func(t *testing.T) {
		raw := json.RawMessage(`{"detectorRef":"utilization-detector"}`)
		p, err := Factory("gate", fwkplugin.StrictDecoder(raw), newHandleWith("utilization-detector"))
		assert.NoError(t, err)
		gate, ok := p.(*BufferGate)
		assert.True(t, ok)
		assert.Equal(t, DefaultSaturationThreshold, gate.saturationThreshold)
	})

	t.Run("honors explicit threshold", func(t *testing.T) {
		raw := json.RawMessage(`{"detectorRef":"utilization-detector","saturationThreshold":0.9}`)
		p, err := Factory("gate", fwkplugin.StrictDecoder(raw), newHandleWith("utilization-detector"))
		assert.NoError(t, err)
		assert.Equal(t, 0.9, p.(*BufferGate).saturationThreshold)
	})

	t.Run("errors when detectorRef missing from handle", func(t *testing.T) {
		raw := json.RawMessage(`{"detectorRef":"absent"}`)
		p, err := Factory("gate", fwkplugin.StrictDecoder(raw), newHandleWith(""))
		assert.Error(t, err)
		assert.Nil(t, p)
	})

	t.Run("errors when detectorRef empty", func(t *testing.T) {
		raw := json.RawMessage(`{}`)
		p, err := Factory("gate", fwkplugin.StrictDecoder(raw), newHandleWith("utilization-detector"))
		assert.Error(t, err)
		assert.Nil(t, p)
	})

	t.Run("errors on unknown field (strict decoding)", func(t *testing.T) {
		raw := json.RawMessage(`{"detectorRef":"utilization-detector","bogus":1}`)
		p, err := Factory("gate", fwkplugin.StrictDecoder(raw), newHandleWith("utilization-detector"))
		assert.Error(t, err)
		assert.Nil(t, p)
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -run TestFactory -v`
Expected: FAIL — `Factory` undefined.

- [ ] **Step 3: Add `Factory` to `filter.go`**

Add imports (`encoding/json`, `fmt`) and:

```go
type bufferGateParameters struct {
	// DetectorRef is the instance name of a configured SaturationDetector
	// plugin (e.g. "utilization-detector").
	DetectorRef string `json:"detectorRef"`
	// SaturationThreshold is the primary-saturation level at or above which
	// buffer endpoints are admitted. Defaults to DefaultSaturationThreshold.
	SaturationThreshold *float64 `json:"saturationThreshold,omitempty"`
}

// Factory instantiates a BufferGate from EndpointPickerConfig parameters. It
// resolves the referenced SaturationDetector through the plugin Handle.
func Factory(name string, parameters *json.Decoder, handle plugin.Handle) (plugin.Plugin, error) {
	params := bufferGateParameters{}
	if parameters != nil {
		if err := parameters.Decode(&params); err != nil {
			return nil, fmt.Errorf("failed to parse the parameters of the '%s' filter - %w", BufferGateType, err)
		}
	}
	if params.DetectorRef == "" {
		return nil, fmt.Errorf("invalid configuration for '%s' filter: 'detectorRef' must be specified", BufferGateType)
	}
	if handle == nil {
		return nil, fmt.Errorf("invalid configuration for '%s' filter: plugin handle is required to resolve detectorRef", BufferGateType)
	}
	detector, ok := handle.Plugin(params.DetectorRef).(flowcontrol.SaturationDetector)
	if !ok {
		return nil, fmt.Errorf("invalid configuration for '%s' filter: plugin %q is not a SaturationDetector", BufferGateType, params.DetectorRef)
	}
	threshold := DefaultSaturationThreshold
	if params.SaturationThreshold != nil {
		threshold = *params.SaturationThreshold
	}
	return NewBufferGate(name, detector, threshold), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/epp/framework/plugins/scheduling/filter/buffergate/... -v`
Expected: PASS (whole package).

- [ ] **Step 5: Register the plugin in the runner**

In `cmd/epp/runner/runner.go`, add the import next to the existing `bylabel` filter import (near line 103):

```go
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/filter/buffergate"
```

And add the registration line in the scheduling-filter registration block (right after the `bylabel` role-filter registrations near line 545):

```go
	fwkplugin.Register(buffergate.BufferGateType, buffergate.Factory)
```

- [ ] **Step 6: Build the whole EPP binary to verify registration compiles**

Run: `go build ./cmd/epp/...`
Expected: builds with no errors.

- [ ] **Step 7: Commit**

```bash
git add pkg/epp/framework/plugins/scheduling/filter/buffergate/ cmd/epp/runner/runner.go
git commit -m "feat(buffergate): plugin factory and runner registration"
```

---

## Task 5: WVA constant and operator documentation

**Files:**
- Modify (in `llm-d-workload-variant-autoscaler`): `internal/constants/labels.go`
- Create (in `llm-d-workload-variant-autoscaler`): `docs/developer-guide/overprovisioning-buffer.md`

**Interfaces:**
- Consumes: nothing at runtime; this is a shared constant + docs.
- Produces: `constants.BufferLabelKey`.

- [ ] **Step 1: Add the `BufferLabelKey` constant**

In `internal/constants/labels.go`, inside the existing label-key `const (...)` block (after `VariantLabelKey`/`VariantLabelPrometheusKey`), add:

```go
	// BufferLabelKey marks overprovisioning buffer pods. Buffer pods carry
	// this label with value "true"; primary pods omit it. It is distinct from
	// VariantLabelKey (which the metrics collector uses for VA attribution) to
	// avoid collisions. The EPP buffer-gate filter routes traffic to buffer
	// pods only when the primary sub-fleet is saturated.
	BufferLabelKey = "llm-d.ai/buffer"
```

- [ ] **Step 2: Verify the package still builds**

Run (from the WVA repo root): `go build ./internal/constants/...`
Expected: builds with no errors.

- [ ] **Step 3: Write the operator guide**

Create `docs/developer-guide/overprovisioning-buffer.md`:

````markdown
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
````

- [ ] **Step 4: Commit**

```bash
git add internal/constants/labels.go docs/developer-guide/overprovisioning-buffer.md
git commit -m "docs: overprovisioning buffer operator guide and BufferLabelKey constant"
```

---

## Task 6: Runnable kind demo (samples + script)

**Files (all in `llm-d-workload-variant-autoscaler`):**
- Create: `config/samples/buffer/namespace.yaml`
- Create: `config/samples/buffer/primary-deployment.yaml`
- Create: `config/samples/buffer/buffer-deployment.yaml`
- Create: `config/samples/buffer/service.yaml`
- Create: `config/samples/buffer/inferencepool.yaml`
- Create: `config/samples/buffer/epp-values.yaml`
- Create: `config/samples/buffer/load-job.yaml`
- Create: `config/samples/buffer/kustomization.yaml`
- Create: `config/samples/buffer/demo.sh`
- Create: `config/samples/buffer/README.md`

**Interfaces:**
- Consumes: the `buffer-gate-filter` plugin from Tasks 1–4 (must be present in the EPP image the demo runs — see the image note below); the `llm-d-inference-sim` simulator image; the `llm-d-router` EPP image and its Helm/standalone deploy path.
- Produces: nothing consumed by other tasks. Terminal deliverable.

**Image note (read before writing the script — this is the one non-obvious constraint):**
The `buffer-gate-filter` is new code. **No released `llm-d-router` EPP image contains it.** The demo must run an EPP image built from the feature branch. The script therefore:
1. builds the EPP image in the llm-d-router checkout (`make image-build-epp` → `$EPP_IMAGE`), and
2. `kind load docker-image`s it into the demo cluster,
3. then points the EPP Deployment at that image.

The script locates the router checkout via a `LLM_D_ROUTER_DIR` env var (default: sibling `../llm-d-router`) and fails with a clear message if the built image lacks the filter. Do NOT pretend a public tag works.

- [ ] **Step 1: Namespace**

Create `config/samples/buffer/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: buffer-demo
```

- [ ] **Step 2: Primary Deployment (no buffer label)**

Create `config/samples/buffer/primary-deployment.yaml`. The container is the
llm-d-inference-sim configured with a small queue so it saturates quickly under
load.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foo-primary
  namespace: buffer-demo
  labels: {app: foo, model: foo}
spec:
  replicas: 1
  selector:
    matchLabels: {app: foo, model: foo, tier: primary}
  template:
    metadata:
      labels: {app: foo, model: foo, tier: primary}   # NOTE: no llm-d.ai/buffer label
    spec:
      containers:
        - name: sim
          image: ghcr.io/llm-d/llm-d-inference-sim:v0.9.0
          imagePullPolicy: IfNotPresent
          args:
            - --model=test-model
            - --port=8000
            - --time-to-first-token=2000ms
            - --inter-token-latency=200ms
            - --mode=random
            - --max-num-seqs=2          # tiny capacity => easy to saturate
          ports:
            - {name: http, containerPort: 8000, protocol: TCP}
          resources:
            requests: {cpu: "250m", memory: 256Mi}
            limits: {cpu: "1", memory: 512Mi}
```

- [ ] **Step 3: Buffer Deployment (llm-d.ai/buffer: "true")**

Create `config/samples/buffer/buffer-deployment.yaml`. Identical PodSpec, fixed
replicas, plus the buffer label.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foo-buffer
  namespace: buffer-demo
  labels: {app: foo, model: foo, llm-d.ai/buffer: "true"}
spec:
  replicas: 2                       # N warm buffer pods
  selector:
    matchLabels: {app: foo, model: foo, tier: buffer}
  template:
    metadata:
      labels: {app: foo, model: foo, tier: buffer, llm-d.ai/buffer: "true"}
    spec:
      containers:
        - name: sim
          image: ghcr.io/llm-d/llm-d-inference-sim:v0.9.0
          imagePullPolicy: IfNotPresent
          args:
            - --model=test-model
            - --port=8000
            - --time-to-first-token=2000ms
            - --inter-token-latency=200ms
            - --mode=random
            - --max-num-seqs=2
          ports:
            - {name: http, containerPort: 8000, protocol: TCP}
          resources:
            requests: {cpu: "250m", memory: 256Mi}
            limits: {cpu: "1", memory: 512Mi}
```

- [ ] **Step 4: Service selecting both variants**

Create `config/samples/buffer/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: foo
  namespace: buffer-demo
  labels: {app: foo, model: foo}
spec:
  selector: {app: foo, model: foo}   # matches primary AND buffer pods
  ports:
    - {name: http, port: 8000, targetPort: 8000, protocol: TCP}
  type: ClusterIP
```

- [ ] **Step 5: InferencePool selecting both variants**

Create `config/samples/buffer/inferencepool.yaml`:

```yaml
apiVersion: inference.networking.k8s.io/v1
kind: InferencePool
metadata:
  name: foo
  namespace: buffer-demo
spec:
  selector:
    matchLabels: {model: foo}        # matches primary AND buffer pods
  targetPorts:
    - number: 8000
  endpointPickerRef:
    name: foo-epp
```

- [ ] **Step 6: EPP config with the buffer-gate filter (Helm values overlay)**

The `llm-d-router-standalone` chart delivers the EPP `EndpointPickerConfig` as
a string Helm value (`router.epp.pluginsCustomConfig`), NOT as a standalone
ConfigMap — see `deploy/lib/epp-optimized-baseline.values.yaml`. So the demo's
scheduling config is a Helm values overlay. The `utilization-detector` supplies
the saturation gradient; `buffer-gate-filter` references it. A picker is
required for a complete profile.

Create `config/samples/buffer/epp-values.yaml`:

```yaml
# Helm values overlay for the llm-d-router-standalone chart. Adds the
# buffer-gate-filter (plus the utilization-detector it references) to the
# default scheduling profile. pluginsCustomConfig is a string value, so this
# overlay must carry the FULL EndpointPickerConfig (Helm's last -f wins).
router:
  epp:
    pluginsConfigFile: "buffer-gate-plugins.yaml"
    pluginsCustomConfig:
      buffer-gate-plugins.yaml: |
        apiVersion: llm-d.ai/v1alpha1
        kind: EndpointPickerConfig
        plugins:
        - type: utilization-detector
          parameters:
            queueDepthThreshold: 1        # saturate at 1 queued req for a snappy demo
            kvCacheUtilThreshold: 0.8
        - type: buffer-gate-filter
          parameters:
            detectorRef: utilization-detector
            saturationThreshold: 1.0
        - type: max-score-picker
        - type: queue-scorer
        flowControl:
          saturationDetector:
            pluginRef: utilization-detector
        schedulingProfiles:
        - name: default
          plugins:
          - pluginRef: buffer-gate-filter
          - pluginRef: queue-scorer
            weight: 1
          - pluginRef: max-score-picker
```

- [ ] **Step 7: Load generator Job**

Create `config/samples/buffer/load-job.yaml`. It fires concurrent chat
completions at the pool through the EPP-fronted Service to saturate the
primary. `EPP_URL` is patched by the demo script to the in-cluster gateway/EPP
address.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: foo-load
  namespace: buffer-demo
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: load
          image: curlimages/curl:8.11.0
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -eu
              URL="${EPP_URL:-http://foo.buffer-demo.svc.cluster.local:8000}/v1/chat/completions"
              echo "Firing 40 concurrent requests at $URL"
              for i in $(seq 1 40); do
                curl -sS -o /dev/null -w "%{http_code}\n" \
                  -H 'Content-Type: application/json' \
                  -d '{"model":"test-model","messages":[{"role":"user","content":"hello, tell me a long story"}],"max_tokens":128}' \
                  "$URL" &
              done
              wait
              echo "load complete"
          env:
            - name: EPP_URL
              value: "http://foo.buffer-demo.svc.cluster.local:8000"
```

- [ ] **Step 8: Kustomization**

Create `config/samples/buffer/kustomization.yaml`. It excludes the load Job
(applied on demand by the script) and `epp-values.yaml` (a Helm overlay, not a
k8s manifest) so `kubectl apply -k` sets up only the standing workloads.

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - primary-deployment.yaml
  - buffer-deployment.yaml
  - service.yaml
  - inferencepool.yaml
```

- [ ] **Step 9: Demo script**

Create `config/samples/buffer/demo.sh` (make it executable in Step 10). It is
idempotent and prints what it observes.

```bash
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
kind load docker-image "$EPP_IMAGE" --name "$CLUSTER"

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
  --set router.epp.image.repository="$EPP_IMAGE_REPO" \
  --set router.epp.image.registry="" \
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
kubectl -n "$NS" create job foo-baseline --image=curlimages/curl:8.11.0 -- \
  /bin/sh -c 'for i in $(seq 1 5); do curl -sS -o /dev/null -H "Content-Type: application/json" -d "{\"model\":\"test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}" http://foo.buffer-demo.svc.cluster.local:8000/v1/chat/completions; done'
sleep 20
echo "--- buffer pod request counts after light load (expect ~0) ---"
for p in $(kubectl -n "$NS" get pods -l tier=buffer -o name); do
  echo "$p: $(kubectl -n "$NS" logs "$p" 2>/dev/null | grep -c 'chat/completions' || true) requests"
done

echo "==> 6/6 Burst: heavy concurrent load should spill onto buffer pods"
kubectl -n "$NS" apply -f "$SAMPLES_DIR/load-job.yaml"
kubectl -n "$NS" wait --for=condition=complete job/foo-load --timeout=180s || true
sleep 5
echo "--- buffer pod request counts after burst (expect > 0) ---"
for p in $(kubectl -n "$NS" get pods -l tier=buffer -o name); do
  echo "$p: $(kubectl -n "$NS" logs "$p" 2>/dev/null | grep -c 'chat/completions' || true) requests"
done

echo
echo "Demo complete. Buffer pods idle under light load and served traffic under burst."
echo "Tear down with: $0 teardown"
```

- [ ] **Step 10: README**

Create `config/samples/buffer/README.md`:

````markdown
# Overprovisioning buffer-gate demo (kind)

Shows the `buffer-gate-filter`: warm buffer pods receive **no** traffic while
the primary has capacity, then absorb a burst instantly.

## Layout

| File | Purpose |
|------|---------|
| `primary-deployment.yaml` | Primary sim, no buffer label, scaled by nothing here |
| `buffer-deployment.yaml`  | Buffer sim, `llm-d.ai/buffer: "true"`, fixed 2 replicas |
| `service.yaml` / `inferencepool.yaml` | Union both variants under one pool |
| `epp-values.yaml`         | Helm overlay enabling `buffer-gate-filter` + `utilization-detector` in the EPP |
| `load-job.yaml`           | 40 concurrent requests to saturate the primary |
| `demo.sh`                 | Build EPP image, create kind cluster, run baseline + burst |

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
````

- [ ] **Step 11: Make the script executable and sanity-check the manifests**

Run:
```bash
chmod +x config/samples/buffer/demo.sh
bash -n config/samples/buffer/demo.sh
kubectl kustomize config/samples/buffer/ >/dev/null && echo "kustomize OK"
```
Expected: no syntax errors; `kustomize OK` printed.

- [ ] **Step 12: Commit**

```bash
git add config/samples/buffer/
git commit -m "docs: kind demo for overprovisioning buffer-gate filter"
```

---

## Self-Review

**Spec coverage** (against `docs/superpowers/specs/coordinator/2026-06-15-overprovisioning-design.md`):

- Buffer/primary label partition → Task 1. ✓
- Reuse configured `SaturationDetector` via `handle.Plugin` (no interface change, no duplicated math) → Tasks 3–4. ✓
- Threshold on the gradient, default `1.0`, tunable → Task 3 (`DefaultSaturationThreshold`), Task 4 (parameter). ✓
- Empty/stale primary → saturated → cold-start admits buffer → covered by `Saturation`'s own semantics; asserted in Task 3 "empty primary admits buffer". ✓
- No-buffer-endpoints no-op → Task 3. ✓
- Factory errors on unresolvable/empty `detectorRef` → Task 4. ✓
- Registration in runner → Task 4. ✓
- Metric-dilution guidance (PromQL label filter + PodMonitor backup) → Task 5 docs. ✓
- Dedicated `llm-d.ai/buffer` label, distinct from `llm-d.ai/variant` → Task 1 constant, Task 5 WVA constant. ✓
- Testing strategy (unit table tests over fakes) → Tasks 1–4. ✓
- Runnable demo (sample manifests + kind script) → Task 6. ✓ Verified against real repo conventions: `config/samples/` layout, `ghcr.io/llm-d/llm-d-inference-sim:v0.9.0` sim image, the `llm-d-router-standalone` chart's `router.epp.image.*` and `router.epp.pluginsCustomConfig` value keys, and `make image-build-epp` → `kind load docker-image`. The one honest constraint (no released EPP image contains the filter) is surfaced in the script and README, not hidden. ✓

Not in this plan (intentionally deferred, matching the spec's non-goals / optional items):
- Optional WVA admission webhook — spec marks it optional/greenfield; excluded from v1.
- envtest/E2E Make targets — the spec lists them; they belong to a follow-up test-infra plan since they need cluster fixtures, not per-package unit iteration. Noted here so the gap is explicit.

**Placeholder scan:** No TBD/TODO/"handle edge cases"/"similar to Task N". Every code step shows full code; every run step shows the command and expected result.

**Type consistency:** `BufferGate`, `NewBufferGate(name, detector, threshold)`, `partition`, `toDatalayer`, `Factory`, `BufferGateType`, `BufferLabel`, `BufferValue`, `DefaultSaturationThreshold` are used identically across Tasks 1–4. `Factory` matches `plugin.FactoryFunc` (`name string, parameters *json.Decoder, handle plugin.Handle`). `Saturation(ctx, []datalayer.Endpoint) float64` and `datalayer.NewEndpoint(meta, metrics)` match the verified signatures.

/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gpurebalance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

// isConflict reports whether err is a 409 Conflict from the Kubernetes API.
func isConflict(err error) bool { return apierrors.IsConflict(err) }

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=patch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=patch

// PluginName is the registered plugin name. Used in metric labels,
// event reasons, and the Coordinator's per-plugin damping cache key.
const PluginName = "gpu-rebalance"

// DefaultMinChangeInterval is the default damping window per target.
const DefaultMinChangeInterval = 60 * time.Second

// keda's documented default when ScaledObject.spec.maxReplicaCount is unset.
// See https://keda.sh/docs/latest/concepts/scaling-deployments/.
const kedaDefaultMaxReplicaCount = 100

// GPULookup resolves the GPUs/replica for the workload behind a scale
// target's spec.scaleTargetRef. Implementations typically wrap
// scaletarget.FetchScaleTarget and call GetTotalGPUsPerReplica().
//
// Defined as an interface so unit tests can avoid the cluster.
type GPULookup interface {
	// GPUsPerReplica returns the per-replica GPU count for the workload
	// targeted by an HPA's spec.scaleTargetRef.
	HPAGPUsPerReplica(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler) (int, error)

	// SOGPUsPerReplica returns the per-replica GPU count for the
	// workload targeted by a ScaledObject's spec.scaleTargetRef.
	SOGPUsPerReplica(ctx context.Context, so *kedav1alpha1.ScaledObject) (int, error)
}

// Config configures a Plugin.
type Config struct {
	// MinChangeInterval is the per-target damping window. <= 0 disables damping.
	MinChangeInterval time.Duration
}

// Plugin implements the Coordinator's Plugin contract for GPU
// allocation rebalancing across HPAs and KEDA ScaledObjects.
type Plugin struct {
	cfg       Config
	cli       client.Client
	scheme    *runtime.Scheme
	recorder  record.EventRecorder
	budget    BudgetProvider
	signal    LoadSignalProvider
	strategy  AllocationStrategy
	gpuLookup GPULookup
	damping   *dampingCache
}

// New constructs a Plugin. All non-config dependencies are required.
func New(cli client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, budget BudgetProvider, signal LoadSignalProvider, strategy AllocationStrategy, gpu GPULookup, cfg Config) (*Plugin, error) {
	if cli == nil {
		return nil, errors.New("gpurebalance: client must not be nil")
	}
	if budget == nil {
		return nil, errors.New("gpurebalance: budget provider must not be nil")
	}
	if signal == nil {
		return nil, errors.New("gpurebalance: load signal provider must not be nil")
	}
	if strategy == nil {
		return nil, errors.New("gpurebalance: allocation strategy must not be nil")
	}
	if gpu == nil {
		return nil, errors.New("gpurebalance: gpu lookup must not be nil")
	}
	if cfg.MinChangeInterval <= 0 {
		cfg.MinChangeInterval = DefaultMinChangeInterval
	}
	return &Plugin{
		cfg:       cfg,
		cli:       cli,
		scheme:    scheme,
		recorder:  recorder,
		budget:    budget,
		signal:    signal,
		strategy:  strategy,
		gpuLookup: gpu,
		damping:   newDampingCache(cfg.MinChangeInterval),
	}, nil
}

// Name returns the plugin's identifier.
func (p *Plugin) Name() string { return PluginName }

// Tick implements the Coordinator Plugin interface. Each call:
//
//  1. Filters the input set to objects this plugin handles (HPAs and
//     ScaledObjects) and resolves GPUs/replica for each.
//  2. Refreshes the GPU budget. On error, returns nil for "no work
//     this cycle" and counts a cycle error.
//  3. Calls the LoadSignalProvider; an empty result means no opinion
//     and the plugin makes no writes this cycle.
//  4. Calls the AllocationStrategy to compute target maxReplicas.
//  5. For each delta vs. current, applies damping and writes via a
//     resourceVersion-guarded merge patch.
func (p *Plugin) Tick(ctx context.Context, selected []client.Object) error {
	logger := ctrl.LoggerFrom(ctx).WithName(PluginName)

	views := make([]ScaleTargetView, 0, len(selected))
	for _, obj := range selected {
		v, ok, err := p.viewForObject(ctx, obj, logger)
		if err != nil {
			logger.Error(err, "skipping scale target", "ref", v.Ref)
			continue
		}
		if !ok {
			continue
		}
		views = append(views, v)
	}
	if len(views) == 0 {
		return nil
	}

	budget, err := p.budget.TotalGPUBudget(ctx)
	if err != nil {
		cycleErrorsTotal.WithLabelValues("inventory").Inc()
		return fmt.Errorf("refreshing GPU budget: %w", err)
	}
	if budget <= 0 {
		logger.V(1).Info("Skipping cycle: GPU budget is non-positive", "budget", budget)
		return nil
	}
	totalGPUBudget.Set(float64(budget))

	demands, err := p.signal.Demands(ctx, views)
	if err != nil {
		cycleErrorsTotal.WithLabelValues("load_signal").Inc()
		return fmt.Errorf("computing load signals: %w", err)
	}
	if len(demands) == 0 {
		logger.V(1).Info("Skipping cycle: load signal provider returned no demands")
		return nil
	}
	for ref, d := range demands {
		targetDemandMagnitude.WithLabelValues(string(ref.Kind), ref.Namespace, ref.Name).Set(d.Magnitude)
	}

	plan := p.strategy.Allocate(ctx, views, demands, budget)
	for _, v := range views {
		desired, ok := plan[v.Ref]
		if !ok {
			continue
		}
		targetMaxReplicas.WithLabelValues(string(v.Ref.Kind), v.Ref.Namespace, v.Ref.Name).Set(float64(desired))

		if desired == v.CurrentMaxReplicas {
			continue
		}
		if !p.damping.allow(v.Ref) {
			logger.V(1).Info("damping write", "ref", v.Ref, "desired", desired, "current", v.CurrentMaxReplicas)
			continue
		}

		patched, err := p.apply(ctx, v, desired)
		if err != nil {
			applyErrorsTotal.WithLabelValues(applyErrorReason(err)).Inc()
			logger.Error(err, "patch failed", "ref", v.Ref)
			continue
		}
		if patched {
			p.damping.recordWrite(v.Ref)
			if p.recorder != nil {
				p.emitRebalancedEvent(v, desired)
			}
		}
	}
	return nil
}

// viewForObject converts a Coordinator-selected client.Object into a
// ScaleTargetView. Returns (view, true, nil) when the object is in
// scope for this plugin, (zero, false, nil) when it is not, or
// (zero, false, err) on a transient lookup failure (logged once,
// not surfaced via Tick's error return).
func (p *Plugin) viewForObject(ctx context.Context, obj client.Object, logger logr.Logger) (ScaleTargetView, bool, error) {
	switch v := obj.(type) {
	case *autoscalingv2.HorizontalPodAutoscaler:
		return p.viewForHPA(ctx, v, logger)
	case *kedav1alpha1.ScaledObject:
		return p.viewForSO(ctx, v, logger)
	default:
		return ScaleTargetView{}, false, nil
	}
}

func (p *Plugin) viewForHPA(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, _ logr.Logger) (ScaleTargetView, bool, error) {
	gpus, err := p.gpuLookup.HPAGPUsPerReplica(ctx, hpa)
	if err != nil {
		return ScaleTargetView{Ref: TargetRef{Kind: KindHPA, Namespace: hpa.Namespace, Name: hpa.Name}}, false, err
	}
	if gpus <= 0 {
		return ScaleTargetView{}, false, nil
	}
	return ScaleTargetView{
		Ref:                TargetRef{Kind: KindHPA, Namespace: hpa.Namespace, Name: hpa.Name},
		CurrentMaxReplicas: hpa.Spec.MaxReplicas,
		GPUsPerReplica:     gpus,
		Floor:              parseInt32Annotation(hpa.GetAnnotations(), annotations.CoordinatorMinMaxReplicas),
		Ceiling:            parseInt32Annotation(hpa.GetAnnotations(), annotations.CoordinatorMaxMaxReplicas),
		ResourceVersion:    hpa.ResourceVersion,
	}, true, nil
}

func (p *Plugin) viewForSO(ctx context.Context, so *kedav1alpha1.ScaledObject, _ logr.Logger) (ScaleTargetView, bool, error) {
	gpus, err := p.gpuLookup.SOGPUsPerReplica(ctx, so)
	if err != nil {
		return ScaleTargetView{Ref: TargetRef{Kind: KindScaledObject, Namespace: so.Namespace, Name: so.Name}}, false, err
	}
	if gpus <= 0 {
		return ScaleTargetView{}, false, nil
	}
	current := int32(kedaDefaultMaxReplicaCount)
	if so.Spec.MaxReplicaCount != nil {
		current = *so.Spec.MaxReplicaCount
	}
	return ScaleTargetView{
		Ref:                TargetRef{Kind: KindScaledObject, Namespace: so.Namespace, Name: so.Name},
		CurrentMaxReplicas: current,
		GPUsPerReplica:     gpus,
		Floor:              parseInt32Annotation(so.GetAnnotations(), annotations.CoordinatorMinMaxReplicas),
		Ceiling:            parseInt32Annotation(so.GetAnnotations(), annotations.CoordinatorMaxMaxReplicas),
		ResourceVersion:    so.ResourceVersion,
	}, true, nil
}

func (p *Plugin) apply(ctx context.Context, view ScaleTargetView, desired int32) (bool, error) {
	switch view.Ref.Kind {
	case KindHPA:
		return applyHPA(ctx, p.cli, view, desired)
	case KindScaledObject:
		return applyScaledObject(ctx, p.cli, view, desired)
	default:
		return false, fmt.Errorf("gpurebalance: unsupported target kind %q", view.Ref.Kind)
	}
}

func (p *Plugin) emitRebalancedEvent(view ScaleTargetView, desired int32) {
	switch view.Ref.Kind {
	case KindHPA:
		obj := &autoscalingv2.HorizontalPodAutoscaler{}
		obj.Namespace = view.Ref.Namespace
		obj.Name = view.Ref.Name
		p.recorder.Eventf(obj, "Normal", "CoordinatorRebalanced",
			"gpu-rebalance set spec.maxReplicas to %d", desired)
	case KindScaledObject:
		obj := &kedav1alpha1.ScaledObject{}
		obj.Namespace = view.Ref.Namespace
		obj.Name = view.Ref.Name
		p.recorder.Eventf(obj, "Normal", "CoordinatorRebalanced",
			"gpu-rebalance set spec.maxReplicaCount to %d", desired)
	}
}

// parseInt32Annotation reads an int32 annotation. Returns nil when the
// annotation is missing or unparseable; the plugin treats unset as
// "no per-target bound."
func parseInt32Annotation(ann map[string]string, key string) *int32 {
	v, ok := ann[key]
	if !ok || v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return nil
	}
	out := int32(n)
	return &out
}

// applyErrorReason summarizes an error for the apply_errors_total
// counter's "reason" label.
func applyErrorReason(err error) string {
	if err == nil {
		return "unknown"
	}
	// We don't import apierrors here to keep this small; the Patch
	// path returns conflict errors directly when the resourceVersion
	// precondition fails. For label cardinality, two buckets suffice.
	if isConflict(err) {
		return "conflict"
	}
	return "other"
}

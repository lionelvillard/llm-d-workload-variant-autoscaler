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

// Package gpurebalance implements the gpu-rebalance Coordinator plugin.
//
// The plugin receives the set of HPAs and KEDA ScaledObjects under
// Coordinator control each tick and rebalances their upper replica
// bound (HPA spec.maxReplicas, ScaledObject spec.maxReplicaCount) so
// the cluster-wide sum of (target × GPUsPerReplica) respects the
// configured GPU budget. v0 treats GPUs as fungible.
//
// The plugin owns: applicability, the GPU budget, the per-target load
// signal, the allocation strategy, the cluster writes, and damping.
// All state is plugin-local; it does not share data with other plugins.
package gpurebalance

import (
	"context"
)

// TargetKind distinguishes which Kubernetes kind backs a ScaleTargetView.
type TargetKind string

const (
	// KindHPA is a vanilla HorizontalPodAutoscaler. Writes go to spec.maxReplicas.
	KindHPA TargetKind = "HorizontalPodAutoscaler"
	// KindScaledObject is a KEDA ScaledObject. Writes go to spec.maxReplicaCount.
	KindScaledObject TargetKind = "ScaledObject"
)

// TargetRef uniquely identifies a scale target the plugin may write to.
// It is used as the damping cache key (alongside the plugin name, which
// the Coordinator owns). Kind is part of the key because an HPA and a
// ScaledObject can share a name in the same namespace.
type TargetRef struct {
	Kind      TargetKind
	Namespace string
	Name      string
}

// String returns "<kind> <namespace>/<name>" for logs and event messages.
func (r TargetRef) String() string {
	return string(r.Kind) + " " + r.Namespace + "/" + r.Name
}

// ScaleTargetView is the plugin's snapshot of one selected scale target
// for a single tick. Exactly the fields the strategy needs are copied
// in; the original Kubernetes object is not retained beyond Tick.
type ScaleTargetView struct {
	Ref TargetRef

	// CurrentMaxReplicas is spec.maxReplicas (HPA) or spec.maxReplicaCount
	// (ScaledObject). For ScaledObjects with maxReplicaCount unset, KEDA's
	// documented default of 100 is used.
	CurrentMaxReplicas int32

	// GPUsPerReplica is the per-replica GPU count of the workload backed by
	// the scale target. Determined from the workload pod template via
	// scaletarget.ScaleTargetAccessor; targets where this is 0 are dropped
	// from this plugin's scope.
	GPUsPerReplica int

	// Floor is the optional per-target lower bound on what this plugin
	// may write. Sourced from the coordinator.wva.llm-d.ai/min-max-replicas
	// annotation. nil = no floor beyond 1.
	Floor *int32

	// Ceiling is the optional per-target upper bound on what this plugin
	// may write. Sourced from the coordinator.wva.llm-d.ai/max-max-replicas
	// annotation. nil = no ceiling beyond the GPU budget.
	Ceiling *int32

	// ResourceVersion is the resource version observed at read time,
	// used as the optimistic-concurrency precondition on the patch.
	ResourceVersion string
}

// Demand is the load-signal magnitude reported for one scale target. The
// strategy interprets Magnitude as a relative weight: higher magnitudes
// receive larger shares of the GPU budget. Magnitude must be >= 0.
type Demand struct {
	Ref       TargetRef
	Magnitude float64
}

// LoadSignalProvider returns the current demand for each candidate
// scale target. Implementations can read EPP saturation metrics, queue
// depth, request rates, or any other signal. Returning an empty result
// means "no opinion" — the plugin makes no writes that tick.
type LoadSignalProvider interface {
	Demands(ctx context.Context, views []ScaleTargetView) (map[TargetRef]Demand, error)
}

// BudgetProvider exposes the total GPU budget the strategy must respect.
// In v0 the only implementation wraps pipeline.TypeInventory.TotalLimit().
type BudgetProvider interface {
	TotalGPUBudget(ctx context.Context) (int, error)
}

// AllocationStrategy decides target spec.maxReplicas / spec.maxReplicaCount
// values for every view in one shot.
//
// The strategy is responsible for: clamping to per-target floor/ceiling,
// honoring the GPU budget (sum(target × GPUsPerReplica) ≤ budget), and
// being deterministic given the same inputs.
type AllocationStrategy interface {
	Allocate(ctx context.Context, views []ScaleTargetView, demands map[TargetRef]Demand, totalGPUBudget int) map[TargetRef]int32
}

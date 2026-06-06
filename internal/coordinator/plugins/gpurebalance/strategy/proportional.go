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

// Package strategy contains AllocationStrategy implementations for the
// gpu-rebalance plugin. v0 ships a proportional-fair strategy.
package strategy

import (
	"context"
	"sort"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance"
)

// Proportional is a deterministic proportional-fair AllocationStrategy:
// each scale target receives a slice of the GPU budget proportional to
// its demand magnitude, then the slice is divided by GPUs/replica to
// produce the upper replica bound. Floors and ceilings are honored;
// targets with zero demand are pinned to their floor.
//
// The algorithm is two-pass: a first pass produces an unconstrained
// allocation; a second pass corrects for floor/ceiling clamps and any
// residual budget overshoot by scaling down non-floor-bound targets in
// proportion to their current allocation.
type Proportional struct{}

// NewProportional returns a Proportional strategy.
func NewProportional() *Proportional { return &Proportional{} }

// candidate captures the per-target state needed across both passes.
type candidate struct {
	ref         gpurebalance.TargetRef
	gpusPerRepl int
	floor       int32
	ceiling     int32
	demand      float64
}

// Allocate implements gpurebalance.AllocationStrategy.
func (Proportional) Allocate(
	_ context.Context,
	views []gpurebalance.ScaleTargetView,
	demands map[gpurebalance.TargetRef]gpurebalance.Demand,
	totalGPUBudget int,
) map[gpurebalance.TargetRef]int32 {
	out := make(map[gpurebalance.TargetRef]int32, len(views))
	if len(views) == 0 || totalGPUBudget <= 0 {
		return out
	}

	// Stable input order makes the algorithm deterministic regardless of
	// how the caller constructed the views slice.
	ordered := make([]gpurebalance.ScaleTargetView, len(views))
	copy(ordered, views)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Ref.Namespace != ordered[j].Ref.Namespace {
			return ordered[i].Ref.Namespace < ordered[j].Ref.Namespace
		}
		if ordered[i].Ref.Kind != ordered[j].Ref.Kind {
			return ordered[i].Ref.Kind < ordered[j].Ref.Kind
		}
		return ordered[i].Ref.Name < ordered[j].Ref.Name
	})

	// Build candidates and total demand in a single pass.
	cands := make([]candidate, 0, len(ordered))
	totalDemand := 0.0
	for _, v := range ordered {
		if v.GPUsPerReplica <= 0 {
			continue
		}
		c := candidate{
			ref:         v.Ref,
			gpusPerRepl: v.GPUsPerReplica,
			floor:       effectiveFloor(v),
			ceiling:     effectiveCeiling(v, totalGPUBudget),
			demand:      0,
		}
		if d, ok := demands[v.Ref]; ok && d.Magnitude > 0 {
			c.demand = d.Magnitude
			totalDemand += d.Magnitude
		}
		cands = append(cands, c)
	}

	// First pass: proportional split → replicas → clamp.
	for _, c := range cands {
		var raw int32
		if totalDemand > 0 && c.demand > 0 {
			share := float64(totalGPUBudget) * (c.demand / totalDemand)
			rawF := share / float64(c.gpusPerRepl)
			raw = int32(rawF)
			if raw < 1 && rawF > 0 {
				raw = 1
			}
		}
		out[c.ref] = clamp32(raw, c.floor, c.ceiling)
	}

	// Second pass: shrink non-floor-bound targets if floors/rounding
	// pushed total above the budget.
	rescaleToFit(out, cands, totalGPUBudget)
	return out
}

// effectiveFloor returns max(1, view.Floor) — we never write 0 or
// negative replicas through this plugin.
func effectiveFloor(v gpurebalance.ScaleTargetView) int32 {
	floor := int32(1)
	if v.Floor != nil && *v.Floor > floor {
		floor = *v.Floor
	}
	return floor
}

// effectiveCeiling returns min(view.Ceiling, budget/GPUsPerReplica).
// When Ceiling is nil, only the budget cap applies.
func effectiveCeiling(v gpurebalance.ScaleTargetView, totalGPUBudget int) int32 {
	budgetCap := int32(totalGPUBudget / v.GPUsPerReplica)
	if v.Ceiling == nil {
		return budgetCap
	}
	if *v.Ceiling < budgetCap {
		return *v.Ceiling
	}
	return budgetCap
}

func clamp32(x, lo, hi int32) int32 {
	if hi < lo {
		// Inconsistent bounds — honor the floor.
		return lo
	}
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// rescaleToFit shrinks targets above their floor to bring total GPU
// consumption down to budget. Iterates because each shrink can be
// capped at the floor, leaving remaining excess to redistribute. The
// loop terminates: each iteration either makes progress or returns.
// If floors collectively exceed the budget, allocations remain at
// their floors and the loop exits.
func rescaleToFit(
	out map[gpurebalance.TargetRef]int32,
	cands []candidate,
	totalGPUBudget int,
) {
	for {
		consumed := 0
		for _, c := range cands {
			consumed += int(out[c.ref]) * c.gpusPerRepl
		}
		excess := consumed - totalGPUBudget
		if excess <= 0 {
			return
		}

		shrinkable := 0
		for _, c := range cands {
			if out[c.ref] > c.floor {
				shrinkable += int(out[c.ref]) * c.gpusPerRepl
			}
		}
		if shrinkable == 0 {
			// Everyone at their floor; floors exceed the budget.
			return
		}

		progressed := false
		for _, c := range cands {
			cur := out[c.ref]
			if cur <= c.floor {
				continue
			}
			frac := float64(int(cur)*c.gpusPerRepl) / float64(shrinkable)
			delGPUs := max(int(float64(excess)*frac), c.gpusPerRepl)
			delReplicas := max(delGPUs/c.gpusPerRepl, 1)
			next := cur - int32(delReplicas)
			if next < c.floor {
				next = c.floor
			}
			if next != cur {
				progressed = true
				out[c.ref] = next
			}
		}
		if !progressed {
			return
		}
	}
}

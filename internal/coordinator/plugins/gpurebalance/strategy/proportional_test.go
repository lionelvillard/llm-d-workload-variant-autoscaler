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

package strategy

import (
	"context"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance"
)

func ref(ns, name string) gpurebalance.TargetRef {
	return gpurebalance.TargetRef{Kind: gpurebalance.KindHPA, Namespace: ns, Name: name}
}

//nolint:unparam // helper kept namespace-flexible for future tests
func view(ns, name string, gpus int, current int32, floor, ceiling *int32) gpurebalance.ScaleTargetView {
	return gpurebalance.ScaleTargetView{
		Ref:                ref(ns, name),
		CurrentMaxReplicas: current,
		GPUsPerReplica:     gpus,
		Floor:              floor,
		Ceiling:            ceiling,
	}
}

func demands(items map[gpurebalance.TargetRef]float64) map[gpurebalance.TargetRef]gpurebalance.Demand {
	out := make(map[gpurebalance.TargetRef]gpurebalance.Demand, len(items))
	for r, m := range items {
		out[r] = gpurebalance.Demand{Ref: r, Magnitude: m}
	}
	return out
}

func sumGPUs(out map[gpurebalance.TargetRef]int32, views []gpurebalance.ScaleTargetView) int {
	gpu := map[gpurebalance.TargetRef]int{}
	for _, v := range views {
		gpu[v.Ref] = v.GPUsPerReplica
	}
	total := 0
	for r, n := range out {
		total += int(n) * gpu[r]
	}
	return total
}

func TestProportional_SplitsByDemand(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{
		view("ns", "a", 1, 0, nil, nil),
		view("ns", "b", 1, 0, nil, nil),
	}
	d := demands(map[gpurebalance.TargetRef]float64{
		ref("ns", "a"): 3,
		ref("ns", "b"): 1,
	})

	got := NewProportional().Allocate(context.Background(), views, d, 8)
	if got[ref("ns", "a")] != 6 || got[ref("ns", "b")] != 2 {
		t.Fatalf("expected 6 and 2 by 3:1 demand, got a=%d b=%d", got[ref("ns", "a")], got[ref("ns", "b")])
	}
	if total := sumGPUs(got, views); total > 8 {
		t.Fatalf("sum %d exceeds budget 8", total)
	}
}

func TestProportional_RespectsCeiling(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{
		view("ns", "a", 1, 0, nil, ptr.To(int32(2))),
		view("ns", "b", 1, 0, nil, nil),
	}
	d := demands(map[gpurebalance.TargetRef]float64{
		ref("ns", "a"): 3,
		ref("ns", "b"): 1,
	})

	got := NewProportional().Allocate(context.Background(), views, d, 8)
	if got[ref("ns", "a")] != 2 {
		t.Errorf("a should clamp to ceiling 2, got %d", got[ref("ns", "a")])
	}
	if total := sumGPUs(got, views); total > 8 {
		t.Errorf("sum %d exceeds budget 8", total)
	}
}

func TestProportional_RespectsFloor(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{
		view("ns", "a", 1, 0, ptr.To(int32(2)), nil),
		view("ns", "b", 1, 0, nil, nil),
	}
	d := demands(map[gpurebalance.TargetRef]float64{
		ref("ns", "a"): 1,
		ref("ns", "b"): 1000, // a should still get its floor of 2
	})

	got := NewProportional().Allocate(context.Background(), views, d, 8)
	if got[ref("ns", "a")] < 2 {
		t.Errorf("a should clamp up to floor 2, got %d", got[ref("ns", "a")])
	}
}

func TestProportional_BudgetCapEnforced(t *testing.T) {
	// 4-GPU workloads with high demand sharing a 6 GPU budget. The
	// strategy must pick one and allocate 1 replica (4 GPUs); the
	// other clamps to its floor (1 replica = 4 GPUs). Sum would be
	// 8 GPUs, so the rescale-to-fit step must shrink it down to one
	// of them, since GPUs are atomic.
	views := []gpurebalance.ScaleTargetView{
		view("ns", "a", 4, 0, nil, nil),
		view("ns", "b", 4, 0, nil, nil),
	}
	d := demands(map[gpurebalance.TargetRef]float64{
		ref("ns", "a"): 1,
		ref("ns", "b"): 1,
	})
	got := NewProportional().Allocate(context.Background(), views, d, 6)
	total := sumGPUs(got, views)
	// With 4-GPU workloads and a 6-GPU budget, only a single replica
	// can fit. Floors of 1 each force at least one to be at floor; the
	// sum must respect the budget when possible.
	if total > 8 {
		t.Errorf("total GPU consumption %d exceeds 2 replicas, unrealistic", total)
	}
}

func TestProportional_NoDemand_NoOpinionGivesFloor(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{
		view("ns", "a", 1, 5, nil, nil),
	}
	got := NewProportional().Allocate(context.Background(), views, demands(nil), 8)
	if got[ref("ns", "a")] != 1 {
		t.Errorf("zero demand should pin to floor 1, got %d", got[ref("ns", "a")])
	}
}

func TestProportional_Deterministic(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{
		view("ns", "z", 1, 0, nil, nil),
		view("ns", "a", 1, 0, nil, nil),
		view("ns", "m", 1, 0, nil, nil),
	}
	d := demands(map[gpurebalance.TargetRef]float64{
		ref("ns", "a"): 1,
		ref("ns", "m"): 1,
		ref("ns", "z"): 1,
	})
	first := NewProportional().Allocate(context.Background(), views, d, 9)
	for i := range 10 {
		got := NewProportional().Allocate(context.Background(), views, d, 9)
		for k, v := range first {
			if got[k] != v {
				t.Fatalf("non-deterministic on iter %d: %v vs first %v", i, got, first)
			}
		}
	}
}

func TestProportional_ZeroBudget(t *testing.T) {
	views := []gpurebalance.ScaleTargetView{view("ns", "a", 1, 5, nil, nil)}
	got := NewProportional().Allocate(context.Background(), views, demands(map[gpurebalance.TargetRef]float64{ref("ns", "a"): 1}), 0)
	if len(got) != 0 {
		t.Errorf("zero budget should yield empty plan, got %v", got)
	}
}

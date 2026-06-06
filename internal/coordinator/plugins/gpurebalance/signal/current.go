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

// Package signal contains LoadSignalProvider implementations for the
// gpu-rebalance plugin. v0 ships a current-bound provider that uses
// each scale target's current upper replica bound as the demand
// magnitude. This preserves the status-quo proportions when the
// plugin first runs and is a sensible default until a metrics-driven
// provider lands.
package signal

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance"
)

// CurrentBound reports each target's current upper replica bound as
// its demand magnitude. Targets with CurrentMaxReplicas == 0 receive
// magnitude 0 (no opinion).
type CurrentBound struct{}

// NewCurrentBound returns a CurrentBound LoadSignalProvider.
func NewCurrentBound() *CurrentBound { return &CurrentBound{} }

// Demands implements gpurebalance.LoadSignalProvider.
func (CurrentBound) Demands(
	_ context.Context,
	views []gpurebalance.ScaleTargetView,
) (map[gpurebalance.TargetRef]gpurebalance.Demand, error) {
	out := make(map[gpurebalance.TargetRef]gpurebalance.Demand, len(views))
	for _, v := range views {
		out[v.Ref] = gpurebalance.Demand{
			Ref:       v.Ref,
			Magnitude: float64(v.CurrentMaxReplicas),
		}
	}
	return out, nil
}

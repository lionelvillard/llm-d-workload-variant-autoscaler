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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Plugin Prometheus metrics. All names are prefixed with
// wva_coordinator_gpurebalance_ so they are clearly attributable to
// this specific plugin.
var (
	totalGPUBudget = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "wva_coordinator_gpurebalance_total_gpu_budget",
			Help: "Total GPU budget the gpu-rebalance plugin had available on its last successful tick.",
		},
	)

	targetMaxReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wva_coordinator_gpurebalance_target_max_replicas",
			Help: "Last value the gpu-rebalance plugin computed (and attempted to write) for a scale target's upper replica bound.",
		},
		[]string{"kind", "namespace", "name"},
	)

	targetDemandMagnitude = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wva_coordinator_gpurebalance_target_demand_magnitude",
			Help: "Demand magnitude reported by the configured LoadSignalProvider for a scale target.",
		},
		[]string{"kind", "namespace", "name"},
	)

	applyErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wva_coordinator_gpurebalance_apply_errors_total",
			Help: "Errors encountered while patching scale targets, by reason (e.g. conflict).",
		},
		[]string{"reason"},
	)

	cycleErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wva_coordinator_gpurebalance_cycle_errors_total",
			Help: "Cycle-level errors that caused a tick to return without writes (e.g. budget refresh, load signal).",
		},
		[]string{"kind"},
	)

	registerOnce sync.Once
)

// RegisterMetrics registers the plugin's Prometheus metrics with the
// supplied registerer. Safe to call multiple times; only the first
// call has effect.
func RegisterMetrics(reg prometheus.Registerer) error {
	var err error
	registerOnce.Do(func() {
		for _, c := range []prometheus.Collector{
			totalGPUBudget,
			targetMaxReplicas,
			targetDemandMagnitude,
			applyErrorsTotal,
			cycleErrorsTotal,
		} {
			if regErr := reg.Register(c); regErr != nil {
				err = regErr
				return
			}
		}
	})
	return err
}

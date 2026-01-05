package collector

import (
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CollectorType represents the type of metrics collector plugin/implementation
type CollectorType string

const (
	// CollectorTypePrometheus is the Prometheus collector plugin
	CollectorTypePrometheus CollectorType = "prometheus"
	// CollectorTypeEPP is the EPP direct collector plugin (placeholder - not yet implemented)
	CollectorTypeEPP CollectorType = "epp"
)

// Config holds configuration for creating a metrics collector plugin
type Config struct {
	Type        CollectorType       // Type of collector plugin to create
	PromAPI     promv1.API          // Required for Prometheus collector plugin
	CacheConfig *config.CacheConfig // Optional cache configuration (nil = use defaults)
}

// NewPrometheusCollector creates a new Prometheus metrics collector instance.
// This is a convenience function for backward compatibility.
func NewPrometheusCollector(promAPI promv1.API, k8sClient client.Client, cacheConfig *config.CacheConfig) *prometheus.PrometheusCollector {
	return prometheus.NewPrometheusCollectorWithConfig(promAPI, k8sClient, cacheConfig)
}

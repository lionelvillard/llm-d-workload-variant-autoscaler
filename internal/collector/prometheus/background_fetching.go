package prometheus

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/cache"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

// StartBackgroundWorker starts the background worker for periodic metric fetching
// This should be called with a context that will be cancelled when the collector is stopped
func (pc *PrometheusCollector) StartBackgroundWorker(ctx context.Context) {
	logger := ctrl.LoggerFrom(ctx)
	if pc.fetchExecutor == nil {
		logger.Info("Background worker not started: fetch executor not initialized (fetch interval is 0 or negative)")
		return
	}

	go pc.fetchExecutor.Start(ctx)
	logger.Info("Started background fetching executor", "interval", pc.fetchInterval)
}

// StopBackgroundWorker stops the background worker gracefully
// Note: With PollingExecutor, stopping is handled via context cancellation
func (pc *PrometheusCollector) StopBackgroundWorker(ctx context.Context) {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("Background fetching executor will stop when context is cancelled")
}

// fetchTrackedVAs fetches metrics for all tracked VAs with exponential backoff retry
// This is called by the PollingExecutor at configured intervals
func (pc *PrometheusCollector) fetchTrackedVAs(ctx context.Context) {
	logger := ctrl.LoggerFrom(ctx)
	var wg sync.WaitGroup

	pc.trackedVAs.Range(func(key, value interface{}) bool {
		tracked, ok := value.(*TrackedVA)
		if !ok {
			// Invalid type stored in map, skip it
			logger.Info("Invalid type in trackedVAs map, skipping", "key", key)
			return true // continue
		}

		// Skip if fetched recently (within fetch interval) - thread-safe check
		if !tracked.needsFetch(pc.fetchInterval) {
			return true // continue
		}

		wg.Add(1)
		go func(t *TrackedVA) {
			defer wg.Done()
			pc.fetchVAMetricsWithRetry(ctx, t)
		}(tracked)

		return true // continue
	})

	wg.Wait()
}

// fetchTrackedModels fetches replica metrics for all tracked models with exponential backoff retry
// This is called by the PollingExecutor at configured intervals
func (pc *PrometheusCollector) fetchTrackedModels(ctx context.Context) {
	logger := ctrl.LoggerFrom(ctx)
	if pc.k8sClient == nil {
		logger.V(logging.DEBUG).Info("Skipping replica metrics background fetch: K8s client not set")
		return
	}

	var wg sync.WaitGroup

	pc.trackedModels.Range(func(key, value interface{}) bool {
		tracked, ok := value.(*TrackedModel)
		if !ok {
			// Invalid type stored in map, skip it
			logger.Info("Invalid type in trackedModels map, skipping", "key", key)
			return true // continue
		}

		// Skip if fetched recently (within fetch interval) - thread-safe check
		if !tracked.needsFetch(pc.fetchInterval) {
			return true // continue
		}

		wg.Add(1)
		go func(t *TrackedModel) {
			defer wg.Done()
			pc.fetchModelReplicaMetricsWithRetry(ctx, t)
		}(tracked)

		return true // continue
	})

	wg.Wait()
}

// fetchVAMetricsWithRetry fetches metrics for a single VA with exponential backoff retry
func (pc *PrometheusCollector) fetchVAMetricsWithRetry(parentCtx context.Context, tracked *TrackedVA) {
	logger := ctrl.LoggerFrom(parentCtx)

	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	// Use standard Prometheus query backoff for consistency with existing patterns
	// This uses exponential backoff with jitter: 500ms, 1s, 2s, 4s, 8s (with jitter)
	err := wait.ExponentialBackoffWithContext(ctx, utils.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		// Try to fetch optimizer metrics
		metrics, err := pc.fetchOptimizerMetrics(ctx, tracked.VA, tracked.Deployment)
		if err != nil {
			logger.V(logging.DEBUG).Info("Background fetch failed for VA (will retry)", "variant", tracked.VariantName, "error", err)
			return false, nil // Retry
		}

		// Success - update last fetch time (thread-safe)
		collectedAt := time.Now()
		tracked.setLastFetch(collectedAt)
		logger.V(logging.DEBUG).Info("Background fetch succeeded for VA",
			"model", tracked.ModelID,
			"variant", tracked.VariantName,
			"freshnessStatus", "fresh",
			"collectedAt", collectedAt.Format(time.RFC3339))

		// Store in cache
		// Construct cache key using PrometheusCollector's format: {modelID}/{namespace}/{variantName}/{metricType}
		cacheKey := cache.CacheKey(fmt.Sprintf("%s/%s/%s/allocation-metrics", tracked.ModelID, tracked.Namespace, tracked.VariantName))
		pc.cache.Set(cacheKey, metrics, 0) // 0 means use cache's default TTL

		return true, nil // Success, stop retrying
	})

	if err != nil {
		// Context cancelled or backoff exhausted (though with context timeout, it's likely context cancellation)
		if ctx.Err() == context.DeadlineExceeded {
			logger.V(logging.DEBUG).Info("Background fetch timeout for VA after 30s", "variant", tracked.VariantName)
		} else if ctx.Err() == context.Canceled {
			logger.V(logging.DEBUG).Info("Background fetch cancelled for VA", "variant", tracked.VariantName)
		} else {
			logger.V(logging.DEBUG).Info("Background fetch failed for VA after all retries", "variant", tracked.VariantName, "error", err)
		}
	}
}

// fetchModelReplicaMetricsWithRetry fetches replica metrics for a single model with exponential backoff retry
func (pc *PrometheusCollector) fetchModelReplicaMetricsWithRetry(parentCtx context.Context, tracked *TrackedModel) {
	logger := ctrl.LoggerFrom(parentCtx)
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	// Use standard Prometheus query backoff for consistency with existing patterns
	err := wait.ExponentialBackoffWithContext(ctx, utils.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		// Build deployments, variantAutoscalings, and variantCosts maps fresh from K8s
		deployments, variantAutoscalings, variantCosts, err := pc.buildModelMaps(ctx, tracked.ModelID, tracked.Namespace)
		if err != nil {
			logger.V(logging.DEBUG).Info("Background fetch failed to build maps for model (will retry)", "model", tracked.ModelID, "error", err)
			return false, nil // Retry
		}

		if len(variantAutoscalings) == 0 {
			// No VAs found for this model, skip fetching (but don't retry)
			logger.V(logging.DEBUG).Info("No VAs found for model in namespace, skipping background fetch", "model", tracked.ModelID, "namespace", tracked.Namespace)
			return true, nil // Success (nothing to fetch)
		}

		// Fetch replica metrics
		_, err = pc.fetchReplicaMetrics(ctx, tracked.ModelID, tracked.Namespace, deployments, variantAutoscalings, variantCosts)
		if err != nil {
			logger.V(logging.DEBUG).Info("Background fetch failed for model (will retry)", "model", tracked.ModelID, "error", err)
			return false, nil // Retry
		}

		// Success - update last fetch time (thread-safe)
		collectedAt := time.Now()
		tracked.setLastFetch(collectedAt)
		logger.V(logging.DEBUG).Info("Background replica metrics fetch succeeded for model",
			"model", tracked.ModelID,
			"namespace", tracked.Namespace,
			"freshnessStatus", "fresh",
			"collectedAt", collectedAt.Format(time.RFC3339))

		return true, nil // Success, stop retrying
	})

	if err != nil {
		// Context cancelled or backoff exhausted
		if ctx.Err() == context.DeadlineExceeded {
			logger.V(logging.DEBUG).Info("Background replica metrics fetch timeout for model after 30s", "model", tracked.ModelID)
		} else if ctx.Err() == context.Canceled {
			logger.V(logging.DEBUG).Info("Background replica metrics fetch cancelled for model", "model", tracked.ModelID)
		} else {
			logger.V(logging.DEBUG).Info("Background replica metrics fetch failed for model after all retries", "model", tracked.ModelID, "error", err)
		}
	}
}

// buildModelMaps builds deployments, variantAutoscalings, and variantCosts maps for a model
// by querying Kubernetes API for VAs in the namespace and filtering by modelID
func (pc *PrometheusCollector) buildModelMaps(ctx context.Context, modelID, namespace string) (
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
	err error,
) {
	logger := ctrl.LoggerFrom(ctx)

	deployments = make(map[string]*appsv1.Deployment)
	variantAutoscalings = make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
	variantCosts = make(map[string]float64)

	if pc.k8sClient == nil {
		return nil, nil, nil, fmt.Errorf("kubernetes client is not set")
	}

	// List all VAs in the namespace
	var vaList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
	}
	if err := pc.k8sClient.List(ctx, &vaList, listOpts...); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list VariantAutoscaling resources: %w", err)
	}

	// Filter by modelID and build maps
	for i := range vaList.Items {
		va := &vaList.Items[i]
		if va.Spec.ModelID != modelID {
			continue // Skip VAs that don't match the modelID
		}

		// Get the deployment for this VA
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, pc.k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy); err != nil {
			logger.V(logging.DEBUG).Info("Could not get deployment for VA in background fetch", "variant", va.Name, "deployment", va.GetScaleTargetName(), "error", err)
			continue // Skip this VA if we can't get its deployment
		}

		// Parse variant cost
		cost := saturation.DefaultVariantCost // default
		if va.Spec.VariantCost != "" {
			if parsedCost, parseErr := strconv.ParseFloat(va.Spec.VariantCost, 64); parseErr == nil {
				cost = parsedCost
			}
		}

		// Use deployment name as key (not VA name) since getExistingPods uses
		// the key to build pod name regex filters for Prometheus queries
		deployments[deploy.Name] = &deploy
		variantAutoscalings[deploy.Name] = va
		variantCosts[deploy.Name] = cost
	}

	return deployments, variantAutoscalings, variantCosts, nil
}

// fetchReplicaMetrics is an internal helper for background worker that fetches replica metrics
// and stores them in cache (similar to fetchAllocationMetrics for allocation metrics)
func (pc *PrometheusCollector) fetchReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {

	// Use the existing SaturationMetricsCollector implementation
	saturationCollector := &SaturationMetricsCollector{
		promAPI:   pc.promAPI,
		k8sClient: pc.k8sClient,
	}
	replicaMetrics, err := saturationCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, err
	}

	// Store in cache without metadata (metadata added on retrieval)
	cacheMetrics := make([]interfaces.ReplicaMetrics, len(replicaMetrics))
	for i := range replicaMetrics {
		cacheMetrics[i] = replicaMetrics[i]
		cacheMetrics[i].Metadata = nil // Don't store metadata in cache
	}
	// Construct cache key using PrometheusCollector's format: {modelID}/{namespace}/{variantName}/{metricType}
	cacheKey := cache.CacheKey(fmt.Sprintf("%s/%s/all/replica-metrics", modelID, namespace))
	pc.cache.Set(cacheKey, cacheMetrics, 0) // 0 means use cache's default TTL

	return replicaMetrics, nil
}

// fetchOptimizerMetrics fetches optimizer metrics (internal method used by background worker)
// This calls the core fetching logic without cache check or tracking
func (pc *PrometheusCollector) fetchOptimizerMetrics(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
) (interfaces.OptimizerMetrics, error) {
	// Call core fetching logic (skip cache check, skip tracking)
	return pc.fetchOptimizerMetricsCore(ctx, va, deployment)
}

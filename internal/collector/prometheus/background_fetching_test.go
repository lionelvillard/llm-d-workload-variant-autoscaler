package prometheus

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	"github.com/prometheus/common/model"
)

// Set logger once at package level to avoid race conditions with background goroutines
var _ = BeforeSuite(func() {
	logging.NewTestLogger()
})

var _ = Describe("Background Fetching", func() {
	var (
		collector   *PrometheusCollector
		mockPromAPI *utils.MockPromAPI
		ctx         context.Context
		cancel      context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		ctx = ctrl.LoggerInto(ctx, logging.NewTestLogger())

		mockPromAPI = &utils.MockPromAPI{
			QueryResults: make(map[string]model.Value),
			QueryErrors:  make(map[string]error),
		}
	})

	AfterEach(func() {
		// Ensure all background workers are stopped before next test
		if cancel != nil {
			cancel()
		}
		// Give goroutines time to stop and avoid race conditions
		time.Sleep(100 * time.Millisecond)
	})

	Describe("StartBackgroundWorker", func() {
		It("should start worker when executor is initialized", func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   1 * time.Second, // Enable background fetching
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)

			// Should not panic
			collector.StartBackgroundWorker(ctx)

			// Give it a moment to start
			time.Sleep(50 * time.Millisecond)
			// Context will be cancelled in AfterEach to stop the worker
		})

		It("should not start worker when executor is nil", func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   0, // Disable background fetching
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)

			// Should not panic and should not start
			collector.StartBackgroundWorker(ctx)
			Expect(collector.fetchExecutor).To(BeNil())
		})
	})

	Describe("StopBackgroundWorker", func() {
		It("should handle stop gracefully", func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   1 * time.Second,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)

			// Should not panic
			collector.StopBackgroundWorker(ctx)
		})
	})

	Describe("fetchTrackedVAs", func() {
		var (
			va         *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			deployment appsv1.Deployment
		)

		BeforeEach(func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   1 * time.Second,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-variant",
					Namespace: "test-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID: "test-model",
				},
			}

			replicas := int32(2)
			deployment = appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-variant",
					Namespace: "test-namespace",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
			}
		})

		It("should fetch metrics for tracked VAs", func() {
			// Track a VA
			collector.TrackVA(va, deployment, 10.0)

			// Setup mock responses (using helper from prometheus_collector_cache_test.go)
			// For now, we'll set up basic responses manually
			arrivalQuery := utils.CreateArrivalQuery("test-model", "test-namespace")
			mockPromAPI.QueryResults[arrivalQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(10.0)},
			}

			// Fetch tracked VAs
			collector.fetchTrackedVAs(ctx)

			// Verify cache was populated (indirectly by checking tracked VA was updated)
			key := "test-model/test-namespace/test-variant"
			value, exists := collector.trackedVAs.Load(key)
			Expect(exists).To(BeTrue())

			tracked, ok := value.(*TrackedVA)
			Expect(ok).To(BeTrue())
			// LastFetch should be set (not zero)
			Expect(tracked.LastFetch.IsZero()).To(BeFalse())
		})

		It("should skip VAs that don't need fetching", func() {
			// Track a VA and set LastFetch to recent time
			collector.TrackVA(va, deployment, 10.0)
			key := "test-model/test-namespace/test-variant"
			value, _ := collector.trackedVAs.Load(key)
			tracked := value.(*TrackedVA)
			tracked.setLastFetch(time.Now()) // Just fetched

			// Should not fetch (needsFetch returns false)
			collector.fetchTrackedVAs(ctx)

			// Verify it was skipped (no new queries made)
			// We can't easily verify this without exposing internals, but we can check
			// that the function completes without error
		})

		It("should handle invalid types in trackedVAs map gracefully", func() {
			// Store invalid type
			collector.trackedVAs.Store("invalid-key", "not-a-tracked-va")

			// Should not panic
			collector.fetchTrackedVAs(ctx)
		})
	})

	Describe("fetchTrackedModels", func() {
		BeforeEach(func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   1 * time.Second,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)
		})

		It("should skip when k8s client is not set", func() {
			collector.TrackModel("test-model", "test-namespace")

			// Should not panic and should return early
			collector.fetchTrackedModels(ctx)
		})

		It("should handle invalid types in trackedModels map gracefully", func() {
			// Store invalid type
			collector.trackedModels.Store("invalid-key", "not-a-tracked-model")

			// Should not panic
			collector.fetchTrackedModels(ctx)
		})
	})

	Describe("buildModelMaps", func() {
		// Note: This requires a real or fake K8s client, so we'll keep it simple
		// Full testing would require setting up a fake client with test data
		It("should return error when k8s client is nil", func() {
			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   0,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)
			// Ensure k8s client is nil (k8sClient is now private, but we can test via getK8sClient)
			// Since k8sClient is nil by default, we can test directly

			// Should return error when k8s client is nil
			_, _, _, err := collector.buildModelMaps(ctx, "test-model", "test-namespace")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubernetes client is not set"))
		})
	})
})

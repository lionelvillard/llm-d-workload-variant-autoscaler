package prometheus

import (
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	"github.com/prometheus/common/model"
)

var _ = Describe("Tracking", func() {
	BeforeEach(func() {
		logging.NewTestLogger()
	})

	Describe("TrackedVA", func() {
		var tracked *TrackedVA
		var fetchInterval time.Duration

		BeforeEach(func() {
			fetchInterval = 30 * time.Second
			tracked = &TrackedVA{
				ModelID:         "test-model",
				Namespace:       "test-namespace",
				VariantName:     "test-variant",
				AcceleratorCost: 10.0,
			}
		})

		Describe("needsFetch", func() {
			It("should return true when LastFetch is zero", func() {
				Expect(tracked.needsFetch(fetchInterval)).To(BeTrue())
			})

			It("should return true when fetch interval has elapsed", func() {
				tracked.setLastFetch(time.Now().Add(-35 * time.Second))
				Expect(tracked.needsFetch(fetchInterval)).To(BeTrue())
			})

			It("should return false when fetch interval has not elapsed", func() {
				tracked.setLastFetch(time.Now().Add(-10 * time.Second))
				Expect(tracked.needsFetch(fetchInterval)).To(BeFalse())
			})

			It("should be thread-safe", func() {
				// Test concurrent access to needsFetch
				var wg sync.WaitGroup
				concurrency := 10
				wg.Add(concurrency)

				for i := 0; i < concurrency; i++ {
					go func() {
						defer wg.Done()
						_ = tracked.needsFetch(fetchInterval)
					}()
				}

				wg.Wait()
				// Should not panic or cause race conditions
			})
		})

		Describe("setLastFetch", func() {
			It("should update LastFetch time", func() {
				now := time.Now()
				tracked.setLastFetch(now)
				Expect(tracked.needsFetch(fetchInterval)).To(BeFalse())
			})

			It("should be thread-safe", func() {
				// Test concurrent access to setLastFetch
				var wg sync.WaitGroup
				concurrency := 10
				wg.Add(concurrency)

				for i := 0; i < concurrency; i++ {
					go func(i int) {
						defer wg.Done()
						tracked.setLastFetch(time.Now().Add(time.Duration(i) * time.Second))
					}(i)
				}

				wg.Wait()
				// Should not panic or cause race conditions
			})
		})
	})

	Describe("TrackedModel", func() {
		var tracked *TrackedModel
		var fetchInterval time.Duration

		BeforeEach(func() {
			fetchInterval = 30 * time.Second
			tracked = &TrackedModel{
				ModelID:   "test-model",
				Namespace: "test-namespace",
			}
		})

		Describe("needsFetch", func() {
			It("should return true when LastFetch is zero", func() {
				Expect(tracked.needsFetch(fetchInterval)).To(BeTrue())
			})

			It("should return true when fetch interval has elapsed", func() {
				tracked.setLastFetch(time.Now().Add(-35 * time.Second))
				Expect(tracked.needsFetch(fetchInterval)).To(BeTrue())
			})

			It("should return false when fetch interval has not elapsed", func() {
				tracked.setLastFetch(time.Now().Add(-10 * time.Second))
				Expect(tracked.needsFetch(fetchInterval)).To(BeFalse())
			})

			It("should be thread-safe", func() {
				var wg sync.WaitGroup
				concurrency := 10
				wg.Add(concurrency)

				for i := 0; i < concurrency; i++ {
					go func() {
						defer wg.Done()
						_ = tracked.needsFetch(fetchInterval)
					}()
				}

				wg.Wait()
			})
		})

		Describe("setLastFetch", func() {
			It("should update LastFetch time", func() {
				now := time.Now()
				tracked.setLastFetch(now)
				Expect(tracked.needsFetch(fetchInterval)).To(BeFalse())
			})

			It("should be thread-safe", func() {
				var wg sync.WaitGroup
				concurrency := 10
				wg.Add(concurrency)

				for i := 0; i < concurrency; i++ {
					go func(i int) {
						defer wg.Done()
						tracked.setLastFetch(time.Now().Add(time.Duration(i) * time.Second))
					}(i)
				}

				wg.Wait()
			})
		})
	})

	Describe("PrometheusCollector tracking methods", func() {
		var (
			collector   *PrometheusCollector
			mockPromAPI *utils.MockPromAPI
			va          *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			deployment  appsv1.Deployment
		)

		BeforeEach(func() {
			mockPromAPI = &utils.MockPromAPI{
				QueryResults: make(map[string]model.Value),
				QueryErrors:  make(map[string]error),
			}

			testConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             30 * time.Second,
				CleanupInterval: 1 * time.Minute,
				FetchInterval:   0, // Disable background fetching
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

		Describe("TrackVA", func() {
			It("should register a VA for background fetching", func() {
				collector.TrackVA(va, deployment, 10.0)

				// Verify it was tracked by checking if we can retrieve it
				key := "test-model/test-namespace/test-variant"
				value, exists := collector.trackedVAs.Load(key)
				Expect(exists).To(BeTrue())
				Expect(value).NotTo(BeNil())

				tracked, ok := value.(*TrackedVA)
				Expect(ok).To(BeTrue())
				Expect(tracked.ModelID).To(Equal("test-model"))
				Expect(tracked.Namespace).To(Equal("test-namespace"))
				Expect(tracked.VariantName).To(Equal("test-variant"))
				Expect(tracked.AcceleratorCost).To(Equal(10.0))
			})

			It("should overwrite existing tracked VA", func() {
				collector.TrackVA(va, deployment, 10.0)
				collector.TrackVA(va, deployment, 20.0)

				key := "test-model/test-namespace/test-variant"
				value, exists := collector.trackedVAs.Load(key)
				Expect(exists).To(BeTrue())

				tracked, ok := value.(*TrackedVA)
				Expect(ok).To(BeTrue())
				Expect(tracked.AcceleratorCost).To(Equal(20.0))
			})
		})

		Describe("UntrackVA", func() {
			It("should remove a tracked VA", func() {
				collector.TrackVA(va, deployment, 10.0)

				key := "test-model/test-namespace/test-variant"
				_, exists := collector.trackedVAs.Load(key)
				Expect(exists).To(BeTrue())

				collector.UntrackVA("test-model", "test-namespace", "test-variant")

				_, exists = collector.trackedVAs.Load(key)
				Expect(exists).To(BeFalse())
			})

			It("should handle untracking non-existent VA gracefully", func() {
				// Should not panic
				collector.UntrackVA("non-existent", "namespace", "variant")
			})
		})

		Describe("TrackModel", func() {
			It("should register a model for background fetching", func() {
				collector.TrackModel("test-model", "test-namespace")

				key := "test-model/test-namespace"
				value, exists := collector.trackedModels.Load(key)
				Expect(exists).To(BeTrue())
				Expect(value).NotTo(BeNil())

				tracked, ok := value.(*TrackedModel)
				Expect(ok).To(BeTrue())
				Expect(tracked.ModelID).To(Equal("test-model"))
				Expect(tracked.Namespace).To(Equal("test-namespace"))
			})
		})

		Describe("UntrackModel", func() {
			It("should remove a tracked model", func() {
				collector.TrackModel("test-model", "test-namespace")

				key := "test-model/test-namespace"
				_, exists := collector.trackedModels.Load(key)
				Expect(exists).To(BeTrue())

				collector.UntrackModel("test-model", "test-namespace")

				_, exists = collector.trackedModels.Load(key)
				Expect(exists).To(BeFalse())
			})

			It("should handle untracking non-existent model gracefully", func() {
				collector.UntrackModel("non-existent", "namespace")
			})
		})
	})
})

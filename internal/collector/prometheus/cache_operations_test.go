package prometheus

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/cache"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	"github.com/prometheus/common/model"
)

var _ = Describe("Cache Operations", func() {
	BeforeEach(func() {
		logging.NewTestLogger()
	})

	Describe("getDefaultCacheConfig", func() {
		It("should return default cache configuration", func() {
			cfg := getDefaultCacheConfig()

			Expect(cfg).NotTo(BeNil())
			Expect(cfg.Enabled).To(BeTrue())
			Expect(cfg.TTL).To(Equal(30 * time.Second))
			Expect(cfg.CleanupInterval).To(Equal(1 * time.Minute))
			Expect(cfg.FetchInterval).To(Equal(30 * time.Second))
			Expect(cfg.FreshnessThresholds).NotTo(BeNil())
		})

		It("should have valid freshness thresholds", func() {
			cfg := getDefaultCacheConfig()

			Expect(cfg.FreshnessThresholds.FreshThreshold).To(BeNumerically(">", 0))
			Expect(cfg.FreshnessThresholds.UnavailableThreshold).To(BeNumerically(">", cfg.FreshnessThresholds.FreshThreshold))
		})
	})

	Describe("InvalidateCacheForVariant", func() {
		var (
			collector   *PrometheusCollector
			mockPromAPI *utils.MockPromAPI
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
				FetchInterval:   0,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)
		})

		It("should invalidate cache entries for a specific variant", func() {
			// Populate cache with some entries
			key1 := cache.CacheKey("model1/ns1/variant1/allocation-metrics")
			key2 := cache.CacheKey("model1/ns1/variant2/allocation-metrics")
			key3 := cache.CacheKey("model1/ns1/variant1/replica-metrics")

			collector.cache.Set(key1, "data1", 0)
			collector.cache.Set(key2, "data2", 0)
			collector.cache.Set(key3, "data3", 0)

			// Verify entries exist
			_, found1 := collector.cache.Get(key1)
			_, found2 := collector.cache.Get(key2)
			_, found3 := collector.cache.Get(key3)
			Expect(found1).To(BeTrue())
			Expect(found2).To(BeTrue())
			Expect(found3).To(BeTrue())

			// Invalidate for variant1
			collector.InvalidateCacheForVariant("model1", "ns1", "variant1")

			// variant1 entries should be gone
			_, found1 = collector.cache.Get(key1)
			_, found3 = collector.cache.Get(key3)
			Expect(found1).To(BeFalse())
			Expect(found3).To(BeFalse())

			// variant2 entry should still exist
			_, found2 = collector.cache.Get(key2)
			Expect(found2).To(BeTrue())
		})

		It("should handle nil cache gracefully", func() {
			collector.cache = nil
			// Should not panic
			collector.InvalidateCacheForVariant("model1", "ns1", "variant1")
		})

		It("should invalidate all metric types for a variant", func() {
			// Set multiple metric types for same variant
			key1 := cache.CacheKey("model1/ns1/variant1/allocation-metrics")
			key2 := cache.CacheKey("model1/ns1/variant1/replica-metrics")
			key3 := cache.CacheKey("model1/ns1/variant1/custom-metrics")

			collector.cache.Set(key1, "data1", 0)
			collector.cache.Set(key2, "data2", 0)
			collector.cache.Set(key3, "data3", 0)

			collector.InvalidateCacheForVariant("model1", "ns1", "variant1")

			_, found1 := collector.cache.Get(key1)
			_, found2 := collector.cache.Get(key2)
			_, found3 := collector.cache.Get(key3)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeFalse())
			Expect(found3).To(BeFalse())
		})
	})

	Describe("InvalidateCacheForModel", func() {
		var (
			collector   *PrometheusCollector
			mockPromAPI *utils.MockPromAPI
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
				FetchInterval:   0,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, nil, testConfig)
		})

		It("should invalidate all cache entries for a model", func() {
			// Populate cache with entries for multiple variants
			key1 := cache.CacheKey("model1/ns1/variant1/allocation-metrics")
			key2 := cache.CacheKey("model1/ns1/variant2/allocation-metrics")
			key3 := cache.CacheKey("model1/ns1/all/replica-metrics")
			key4 := cache.CacheKey("model2/ns1/variant1/allocation-metrics")

			collector.cache.Set(key1, "data1", 0)
			collector.cache.Set(key2, "data2", 0)
			collector.cache.Set(key3, "data3", 0)
			collector.cache.Set(key4, "data4", 0)

			// Invalidate for model1
			collector.InvalidateCacheForModel("model1", "ns1")

			// model1 entries should be gone
			_, found1 := collector.cache.Get(key1)
			_, found2 := collector.cache.Get(key2)
			_, found3 := collector.cache.Get(key3)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeFalse())
			Expect(found3).To(BeFalse())

			// model2 entry should still exist
			_, found4 := collector.cache.Get(key4)
			Expect(found4).To(BeTrue())
		})

		It("should handle nil cache gracefully", func() {
			collector.cache = nil
			// Should not panic
			collector.InvalidateCacheForModel("model1", "ns1")
		})

		It("should invalidate entries across multiple namespaces for same model", func() {
			key1 := cache.CacheKey("model1/ns1/variant1/allocation-metrics")
			key2 := cache.CacheKey("model1/ns2/variant1/allocation-metrics")

			collector.cache.Set(key1, "data1", 0)
			collector.cache.Set(key2, "data2", 0)

			// Invalidate only for ns1
			collector.InvalidateCacheForModel("model1", "ns1")

			_, found1 := collector.cache.Get(key1)
			_, found2 := collector.cache.Get(key2)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeTrue()) // ns2 should still exist
		})
	})
})

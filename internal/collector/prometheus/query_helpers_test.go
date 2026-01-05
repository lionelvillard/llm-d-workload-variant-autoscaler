package prometheus

import (
	"context"
	"fmt"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("Query Helpers", func() {
	var (
		collector   *PrometheusCollector
		mockPromAPI *utils.MockPromAPI
		ctx         context.Context
	)

	BeforeEach(func() {
		ctx = ctrl.LoggerInto(context.Background(), logging.NewTestLogger())

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

	Describe("queryAndExtractMetric", func() {
		It("should extract float value from vector result", func() {
			query := "test_query"
			mockPromAPI.QueryResults[query] = model.Vector{
				&model.Sample{Value: model.SampleValue(42.5)},
			}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(42.5))
		})

		It("should return 0.0 for empty vector", func() {
			query := "test_query"
			mockPromAPI.QueryResults[query] = model.Vector{}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(0.0))
		})

		It("should return 0.0 for non-vector types", func() {
			query := "test_query"
			// Return a scalar instead of vector (use pointer for model.Value interface)
			scalar := &model.Scalar{Value: 42.5}
			mockPromAPI.QueryResults[query] = scalar

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(0.0))
		})

		It("should handle query errors", func() {
			query := "failing_query"
			mockPromAPI.QueryErrors[query] = fmt.Errorf("query failed")

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).To(HaveOccurred())
			Expect(result).To(Equal(0.0))
			Expect(err.Error()).To(ContainSubstring("failed to query Prometheus"))
		})

		It("should handle NaN values by converting to 0", func() {
			query := "nan_query"
			mockPromAPI.QueryResults[query] = model.Vector{
				&model.Sample{Value: model.SampleValue(math.NaN())},
			}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(0.0))
		})

		It("should handle positive infinity by converting to 0", func() {
			query := "inf_query"
			mockPromAPI.QueryResults[query] = model.Vector{
				&model.Sample{Value: model.SampleValue(math.Inf(1))},
			}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(0.0))
		})

		It("should handle negative infinity by converting to 0", func() {
			query := "neg_inf_query"
			mockPromAPI.QueryResults[query] = model.Vector{
				&model.Sample{Value: model.SampleValue(math.Inf(-1))},
			}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(0.0))
		})

		It("should use first value from vector with multiple samples", func() {
			query := "multi_sample_query"
			mockPromAPI.QueryResults[query] = model.Vector{
				&model.Sample{Value: model.SampleValue(10.0)},
				&model.Sample{Value: model.SampleValue(20.0)},
				&model.Sample{Value: model.SampleValue(30.0)},
			}

			result, err := collector.queryAndExtractMetric(ctx, query, "TestMetric")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(10.0)) // Should use first value
		})
	})

	Describe("fixValue", func() {
		It("should convert NaN to 0", func() {
			value := math.NaN()
			fixValue(&value)
			Expect(value).To(Equal(0.0))
		})

		It("should convert positive infinity to 0", func() {
			value := math.Inf(1)
			fixValue(&value)
			Expect(value).To(Equal(0.0))
		})

		It("should convert negative infinity to 0", func() {
			value := math.Inf(-1)
			fixValue(&value)
			Expect(value).To(Equal(0.0))
		})

		It("should leave valid numbers unchanged", func() {
			value := 42.5
			fixValue(&value)
			Expect(value).To(Equal(42.5))
		})

		It("should leave zero unchanged", func() {
			value := 0.0
			fixValue(&value)
			Expect(value).To(Equal(0.0))
		})

		It("should leave negative numbers unchanged", func() {
			value := -10.5
			fixValue(&value)
			Expect(value).To(Equal(-10.5))
		})
	})
})

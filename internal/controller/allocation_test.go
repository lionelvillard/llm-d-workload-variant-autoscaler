package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("Allocation", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)).To(Succeed())
	})

	Context("When adding metrics to optimization status", func() {
		var (
			mockProm      *utils.MockPromAPI
			deployment    appsv1.Deployment
			va            llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			name          string
			modelID       string
			testNamespace string
			accCost       float64
		)

		BeforeEach(func() {
			mockProm = &utils.MockPromAPI{
				QueryResults: make(map[string]model.Value),
				QueryErrors:  make(map[string]error),
			}

			name = "test"
			modelID = "default/default"
			testNamespace = "default"
			accCost = 40.0 // sample accelerator cost

			deployment = appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: func() *int32 { r := int32(2); return &r }(),
				},
			}

			va = llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "A100",
					},
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID: modelID,
				},
			}
		})

		It("should collect metrics successfully", func() {
			// Setup mock responses
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			avgPromptToksQuery := utils.CreatePromptToksQuery(modelID, testNamespace)
			avgDecToksQuery := utils.CreateDecToksQuery(modelID, testNamespace)
			ttftQuery := utils.CreateTTFTQuery(modelID, testNamespace)
			itlQuery := utils.CreateITLQuery(modelID, testNamespace)

			mockProm.QueryResults[arrivalQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.175)}, // 0.175 req/sec = 10.5 req/min after * 60
			}
			mockProm.QueryResults[avgPromptToksQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(100.0)}, // 100 input tokens per request
			}
			mockProm.QueryResults[avgDecToksQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(150.0)}, // 150 output tokens per request
			}
			mockProm.QueryResults[ttftQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.5)}, // 0.5 seconds
			}
			mockProm.QueryResults[itlQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.05)}, // 0.05 seconds
			}

			collector := prometheus.NewPrometheusCollectorWithConfig(mockProm, nil, nil)
			metrics, err := collector.AddMetricsToOptStatus(ctx, &va, deployment, accCost)
			Expect(err).NotTo(HaveOccurred())

			allocation, err := BuildAllocationFromMetrics(metrics, &va, deployment, accCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocation.Accelerator).To(Equal("A100"))
			Expect(allocation.NumReplicas).To(Equal(2))
			Expect(allocation.MaxBatch).To(Equal(256))
			Expect(allocation.VariantCost).To(Equal("80.00"))           // 2 replicas * 40.0 acc cost
			Expect(allocation.TTFTAverage).To(Equal("500.00"))          // 0.5 * 1000 ms
			Expect(allocation.ITLAverage).To(Equal("50.00"))            // 0.05 * 1000 ms
			Expect(allocation.Load.ArrivalRate).To(Equal("10.50"))      // req per min
			Expect(allocation.Load.AvgInputTokens).To(Equal("100.00"))  // input tokens per req
			Expect(allocation.Load.AvgOutputTokens).To(Equal("150.00")) // output tokens per req
		})

		It("should check missing accelerator label", func() {
			// Remove accelerator label
			delete(va.Labels, "inference.optimization/acceleratorName")

			// Setup minimal mock responses
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			tokenQuery := utils.CreateDecToksQuery(modelID, testNamespace)

			mockProm.QueryResults[arrivalQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(5.0)},
			}
			mockProm.QueryResults[tokenQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(100.0)},
			}

			collector := prometheus.NewPrometheusCollectorWithConfig(mockProm, nil, nil)
			metrics, err := collector.AddMetricsToOptStatus(ctx, &va, deployment, accCost)
			Expect(err).NotTo(HaveOccurred())

			allocation, err := BuildAllocationFromMetrics(metrics, &va, deployment, accCost)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing or empty acceleratorName label"))
			Expect(allocation).To(Equal(llmdVariantAutoscalingV1alpha1.Allocation{})) // Empty allocation on error
		})

		It("should handle Prometheus Query errors", func() {
			// Setup error for arrival Query
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			mockProm.QueryErrors[arrivalQuery] = fmt.Errorf("prometheus connection failed")

			collector := prometheus.NewPrometheusCollectorWithConfig(mockProm, nil, nil)
			_, err := collector.AddMetricsToOptStatus(ctx, &va, deployment, accCost)
			Expect(err).To(HaveOccurred())

			Expect(err.Error()).To(ContainSubstring("prometheus connection failed"))
		})

		It("should handle empty metric results gracefully", func() {
			// Setup empty responses (no data points)
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			tokenQuery := utils.CreateDecToksQuery(modelID, testNamespace)

			// Empty vectors (no data)
			mockProm.QueryResults[arrivalQuery] = model.Vector{}
			mockProm.QueryResults[tokenQuery] = model.Vector{}

			collector := prometheus.NewPrometheusCollectorWithConfig(mockProm, nil, nil)
			metrics, err := collector.AddMetricsToOptStatus(ctx, &va, deployment, accCost)
			Expect(err).NotTo(HaveOccurred())

			allocation, err := BuildAllocationFromMetrics(metrics, &va, deployment, accCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(allocation.ITLAverage).To(Equal("0.00"))
			Expect(allocation.TTFTAverage).To(Equal("0.00"))
			Expect(allocation.Load.ArrivalRate).To(Equal("0.00"))
			Expect(allocation.Load.AvgInputTokens).To(Equal("0.00"))
			Expect(allocation.Load.AvgOutputTokens).To(Equal("0.00"))
		})
	})

})

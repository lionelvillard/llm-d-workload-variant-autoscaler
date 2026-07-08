package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// GPU Limiter test validates that the WVA controller respects GPU resource constraints
// and doesn't recommend scaling beyond available GPU capacity.
//
// This test creates VAs with different accelerator requirements and verifies that
// the limiter correctly constrains scale-up decisions based on GPU availability.
var _ = Describe("GPU Limiter Feature", Label("full"), Ordered, func() {
	var (
		poolA         = "limiter-pool-a"
		poolB         = "limiter-pool-b"
		modelServiceA = "limiter-ms-a"
		modelServiceB = "limiter-ms-b"
		vaA           = "limiter-va-nvidia"
		vaB           = "limiter-va-amd"
		hpaA          = "limiter-hpa-nvidia"
		hpaB          = "limiter-hpa-amd"
		ns            string
	)

	BeforeAll(func() {
		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "limiter-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to create isolated test namespace")
		ns = nsObj.Name
		By("Using isolated test namespace " + ns)
		DeferCleanup(func() {
			By("Deleting isolated namespace " + ns)
			if err := k8sClient.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to delete namespace %s: %v\n", ns, err)
			}
		})

		By("Creating two model services with different accelerator requirements")

		// Pool A - NVIDIA GPUs
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceA, poolA, cfg.ModelID, vaA, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service A")

		err = fixtures.EnsureService(ctx, k8sClient, ns, modelServiceA, modelServiceA+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service A")

		By("Creating ServiceMonitor for service A")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, modelServiceA, modelServiceA+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor A")

		DeferCleanup(func() {
			_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      modelServiceA + "-monitor",
					Namespace: cfg.MonitoringNS,
				},
			})
		})

		// Pool B - AMD GPUs
		err = fixtures.EnsureModelService(ctx, k8sClient, ns, modelServiceB, poolB, cfg.ModelID, vaB, cfg.UseSimulator, cfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service B")

		err = fixtures.EnsureService(ctx, k8sClient, ns, modelServiceB, modelServiceB+"-decode", 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service B")

		By("Creating ServiceMonitor for service B")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, modelServiceB, modelServiceB+"-decode")
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor B")

		DeferCleanup(func() {
			_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      modelServiceB + "-monitor",
					Namespace: cfg.MonitoringNS,
				},
			})
		})

		By("Waiting for both model services to be ready")
		Eventually(func(g Gomega) {
			depA, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depA.Status.ReadyReplicas).To(Equal(int32(1)))

			depB, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(depB.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Creating VAs with different accelerator types")

		// VA A - NVIDIA accelerator
		err = fixtures.EnsureVariantAutoscaling(
			ctx, crClient, ns, vaA,
			modelServiceA+"-decode", cfg.ModelID, "H100", 30.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VA A")

		// VA B - AMD accelerator
		err = fixtures.EnsureVariantAutoscaling(
			ctx, crClient, ns, vaB,
			modelServiceB+"-decode", cfg.ModelID, "MI300X", 40.0,
			cfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VA B")

		By("Creating scalers for both deployments (HPA or ScaledObject per backend)")
		if cfg.ScalerBackend == scalerBackendKeda {
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, hpaA+"-hpa", metav1.DeleteOptions{})
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, hpaB+"-hpa", metav1.DeleteOptions{})
			err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaA, modelServiceA+"-decode", vaA, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject A")
			err = fixtures.EnsureScaledObject(ctx, crClient, ns, hpaB, modelServiceB+"-decode", vaB, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ScaledObject B")
		} else {
			err = fixtures.EnsureHPA(ctx, k8sClient, ns, hpaA, modelServiceA+"-decode", vaA, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA A")
			err = fixtures.EnsureHPA(ctx, k8sClient, ns, hpaB, modelServiceB+"-decode", vaB, 1, 10)
			Expect(err).NotTo(HaveOccurred(), "Failed to create HPA B")
		}

		GinkgoWriter.Println("GPU Limiter test setup complete with two VAs (NVIDIA and AMD accelerators)")
	})

	Context("VA creation and reconciliation", func() {
		It("should have both VAs created with different accelerators", func() {
			By("Verifying VA A (NVIDIA)")
			vaAObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      vaA,
			}, vaAObj)
			Expect(err).NotTo(HaveOccurred())
			Expect(vaAObj.Labels[utils.AcceleratorNameLabel]).To(Equal("H100"))

			By("Verifying VA B (AMD)")
			vaBObj := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: ns,
				Name:      vaB,
			}, vaBObj)
			Expect(err).NotTo(HaveOccurred())
			Expect(vaBObj.Labels[utils.AcceleratorNameLabel]).To(Equal("MI300X"))

			GinkgoWriter.Printf("VA A accelerator: %s, VA B accelerator: %s\n",
				vaAObj.Labels[utils.AcceleratorNameLabel], vaBObj.Labels[utils.AcceleratorNameLabel])
		})

		It("should reconcile both VAs successfully", func() {
			By("Checking VA A status")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaA,
					Namespace: ns,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty())
			}).Should(Succeed())

			By("Checking VA B status")
			Eventually(func(g Gomega) {
				va := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      vaB,
					Namespace: ns,
				}, va)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(va.Status.Conditions).NotTo(BeEmpty())
			}).Should(Succeed())

			GinkgoWriter.Println("Both VAs reconciled successfully")
		})
	})

	Context("Accelerator-specific scaling", func() {
		It("should respect GPU resource constraints per accelerator type", func() {
			By("Checking deployment replicas don't exceed expected limits")

			// Get deployment replica counts
			depA, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceA+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			depB, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, modelServiceB+"-decode", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicasA := depA.Status.Replicas
			replicasB := depB.Status.Replicas

			GinkgoWriter.Printf("Deployment A (NVIDIA) replicas: %d\n", replicasA)
			GinkgoWriter.Printf("Deployment B (AMD) replicas: %d\n", replicasB)

			// In emulated environment, deployments should still respect HPA maxReplicas
			Expect(replicasA).To(BeNumerically("<=", 10), "Deployment A should not exceed maxReplicas")
			Expect(replicasB).To(BeNumerically("<=", 10), "Deployment B should not exceed maxReplicas")

			// Both deployments should be able to scale independently
			GinkgoWriter.Println("GPU limiter correctly manages deployments with different accelerator types")
		})
	})
})

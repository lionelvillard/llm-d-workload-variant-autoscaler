/*
Copyright 2025.

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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("VariantAutoscalings Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		VariantAutoscalings := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required scale target ref deployment")
			deployment := testutils.CreateLlmdSimDeployment("default", resourceName, "default-default", "default", "8000", 0, 0, 1)
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("creating the required configmap for optimization")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(defaultConfigMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("creating the custom resource for the Kind VariantAutoscalings")
			err := k8sClient.Get(ctx, typeNamespacedName, VariantAutoscalings)
			if err != nil && errors.IsNotFound(err) {
				resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: resourceName,
						},
						// Example spec fields, adjust as necessary
						ModelID: "default-default",
						ModelProfile: llmdVariantAutoscalingV1alpha1.ModelProfile{
							Accelerators: []llmdVariantAutoscalingV1alpha1.AcceleratorProfile{
								{
									Acc:      "A100",
									AccCount: 1,
									PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
										DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
										PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
									},
									MaxBatchSize: 4,
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VariantAutoscalings")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "accelerator-unit-costs",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      defaultConfigMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}
			// Initialize MetricsCollector with mock Prometheus API
			metricsCollector := prometheus.NewPrometheusCollectorWithConfig(mockPromAPI, nil, nil)
			controllerReconciler := &VariantAutoscalingReconciler{
				Client:              k8sClient,
				Scheme:              k8sClient.Scheme(),
				PrometheusCollector: metricsCollector,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

		})
	})

	Context("When validating configurations", func() {
		const configResourceName = "config-test-resource"

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())

			configMap = testutils.CreateVariantAutoscalingConfigMap(defaultConfigMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "accelerator-unit-costs",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      defaultConfigMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should validate accelerator profiles", func() {
			By("Creating VariantAutoscaling with invalid accelerator profile")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configResourceName,
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: configResourceName,
					},
					ModelID: "default-default",
					ModelProfile: llmdVariantAutoscalingV1alpha1.ModelProfile{
						Accelerators: []llmdVariantAutoscalingV1alpha1.AcceleratorProfile{
							{
								Acc:      "INVALID_GPU",
								AccCount: -1, // Invalid count
								PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
									DecodeParms:  map[string]string{"alpha": "invalid", "beta": "invalid"},
									PrefillParms: map[string]string{"gamma": "invalid", "delta": "invalid"},
								},
								MaxBatchSize: -1, // Invalid batch size
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred()) // Expect validation error at API level
			Expect(err.Error()).To(ContainSubstring("Invalid value"))
		})

		It("should handle empty ModelID value", func() {
			By("Creating VariantAutoscaling with empty ModelID")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-model-id",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "invalid-model-id",
					},
					ModelID: "", // Empty ModelID
					ModelProfile: llmdVariantAutoscalingV1alpha1.ModelProfile{
						Accelerators: []llmdVariantAutoscalingV1alpha1.AcceleratorProfile{
							{
								Acc:      "A100",
								AccCount: 1,
								PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
									DecodeParms:  map[string]string{"alpha": "0.28", "beta": "0.72"},
									PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
								},
								MaxBatchSize: 4,
							},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred()) // Expect validation error at API level
			Expect(err.Error()).To(ContainSubstring("spec.modelID"))
		})

		It("should handle empty accelerator list", func() {
			By("Creating VariantAutoscaling with no accelerators")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-accelerators",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "empty-accelerators",
					},
					ModelID: "default-default",
					ModelProfile: llmdVariantAutoscalingV1alpha1.ModelProfile{
						Accelerators: []llmdVariantAutoscalingV1alpha1.AcceleratorProfile{
							// no configuration for accelerators
						},
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.modelProfile.accelerators"))
		})
	})

	Context("ServiceMonitor Watch", func() {
		var (
			controllerReconciler *VariantAutoscalingReconciler
			fakeRecorder         *record.FakeRecorder
		)

		BeforeEach(func() {
			logging.NewTestLogger()
			fakeRecorder = record.NewFakeRecorder(10)
			controllerReconciler = &VariantAutoscalingReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}
		})

		Context("handleServiceMonitorEvent", func() {
			It("should log and emit event when ServiceMonitor is being deleted", func() {
				By("Creating a ServiceMonitor with deletion timestamp")
				now := metav1.Now()
				serviceMonitor := &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{
						Name:              defaultServiceMonitorName,
						Namespace:         configMapNamespace,
						DeletionTimestamp: &now,
					},
				}

				By("Calling handleServiceMonitorEvent")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, serviceMonitor)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())

				By("Verifying event was emitted")
				select {
				case event := <-fakeRecorder.Events:
					Expect(event).To(ContainSubstring("ServiceMonitorDeleted"))
					Expect(event).To(ContainSubstring(defaultServiceMonitorName))
				case <-time.After(2 * time.Second):
					Fail("Expected event to be emitted but none was received")
				}
			})

			It("should not emit event when ServiceMonitor is created", func() {
				By("Creating a ServiceMonitor without deletion timestamp")
				serviceMonitor := &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{
						Name:      defaultServiceMonitorName,
						Namespace: configMapNamespace,
					},
				}

				By("Calling handleServiceMonitorEvent")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, serviceMonitor)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())

				By("Verifying no error event was emitted")
				Consistently(fakeRecorder.Events).ShouldNot(Receive(ContainSubstring("ServiceMonitorDeleted")))
			})

			It("should handle non-ServiceMonitor objects gracefully", func() {
				By("Creating a non-ServiceMonitor object")
				configMap := &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-configmap",
						Namespace: configMapNamespace,
					},
				}

				By("Calling handleServiceMonitorEvent with non-ServiceMonitor object")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, configMap)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())
			})
		})
	})

})

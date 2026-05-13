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

package controller

import (
	"context"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

func newHandlerReconciler() *VariantAutoscalingReconciler {
	return &VariantAutoscalingReconciler{
		Datastore: datastore.NewDatastore(config.NewTestConfig()),
	}
}

func TestHandleAnnotatedScalerEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("managed object tracks namespace", func(t *testing.T) {
		r := newHandlerReconciler()
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "hpa-a",
				Namespace:   "ns1",
				Annotations: map[string]string{annotations.Managed: "true"},
			},
		}
		r.handleAnnotatedScalerEvent(ctx, hpa)
		if !r.Datastore.IsNamespaceTracked("ns1") {
			t.Error("want ns1 tracked after managed object event")
		}
	})

	t.Run("deleted object untracks namespace", func(t *testing.T) {
		r := newHandlerReconciler()
		r.Datastore.NamespaceTrack("AnnotatedScaler", "hpa-a", "ns1")

		now := metav1.NewTime(time.Now())
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "hpa-a",
				Namespace:         "ns1",
				DeletionTimestamp: &now,
				Finalizers:        []string{"test"}, // required for DeletionTimestamp to be set
				Annotations:       map[string]string{annotations.Managed: "true"},
			},
		}
		r.handleAnnotatedScalerEvent(ctx, hpa)
		if r.Datastore.IsNamespaceTracked("ns1") {
			t.Error("want ns1 untracked after deletion event")
		}
	})

	t.Run("annotation removed untracks namespace", func(t *testing.T) {
		r := newHandlerReconciler()
		r.Datastore.NamespaceTrack("AnnotatedScaler", "hpa-a", "ns1")

		// send update event with annotation removed
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hpa-a",
				Namespace: "ns1",
				// no llm-d.ai/managed annotation
			},
		}
		r.handleAnnotatedScalerEvent(ctx, hpa)
		if r.Datastore.IsNamespaceTracked("ns1") {
			t.Error("want ns1 untracked after annotation removal")
		}
	})
}

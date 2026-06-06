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

package gpurebalance_test

import (
	"context"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance/signal"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/coordinator/plugins/gpurebalance/strategy"
)

// fixedBudget is a BudgetProvider that returns a constant value.
type fixedBudget struct{ b int }

func (f fixedBudget) TotalGPUBudget(_ context.Context) (int, error) { return f.b, nil }

// gpuStub returns a fixed GPUs/replica regardless of target.
type gpuStub struct{ gpus int }

func (g gpuStub) HPAGPUsPerReplica(_ context.Context, _ *autoscalingv2.HorizontalPodAutoscaler) (int, error) {
	return g.gpus, nil
}
func (g gpuStub) SOGPUsPerReplica(_ context.Context, _ *kedav1alpha1.ScaledObject) (int, error) {
	return g.gpus, nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

//nolint:unparam // helper kept name-flexible for future tests
func makeHPA(name string, currentMax int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns",
			Name:            name,
			ResourceVersion: "1",
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MaxReplicas:    currentMax,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name},
		},
	}
}

func makeSO(name string, currentMax *int32) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns",
			Name:            name,
			ResourceVersion: "1",
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			MaxReplicaCount: currentMax,
			ScaleTargetRef:  &kedav1alpha1.ScaleTarget{Kind: "Deployment", Name: name},
		},
	}
}

//nolint:unparam // helper kept gpus-flexible for future tests
func newPluginForTest(t *testing.T, c client.Client, gpusPerReplica int, budget int, minChange time.Duration) *gpurebalance.Plugin {
	t.Helper()
	p, err := gpurebalance.New(
		c,
		newScheme(t),
		record.NewFakeRecorder(10),
		fixedBudget{b: budget},
		signal.NewCurrentBound(),
		strategy.NewProportional(),
		gpuStub{gpus: gpusPerReplica},
		gpurebalance.Config{MinChangeInterval: minChange},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestTick_PatchesHPAMaxReplicas(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 5)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	p := newPluginForTest(t, c, 1, 8, 0)
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// With one HPA and budget 8 / GPUs/replica 1, target == budget == 8.
	if got.Spec.MaxReplicas != 8 {
		t.Errorf("expected MaxReplicas=8, got %d", got.Spec.MaxReplicas)
	}
}

func TestTick_PatchesScaledObjectMaxReplicaCount(t *testing.T) {
	scheme := newScheme(t)
	so := makeSO("s", ptr.To(int32(5)))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(so).Build()

	p := newPluginForTest(t, c, 1, 8, 0)
	if err := p.Tick(context.Background(), []client.Object{so}); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := &kedav1alpha1.ScaledObject{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(so), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.MaxReplicaCount == nil || *got.Spec.MaxReplicaCount != 8 {
		t.Errorf("expected MaxReplicaCount=8, got %v", got.Spec.MaxReplicaCount)
	}
}

func TestTick_IdempotentNoOp(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 8) // already at desired
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	p := newPluginForTest(t, c, 1, 8, 0)
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != hpa.ResourceVersion {
		t.Errorf("idempotent tick must not bump resourceVersion (was %s, now %s)", hpa.ResourceVersion, got.ResourceVersion)
	}
}

func TestTick_DampingSuppressesRepeatWrite(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 5)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	p := newPluginForTest(t, c, 1, 8, time.Hour) // long damping
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("first Tick: %v", err)
	}

	// Mutate the cluster HPA back to a different value to simulate
	// drift; a second tick within the damping window must skip.
	cur := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), cur); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cur.Spec.MaxReplicas = 5
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := p.Tick(context.Background(), []client.Object{cur}); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	after := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), after); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Spec.MaxReplicas != 5 {
		t.Errorf("damping should suppress repeat writes, expected 5, got %d", after.Spec.MaxReplicas)
	}
}

func TestTick_DropsTargetsWithoutGPUInfo(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 5)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	// gpus=0 should drop the HPA from scope without erroring.
	p, err := gpurebalance.New(
		c, scheme, record.NewFakeRecorder(10),
		fixedBudget{b: 8},
		signal.NewCurrentBound(),
		strategy.NewProportional(),
		gpuStub{gpus: 0},
		gpurebalance.Config{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != hpa.ResourceVersion {
		t.Errorf("target with gpus=0 should not be patched")
	}
}

func TestTick_RespectsCeilingAnnotation(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 5)
	hpa.Annotations = map[string]string{
		"coordinator.wva.llm-d.ai/max-max-replicas": "3",
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	p := newPluginForTest(t, c, 1, 8, 0)
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.MaxReplicas != 3 {
		t.Errorf("ceiling annotation should clamp to 3, got %d", got.Spec.MaxReplicas)
	}
}

func TestTick_ZeroBudgetIsNoop(t *testing.T) {
	scheme := newScheme(t)
	hpa := makeHPA("h", 5)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa).Build()

	p, err := gpurebalance.New(
		c, scheme, record.NewFakeRecorder(10),
		fixedBudget{b: 0},
		signal.NewCurrentBound(),
		strategy.NewProportional(),
		gpuStub{gpus: 1},
		gpurebalance.Config{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != hpa.ResourceVersion {
		t.Errorf("zero budget should be a no-op")
	}
}

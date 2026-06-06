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

package gpurebalance

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ScaleTargetGPULookup is a GPULookup that resolves the per-replica
// GPU count by fetching the workload pointed to by spec.scaleTargetRef
// and reading its pod template via scaletarget.FetchScaleTarget. It
// reuses the existing accessor logic that already handles Deployment
// and LeaderWorkerSet (leader_GPUs + (size-1) * worker_GPUs).
type ScaleTargetGPULookup struct {
	Client client.Client
}

// NewScaleTargetGPULookup wires a GPULookup against a cluster client.
func NewScaleTargetGPULookup(c client.Client) *ScaleTargetGPULookup {
	return &ScaleTargetGPULookup{Client: c}
}

// HPAGPUsPerReplica resolves the workload behind hpa.spec.scaleTargetRef
// and returns its per-replica GPU count, or 0 with no error when the
// reference does not point to a supported kind.
func (l *ScaleTargetGPULookup) HPAGPUsPerReplica(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler) (int, error) {
	ref := hpa.Spec.ScaleTargetRef
	return l.gpusFor(ctx, ref.Kind, ref.Name, hpa.Namespace)
}

// SOGPUsPerReplica resolves the workload behind so.spec.scaleTargetRef
// and returns its per-replica GPU count.
func (l *ScaleTargetGPULookup) SOGPUsPerReplica(ctx context.Context, so *kedav1alpha1.ScaledObject) (int, error) {
	ref := so.Spec.ScaleTargetRef
	if ref == nil {
		return 0, nil
	}
	return l.gpusFor(ctx, ref.Kind, ref.Name, so.Namespace)
}

func (l *ScaleTargetGPULookup) gpusFor(ctx context.Context, kind, name, namespace string) (int, error) {
	accessor, err := scaletarget.FetchScaleTarget(ctx, l.Client, "", kind, name, namespace)
	if err != nil {
		return 0, err
	}
	if accessor == nil {
		return 0, nil
	}
	return accessor.GetTotalGPUsPerReplica(), nil
}

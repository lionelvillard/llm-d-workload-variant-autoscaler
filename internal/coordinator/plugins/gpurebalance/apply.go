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
	"encoding/json"
	"fmt"
	"sync"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dampingCache tracks the most recent successful write per target so
// the plugin can suppress repeat writes within minChangeInterval.
type dampingCache struct {
	mu             sync.Mutex
	minInterval    time.Duration
	lastWriteByRef map[TargetRef]time.Time
	timeNow        func() time.Time // injectable for tests
}

// newDampingCache returns a cache that suppresses writes to the same
// target within minInterval. minInterval <= 0 disables damping.
func newDampingCache(minInterval time.Duration) *dampingCache {
	return &dampingCache{
		minInterval:    minInterval,
		lastWriteByRef: make(map[TargetRef]time.Time),
		timeNow:        time.Now,
	}
}

// allow reports whether a write to ref is allowed at this moment.
func (d *dampingCache) allow(ref TargetRef) bool {
	if d.minInterval <= 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.lastWriteByRef[ref]
	if !ok {
		return true
	}
	return d.timeNow().Sub(last) >= d.minInterval
}

// recordWrite stamps a successful write on ref.
func (d *dampingCache) recordWrite(ref TargetRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastWriteByRef[ref] = d.timeNow()
}

// applyHPA writes spec.maxReplicas using a JSON-merge patch with a
// resourceVersion precondition. Returns (patched, error). On 409
// Conflict, retries once. Idempotent: skips when the desired value
// equals the observed CurrentMaxReplicas.
func applyHPA(
	ctx context.Context,
	c client.Client,
	view ScaleTargetView,
	desired int32,
) (bool, error) {
	if view.CurrentMaxReplicas == desired {
		return false, nil
	}

	patch := mergePatchForMaxReplicas(view.ResourceVersion, "maxReplicas", int64(desired))
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       view.Ref.Namespace,
			Name:            view.Ref.Name,
			ResourceVersion: view.ResourceVersion,
		},
	}
	if err := c.Patch(ctx, hpa, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsConflict(err) {
			// Re-read and retry once with the freshest resourceVersion.
			fresh := &autoscalingv2.HorizontalPodAutoscaler{}
			if getErr := c.Get(ctx, types.NamespacedName{Namespace: view.Ref.Namespace, Name: view.Ref.Name}, fresh); getErr != nil {
				return false, fmt.Errorf("re-reading HPA after conflict: %w", getErr)
			}
			if fresh.Spec.MaxReplicas == desired {
				return false, nil
			}
			retryPatch := mergePatchForMaxReplicas(fresh.ResourceVersion, "maxReplicas", int64(desired))
			fresh.ResourceVersion = ""
			fresh.ObjectMeta = metav1.ObjectMeta{
				Namespace:       view.Ref.Namespace,
				Name:            view.Ref.Name,
				ResourceVersion: fresh.ResourceVersion,
			}
			if err := c.Patch(ctx, fresh, client.RawPatch(types.MergePatchType, retryPatch)); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// applyScaledObject writes spec.maxReplicaCount using a JSON-merge patch
// with a resourceVersion precondition. Returns (patched, error). On
// 409 Conflict, retries once. Idempotent: skips when the desired value
// equals the observed CurrentMaxReplicas.
func applyScaledObject(
	ctx context.Context,
	c client.Client,
	view ScaleTargetView,
	desired int32,
) (bool, error) {
	if view.CurrentMaxReplicas == desired {
		return false, nil
	}

	patch := mergePatchForMaxReplicas(view.ResourceVersion, "maxReplicaCount", int64(desired))
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       view.Ref.Namespace,
			Name:            view.Ref.Name,
			ResourceVersion: view.ResourceVersion,
		},
	}
	if err := c.Patch(ctx, so, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsConflict(err) {
			fresh := &kedav1alpha1.ScaledObject{}
			if getErr := c.Get(ctx, types.NamespacedName{Namespace: view.Ref.Namespace, Name: view.Ref.Name}, fresh); getErr != nil {
				return false, fmt.Errorf("re-reading ScaledObject after conflict: %w", getErr)
			}
			if fresh.Spec.MaxReplicaCount != nil && *fresh.Spec.MaxReplicaCount == desired {
				return false, nil
			}
			retryPatch := mergePatchForMaxReplicas(fresh.ResourceVersion, "maxReplicaCount", int64(desired))
			fresh.ObjectMeta = metav1.ObjectMeta{
				Namespace:       view.Ref.Namespace,
				Name:            view.Ref.Name,
				ResourceVersion: fresh.ResourceVersion,
			}
			if err := c.Patch(ctx, fresh, client.RawPatch(types.MergePatchType, retryPatch)); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// mergePatchForMaxReplicas builds a JSON-merge patch that updates a
// single integer field in spec and carries a resourceVersion
// precondition in metadata.
func mergePatchForMaxReplicas(resourceVersion, fieldName string, value int64) []byte {
	body := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": resourceVersion,
		},
		"spec": map[string]any{
			fieldName: value,
		},
	}
	out, _ := json.Marshal(body)
	return out
}

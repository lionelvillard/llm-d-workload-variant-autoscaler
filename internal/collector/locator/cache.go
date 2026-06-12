package locator

import (
	"fmt"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// defaultCacheSize is the size of the pod → top-level scale-target LRU.
// One entry is roughly 100 B of strings + a chainNode value; 4096 entries
// fit in well under a MB and cover typical clusters where the chain-node
// universe is a small multiple of variant count.
const defaultCacheSize = 4096

// defaultCacheTTL bounds how long a pod → top-level scale-target entry is
// trusted. The pod → owner relation is immutable for a *given* pod, but the
// cache is keyed by (namespace, name) and outlives the pod — and some workloads
// reuse pod names (StatefulSet pods, hence LWS group pods). A TTL caps the
// staleness window if a reused name later maps to a different scale target,
// without any per-entry invalidation logic.
const defaultCacheTTL = 10 * time.Minute

// podKey identifies a pod for cache purposes. Pods are uniquely named
// within a namespace, which is sufficient to key the immutable
// pod → top-level scale-target relation.
type podKey struct {
	Namespace, Name string
}

// resolutionCache memoizes pod → top-level scale-target resolution. The
// scale-target → managed scaler step is NOT cached; it always runs through
// the field index so annotation toggles and scaleTargetRef edits take
// effect on the next Locate call.
//
// Eviction is size-bounded LRU with a TTL (defaultCacheTTL). The pod → owner
// relation is immutable for a given pod, but entries are keyed by name and
// outlive the pod, so the TTL bounds staleness when a workload reuses a pod
// name for a different scale target (e.g. StatefulSet / LWS group pods).
type resolutionCache struct {
	c *expirable.LRU[podKey, chainNode]
}

func newResolutionCache(size int) (*resolutionCache, error) {
	if size <= 0 {
		return nil, fmt.Errorf("cache size must be > 0, got %d", size)
	}
	return &resolutionCache{c: expirable.NewLRU[podKey, chainNode](size, nil, defaultCacheTTL)}, nil
}

// get returns the cached top-level scale-target for the pod. The hit boolean
// is true even for negative entries (target == zero chainNode means the pod
// has no scaler-eligible ancestor).
func (r *resolutionCache) get(k podKey) (chainNode, bool) {
	return r.c.Get(k)
}

func (r *resolutionCache) add(k podKey, target chainNode) {
	r.c.Add(k, target)
}

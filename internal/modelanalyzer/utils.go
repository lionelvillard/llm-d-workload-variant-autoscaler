package modelanalyzer

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/types"
	inferno "github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/core"
)

// Adapter from inferno allocations to a model analyzer response
func CreateModelAnalyzeResponseFromAllocations(allocations map[string]*inferno.Allocation) *types.ModelAnalyzeResponse {
	responseAllocations := make(map[string]*types.ModelAcceleratorAllocation)

	for key, alloc := range allocations {
		responseAllocations[key] = &types.ModelAcceleratorAllocation{
			Allocation:         allocations[key],
			RequiredPrefillQPS: float64(alloc.MaxArrvRatePerReplica() * 1000),
			RequiredDecodeQPS:  float64(alloc.MaxArrvRatePerReplica() * 1000),
			Reason:             "markovian analysis",
		}
	}
	return &types.ModelAnalyzeResponse{
		Allocations: responseAllocations,
	}
}

// Adapter from a model analyzer response to inferno allocations
func CreateAllocationsFromModelAnalyzeResponse(response *types.ModelAnalyzeResponse) map[string]*inferno.Allocation {
	allocations := make(map[string]*inferno.Allocation)
	for key, alloc := range response.Allocations {
		if alloc.Allocation != nil {
			allocations[key] = alloc.Allocation
		}
	}
	return allocations
}

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

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// InventoryBudgetProvider wraps a pipeline.TypeInventory and exposes
// its TotalLimit() as the GPU budget. Each Tick triggers a fresh
// inventory refresh so the budget tracks node fleet changes.
type InventoryBudgetProvider struct {
	Inventory *pipeline.TypeInventory
}

// NewInventoryBudgetProvider returns a BudgetProvider that reads
// total cluster GPU capacity from the supplied TypeInventory.
func NewInventoryBudgetProvider(inv *pipeline.TypeInventory) *InventoryBudgetProvider {
	return &InventoryBudgetProvider{Inventory: inv}
}

// TotalGPUBudget refreshes the inventory and returns the total cluster
// GPU capacity across all accelerator types.
func (p *InventoryBudgetProvider) TotalGPUBudget(ctx context.Context) (int, error) {
	if err := p.Inventory.Refresh(ctx); err != nil {
		return 0, err
	}
	return p.Inventory.TotalLimit(), nil
}

package core

import "github.com/patriceckhart/zot/internal/provider"

// CostTracker accumulates usage across turns in a session.
type CostTracker struct {
	Total provider.Usage
}

// Add folds u into the running total and returns the new cumulative value.
func (c *CostTracker) Add(u provider.Usage) provider.Usage {
	c.Total = c.Total.Add(u)
	return c.Total
}

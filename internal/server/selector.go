package server

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dotnode/gatelm/internal/config"
)

// BackendSelector picks healthy backends from candidate lists using
// priority-based selection with weighted round-robin within the same priority.
type BackendSelector struct {
	health   *HealthManager
	counters sync.Map // groupKey(string) -> *atomic.Uint64
}

type candidateResult struct {
	backend                       *config.Backend
	backendModel                  string
	reasoningDefaultEffort        string
	systemPrompt                  string
	defaultMaxTokens              int
	normalizeXHighReasoningEffort bool
	defaultTemperature            *float64
}

func NewBackendSelector(health *HealthManager) *BackendSelector {
	return &BackendSelector{health: health}
}

// Select picks the best single healthy backend from the candidate list.
// Candidates must already be sorted by priority (ascending).
// Algorithm:
// 1. Group by priority
// 2. From highest priority group, filter healthy backends
// 3. Weighted round-robin among healthy backends in that group
// 4. If group is empty, fall through to next priority group
// 5. Returns nil if all backends are unhealthy.
func (bs *BackendSelector) Select(candidates []modelEntry) *candidateResult {
	if len(candidates) == 0 {
		return nil
	}

	groups := groupByPriority(candidates)
	for _, group := range groups {
		var healthy []modelEntry
		for _, c := range group {
			if bs.health.IsHealthy(c.backend.Name) {
				healthy = append(healthy, c)
			}
		}
		if len(healthy) == 0 {
			continue
		}
		if len(healthy) == 1 {
			return &candidateResult{
				backend:                       healthy[0].backend,
				backendModel:                  healthy[0].backendModel,
				reasoningDefaultEffort:        healthy[0].reasoningDefaultEffort,
				systemPrompt:                  healthy[0].systemPrompt,
				defaultMaxTokens:              healthy[0].defaultMaxTokens,
				normalizeXHighReasoningEffort: healthy[0].normalizeXHighReasoningEffort,
				defaultTemperature:            healthy[0].defaultTemperature,
			}
		}
		selected := bs.weightedRoundRobin(healthy)
		return &candidateResult{
			backend:                       selected.backend,
			backendModel:                  selected.backendModel,
			reasoningDefaultEffort:        selected.reasoningDefaultEffort,
			systemPrompt:                  selected.systemPrompt,
			defaultMaxTokens:              selected.defaultMaxTokens,
			normalizeXHighReasoningEffort: selected.normalizeXHighReasoningEffort,
			defaultTemperature:            selected.defaultTemperature,
		}
	}
	return nil
}

// SelectOrdered returns all healthy candidates ordered by priority then weight,
// suitable for retry iteration.
func (bs *BackendSelector) SelectOrdered(candidates []modelEntry) []candidateResult {
	if len(candidates) == 0 {
		return nil
	}

	var results []candidateResult
	groups := groupByPriority(candidates)
	for _, group := range groups {
		for _, c := range group {
			if bs.health.IsHealthy(c.backend.Name) {
				results = append(results, candidateResult{
					backend:                       c.backend,
					backendModel:                  c.backendModel,
					reasoningDefaultEffort:        c.reasoningDefaultEffort,
					systemPrompt:                  c.systemPrompt,
					defaultMaxTokens:              c.defaultMaxTokens,
					normalizeXHighReasoningEffort: c.normalizeXHighReasoningEffort,
					defaultTemperature:            c.defaultTemperature,
				})
			}
		}
	}
	return results
}

// SelectFallback returns all candidates ordered by priority, ignoring health status.
// Used when no healthy backend is available and there is no alternative to route to.
func (bs *BackendSelector) SelectFallback(candidates []modelEntry) []candidateResult {
	if len(candidates) == 0 {
		return nil
	}
	var results []candidateResult
	groups := groupByPriority(candidates)
	for _, group := range groups {
		for _, c := range group {
			results = append(results, candidateResult{
				backend:                       c.backend,
				backendModel:                  c.backendModel,
				reasoningDefaultEffort:        c.reasoningDefaultEffort,
				systemPrompt:                  c.systemPrompt,
				defaultMaxTokens:              c.defaultMaxTokens,
				normalizeXHighReasoningEffort: c.normalizeXHighReasoningEffort,
				defaultTemperature:            c.defaultTemperature,
			})
		}
	}
	return results
}

// groupByPriority splits a priority-sorted candidate list into groups.
func groupByPriority(candidates []modelEntry) [][]modelEntry {
	if len(candidates) == 0 {
		return nil
	}

	var groups [][]modelEntry
	current := []modelEntry{candidates[0]}
	currentPriority := candidates[0].backend.Priority

	for _, c := range candidates[1:] {
		if c.backend.Priority != currentPriority {
			groups = append(groups, current)
			current = []modelEntry{c}
			currentPriority = c.backend.Priority
		} else {
			current = append(current, c)
		}
	}
	groups = append(groups, current)
	return groups
}

// weightedRoundRobin selects one entry from candidates using weighted round-robin.
func (bs *BackendSelector) weightedRoundRobin(candidates []modelEntry) modelEntry {
	totalWeight := 0
	for _, c := range candidates {
		w := c.backend.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
	}

	key := buildGroupKey(candidates)
	counterVal, _ := bs.counters.LoadOrStore(key, &atomic.Uint64{})
	counter := counterVal.(*atomic.Uint64)
	n := counter.Add(1) - 1

	pos := int(n % uint64(totalWeight))
	for _, c := range candidates {
		w := c.backend.Weight
		if w <= 0 {
			w = 1
		}
		pos -= w
		if pos < 0 {
			return c
		}
	}
	return candidates[0]
}

func buildGroupKey(candidates []modelEntry) string {
	var b strings.Builder
	for _, c := range candidates {
		b.WriteString(c.backend.Name)
		b.WriteByte(',')
	}
	return b.String()
}

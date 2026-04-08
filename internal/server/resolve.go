package server

import (
	"sort"
	"strings"

	"github.com/dotnode/gatelm/internal/config"
)

type modelEntry struct {
	backend                       *config.Backend
	backendModel                  string
	reasoningDefaultEffort        string
	systemPrompt                  string
	defaultMaxTokens              int
	normalizeXHighReasoningEffort bool
	defaultTemperature            *float64
}

// ModelIndex provides O(1) model alias → backend resolution.
// Each alias may map to multiple backends (candidates), sorted by priority.
type ModelIndex struct {
	entries        map[string][]modelEntry
	defaultBackend *config.Backend
	prefixBackends []*config.Backend // backends with path_prefix set
}

// BuildModelIndex constructs the lookup index from config backends.
// Model names and their aliases are both registered as lookup keys.
// When the same alias exists in multiple backends, all are stored as candidates
// and sorted by backend priority (ascending = higher priority first).
func BuildModelIndex(backends []config.Backend) *ModelIndex {
	idx := &ModelIndex{
		entries: make(map[string][]modelEntry),
	}

	for i := range backends {
		b := &backends[i]

		if b.Default {
			idx.defaultBackend = b
		}

		if b.PathPrefix != "" {
			idx.prefixBackends = append(idx.prefixBackends, b)
		}

		for _, m := range b.Models {
			entry := modelEntry{
				backend:                       b,
				backendModel:                  m.Name,
				reasoningDefaultEffort:        m.ReasoningDefaultEffort,
				systemPrompt:                  m.SystemPrompt,
				defaultMaxTokens:              m.DefaultMaxTokens,
				normalizeXHighReasoningEffort: m.NormalizeXHighReasoningEffort,
				defaultTemperature:            m.DefaultTemperature,
			}

			// Register model name itself as a lookup key
			if m.Name != "" {
				idx.entries[m.Name] = append(idx.entries[m.Name], entry)
			}

			// Register all aliases
			for _, alias := range m.Aliases {
				alias = strings.TrimSpace(alias)
				if alias != "" {
					idx.entries[alias] = append(idx.entries[alias], entry)
				}
			}
		}
	}

	// Sort candidates by priority (lower number = higher priority)
	for key := range idx.entries {
		candidates := idx.entries[key]
		if len(candidates) > 1 {
			sort.SliceStable(candidates, func(i, j int) bool {
				return candidates[i].backend.Priority < candidates[j].backend.Priority
			})
			idx.entries[key] = candidates
		}
	}

	// If only one backend and no explicit default, treat it as default
	if idx.defaultBackend == nil && len(backends) == 1 {
		idx.defaultBackend = &backends[0]
	}

	return idx
}

// Resolve finds the backend and real model name for a given alias or model name.
// When multiple backends serve the same alias, returns the highest priority one.
func (idx *ModelIndex) Resolve(alias string) (backend *config.Backend, backendModel string, found bool) {
	if entries, ok := idx.entries[alias]; ok && len(entries) > 0 {
		return entries[0].backend, entries[0].backendModel, true
	}
	return nil, "", false
}

// ResolveCandidates returns all candidate backends for a given alias, sorted by priority.
func (idx *ModelIndex) ResolveCandidates(alias string) ([]modelEntry, bool) {
	entries, ok := idx.entries[alias]
	if !ok || len(entries) == 0 {
		return nil, false
	}
	return entries, true
}

// ResolveWithinBackend finds model entries for alias scoped to a specific backend.
func (idx *ModelIndex) ResolveWithinBackend(alias string, backend *config.Backend) ([]modelEntry, bool) {
	entries, ok := idx.entries[alias]
	if !ok || len(entries) == 0 || backend == nil {
		return nil, false
	}
	var matched []modelEntry
	for _, e := range entries {
		if e.backend == backend {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	return matched, true
}

// MatchByPrefix finds a backend by longest path prefix match.
func (idx *ModelIndex) MatchByPrefix(path string) (*config.Backend, bool) {
	var best *config.Backend
	bestLen := 0
	for _, b := range idx.prefixBackends {
		if pathPrefixMatches(path, b.PathPrefix) && len(canonicalPathPrefix(b.PathPrefix)) > bestLen {
			best = b
			bestLen = len(canonicalPathPrefix(b.PathPrefix))
		}
	}
	if best != nil {
		return best, true
	}
	return nil, false
}

// DefaultBackend returns the default backend, or nil if none is configured.
func (idx *ModelIndex) DefaultBackend() *config.Backend {
	return idx.defaultBackend
}

// AllAliases returns all registered model aliases (for model list endpoints).
func (idx *ModelIndex) AllAliases() []string {
	seen := make(map[string]bool)
	var result []string
	for alias := range idx.entries {
		if !seen[alias] {
			seen[alias] = true
			result = append(result, alias)
		}
	}
	return result
}

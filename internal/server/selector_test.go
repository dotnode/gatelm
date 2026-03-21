package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/dotnode/gatelm/internal/config"
	"github.com/dotnode/gatelm/internal/logging"
)

func newTestSelector() (*BackendSelector, *HealthManager) {
	hm := NewHealthManager(HealthManagerConfig{
		FailThreshold:   1,
		RecoveryTimeout: 30 * time.Second,
	}, http.DefaultClient, logging.NewDebugLog(false, ""))
	return NewBackendSelector(hm), hm
}

func TestSelectorSingleCandidate(t *testing.T) {
	sel, _ := newTestSelector()
	backends := []config.Backend{
		{Name: "a", Priority: 1, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
	}

	result := sel.Select(candidates)
	if result == nil {
		t.Fatal("expected a result")
	}
	if result.backend.Name != "a" {
		t.Fatalf("expected backend 'a', got %s", result.backend.Name)
	}
}

func TestSelectorPriorityOrder(t *testing.T) {
	sel, _ := newTestSelector()
	backends := []config.Backend{
		{Name: "secondary", Priority: 2, Weight: 1},
		{Name: "primary", Priority: 1, Weight: 1},
	}
	// Already sorted by priority
	candidates := []modelEntry{
		{backend: &backends[1], backendModel: "gpt-4"},
		{backend: &backends[0], backendModel: "gpt-4"},
	}

	result := sel.Select(candidates)
	if result == nil || result.backend.Name != "primary" {
		t.Fatalf("expected primary, got %v", result)
	}
}

func TestSelectorSkipUnhealthy(t *testing.T) {
	sel, hm := newTestSelector()
	backends := []config.Backend{
		{Name: "primary", Priority: 1, Weight: 1},
		{Name: "secondary", Priority: 2, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
	}

	// Mark primary as unhealthy
	hm.ReportFailure("primary")

	result := sel.Select(candidates)
	if result == nil || result.backend.Name != "secondary" {
		t.Fatalf("expected secondary (primary unhealthy), got %v", result)
	}
}

func TestSelectorAllUnhealthy(t *testing.T) {
	sel, hm := newTestSelector()
	backends := []config.Backend{
		{Name: "a", Priority: 1, Weight: 1},
		{Name: "b", Priority: 2, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
	}

	hm.ReportFailure("a")
	hm.ReportFailure("b")

	result := sel.Select(candidates)
	if result != nil {
		t.Fatalf("expected nil when all unhealthy, got %v", result.backend.Name)
	}
}

func TestSelectorWeightedRoundRobin(t *testing.T) {
	sel, _ := newTestSelector()
	backends := []config.Backend{
		{Name: "heavy", Priority: 1, Weight: 3},
		{Name: "light", Priority: 1, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
	}

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		result := sel.Select(candidates)
		if result == nil {
			t.Fatal("unexpected nil")
		}
		counts[result.backend.Name]++
	}

	// weight 3:1 → expect 30:10 over 40 iterations
	if counts["heavy"] != 30 {
		t.Errorf("expected heavy=30, got %d", counts["heavy"])
	}
	if counts["light"] != 10 {
		t.Errorf("expected light=10, got %d", counts["light"])
	}
}

func TestSelectorFallbackToPriority2(t *testing.T) {
	sel, hm := newTestSelector()
	backends := []config.Backend{
		{Name: "p1-a", Priority: 1, Weight: 1},
		{Name: "p1-b", Priority: 1, Weight: 1},
		{Name: "p2-a", Priority: 2, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
		{backend: &backends[2], backendModel: "gpt-4"},
	}

	// All priority-1 backends are down
	hm.ReportFailure("p1-a")
	hm.ReportFailure("p1-b")

	result := sel.Select(candidates)
	if result == nil || result.backend.Name != "p2-a" {
		t.Fatalf("expected fallback to p2-a, got %v", result)
	}
}

func TestSelectFallbackIgnoresHealth(t *testing.T) {
	sel, hm := newTestSelector()
	backends := []config.Backend{
		{Name: "a", Priority: 1, Weight: 1},
		{Name: "b", Priority: 2, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
	}

	// Mark all backends as unhealthy
	hm.ReportFailure("a")
	hm.ReportFailure("b")

	// SelectOrdered should return nothing
	ordered := sel.SelectOrdered(candidates)
	if len(ordered) != 0 {
		t.Fatalf("expected 0 healthy candidates, got %d", len(ordered))
	}

	// SelectFallback should return all candidates regardless of health
	fallback := sel.SelectFallback(candidates)
	if len(fallback) != 2 {
		t.Fatalf("expected 2 fallback candidates, got %d", len(fallback))
	}
	if fallback[0].backend.Name != "a" {
		t.Errorf("first fallback should be 'a' (priority 1), got %s", fallback[0].backend.Name)
	}
	if fallback[1].backend.Name != "b" {
		t.Errorf("second fallback should be 'b' (priority 2), got %s", fallback[1].backend.Name)
	}
}

func TestSelectOrdered(t *testing.T) {
	sel, hm := newTestSelector()
	backends := []config.Backend{
		{Name: "p1", Priority: 1, Weight: 1},
		{Name: "p2-a", Priority: 2, Weight: 1},
		{Name: "p2-b", Priority: 2, Weight: 1},
	}
	candidates := []modelEntry{
		{backend: &backends[0], backendModel: "gpt-4"},
		{backend: &backends[1], backendModel: "gpt-4"},
		{backend: &backends[2], backendModel: "gpt-4"},
	}

	// p1 is down
	hm.ReportFailure("p1")

	ordered := sel.SelectOrdered(candidates)
	if len(ordered) != 2 {
		t.Fatalf("expected 2 healthy candidates, got %d", len(ordered))
	}
	if ordered[0].backend.Name != "p2-a" {
		t.Errorf("first ordered candidate should be p2-a, got %s", ordered[0].backend.Name)
	}
	if ordered[1].backend.Name != "p2-b" {
		t.Errorf("second ordered candidate should be p2-b, got %s", ordered[1].backend.Name)
	}
}

func TestSelectorPreservesNormalizeXHighReasoningEffort(t *testing.T) {
	sel, _ := newTestSelector()
	backends := []config.Backend{{Name: "a", Priority: 1, Weight: 1}}
	candidates := []modelEntry{{
		backend:                       &backends[0],
		backendModel:                  "gpt-oss-120b",
		normalizeXHighReasoningEffort: true,
	}}

	picked := sel.Select(candidates)
	if picked == nil {
		t.Fatal("expected a result")
	}
	if !picked.normalizeXHighReasoningEffort {
		t.Fatal("expected normalizeXHighReasoningEffort to be preserved")
	}

	ordered := sel.SelectOrdered(candidates)
	if len(ordered) != 1 {
		t.Fatalf("expected 1 ordered candidate, got %d", len(ordered))
	}
	if !ordered[0].normalizeXHighReasoningEffort {
		t.Fatal("expected ordered candidate normalizeXHighReasoningEffort to be preserved")
	}

	fallback := sel.SelectFallback(candidates)
	if len(fallback) != 1 {
		t.Fatalf("expected 1 fallback candidate, got %d", len(fallback))
	}
	if !fallback[0].normalizeXHighReasoningEffort {
		t.Fatal("expected fallback candidate normalizeXHighReasoningEffort to be preserved")
	}
}

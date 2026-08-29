package proxy

import (
	"database/sql"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/provider"
)

// newLegacyHandler builds a Handler with legacy heuristic fallback enabled —
// the pre-strategy-routing behavior. Existing protocol/resilience harnesses
// register bare providers without lb rules and rely on heuristic resolution;
// strict no-route behavior (the default) is covered by lb_route_test.go.
func newLegacyHandler(ps *provider.Store, database *sql.DB) *Handler {
	h := New(ps, database)
	h.LegacyFallback = true
	return h
}

// newLegacyHandlerWithCatalog is newLegacyHandler with a catalog store wired.
func newLegacyHandlerWithCatalog(ps *provider.Store, cs *catalog.Store, database *sql.DB) *Handler {
	h := NewWithCatalog(ps, cs, database)
	h.LegacyFallback = true
	return h
}

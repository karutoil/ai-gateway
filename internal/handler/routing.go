package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/httperr"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/middleware"

	"github.com/go-chi/chi/v5"
)

// RoutingHandler manages operator-curated load-balancer rules:
// per-model ordered provider groups (round-robin across requests,
// deliberately NO within-request failover).
type RoutingHandler struct {
	LB         *lb.Store
	ProviderID func(r *http.Request) string // org scope from JWT context
	Role       func(r *http.Request) string
}

// Routes mounts admin-only endpoints under /lb.
func (h *RoutingHandler) Routes(r chi.Router) {
	r.Route("/lb", func(r chi.Router) {
		r.With(middleware.RequireRole("admin")).Get("/rules", h.ListRules)
		r.With(middleware.RequireRole("admin")).Put("/rules/{model}", h.PutRule)
		r.With(middleware.RequireRole("admin")).Delete("/rules/{model}", h.DeleteRule)
	})
}

func NewRoutingHandler(store *lb.Store) *RoutingHandler {
	return &RoutingHandler{
		LB:         store,
		ProviderID: auth.GetOrgID,
		Role:       auth.GetRole,
	}
}

func (h *RoutingHandler) ListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.LB.AllRules()
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, "failed to load routing rules", httperr.TypeProxy)
		return
	}
	if rules == nil {
		rules = []lb.Rule{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rules)
}

// PutRule replaces the ordered member set for a model. Org-scoped admins may
// only reference providers they own; global admins may use any provider.
func (h *RoutingHandler) PutRule(w http.ResponseWriter, r *http.Request) {
	model := chi.URLParam(r, "model")
	var body struct {
		Providers []string `json:"providers"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !strings.Contains(err.Error(), "EOF") {
		httperr.Invalid(w, "invalid json")
		return
	}
	if len(body.Providers) == 0 {
		httperr.Invalid(w, "providers required (ordered provider ids)")
		return
	}
	if len(body.Providers) > 50 {
		httperr.Invalid(w, "too many providers in rule (max 50)")
		return
	}
	orgID := h.ProviderID(r)
	if orgID != "" {
		for _, pid := range body.Providers {
			var cnt int
			if err := h.LB.DB.QueryRow(`SELECT COUNT(*) FROM providers WHERE id=? AND org_id=?`, pid, orgID).Scan(&cnt); err != nil || cnt == 0 {
				httperr.Forbidden(w, "provider not in your organization: "+pid)
				return
			}
		}
	}
	if err := h.LB.ReplaceRule(model, body.Providers); err != nil {
		httperr.Invalid(w, err.Error())
		return
	}
	rule := h.LB.RuleForModel(model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rule)
}

func (h *RoutingHandler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	model := chi.URLParam(r, "model")
	if model == "" {
		httperr.Invalid(w, "model required")
		return
	}
	if err := h.LB.DeleteRule(model); err != nil {
		httperr.Write(w, http.StatusInternalServerError, "delete failed", httperr.TypeProxy)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

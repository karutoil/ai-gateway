package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"ai-gateway/internal/discovery"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/rbac"
	"ai-gateway/internal/videogen"

	"github.com/go-chi/chi/v5"
)

type DiscoveryHandler struct {
	Service *discovery.Service
}

func (h *DiscoveryHandler) Routes(r chi.Router) {
	// Read routes stay open to authenticated users; every mutating route is
	// gated on catalog:write. This route table is not what cmd/gateway mounts
	// (it wires the handlers one by one with the same permissions), but any
	// future mounting must not inherit an unguarded mutation surface.
	r.Get("/", h.List)
	r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Post("/discover-all", h.DiscoverAll)
	r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Post("/", h.AddManual)
	r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Put("/{id}", h.Update)
	r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Post("/{id}/enrich", h.Enrich)
	r.With(middleware.RequirePerm(rbac.PermCatalogWrite)).Delete("/{id}", h.Delete)
}

func (h *DiscoveryHandler) List(w http.ResponseWriter, r *http.Request) {
	providerID := r.URL.Query().Get("provider_id")
	if providerID == "" {
		providerID = r.URL.Query().Get("provider")
	}
	q := r.URL.Query().Get("q")
	list, err := h.Service.List(providerID, q)
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if list == nil {
		list = []models.ProviderModel{}
	}
	// Derive the capability flags the dashboard needs to keep pure generators out
	// of the text modes. Computed here (not stored) so a catalog/modality change
	// is reflected immediately; the helpers are the same ones /v1/models uses.
	for i := range list {
		if list[i].Image {
			if list[i].Modalities != "" {
				list[i].ImageOnly = imagegen.ImageOnlyFromModalities(list[i].Modalities)
			} else {
				list[i].ImageOnly = imagegen.IsImageOnlyModel(list[i].ModelID)
			}
		}
		if list[i].Video {
			if list[i].Modalities != "" {
				list[i].VideoOnly = videogen.VideoOnlyFromModalities(list[i].Modalities)
			} else {
				list[i].VideoOnly = videogen.IsVideoOnlyModel(list[i].ModelID)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"data": list, "total": len(list)})
}

func (h *DiscoveryHandler) DiscoverProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	n, err := h.Service.Discover(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"discovered": n, "provider_id": id})
}

func (h *DiscoveryHandler) DiscoverAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.Service.DiscoverAll()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"discovered": n})
}

func (h *DiscoveryHandler) AddManual(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		ProviderID            string  `json:"provider_id"`
		ModelID               string  `json:"model_id"`
		DisplayName           string  `json:"display_name"`
		ContextWindow         int     `json:"context_window"`
		MaxOutput             int     `json:"max_output"`
		InputCost             float64 `json:"input_cost"`
		OutputCost            float64 `json:"output_cost"`
		Reasoning             bool    `json:"reasoning"`
		ReasoningType         string  `json:"reasoning_type"`
		ReasoningLevels       string  `json:"reasoning_levels"`
		ReasoningOutputLimits string  `json:"reasoning_output_limits"`
		ToolCall              bool    `json:"tool_call"`
		Attachment            bool    `json:"attachment"`
		Decision              bool    `json:"decision"`
		Image                 bool    `json:"image"`
		// Video was missing here while the model already carried it (the
		// UPDATE path decodes the whole ProviderModel, so it could set video
		// but POST could not). An operator could therefore never turn a
		// hand-added row into a video model through the dashboard.
		Video bool `json:"video"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProviderID == "" || body.ModelID == "" {
		http.Error(w, `{"error":"provider_id and model_id required"}`, 400)
		return
	}
	if len(body.ModelID) > 256 || len(body.ProviderID) > 64 {
		http.Error(w, `{"error":"id too long"}`, 400)
		return
	}
	m := models.ProviderModel{
		DisplayName:           body.DisplayName,
		ContextWindow:         body.ContextWindow,
		MaxOutput:             body.MaxOutput,
		InputCost:             body.InputCost,
		OutputCost:            body.OutputCost,
		Reasoning:             body.Reasoning,
		ReasoningType:         body.ReasoningType,
		ReasoningLevels:       body.ReasoningLevels,
		ReasoningOutputLimits: body.ReasoningOutputLimits,
		ToolCall:              body.ToolCall,
		Attachment:            body.Attachment,
		Decision:              body.Decision,
		Image:                 body.Image,
		Video:                 body.Video,
	}
	id, err := h.Service.AddManual(body.ProviderID, body.ModelID, m)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
}

func (h *DiscoveryHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body models.ProviderModel
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	if err := h.Service.UpdateManual(id, body); err != nil {
		http.Error(w, `{"error":"update failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *DiscoveryHandler) Enrich(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.Enrich(id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "enriched"})
}

func (h *DiscoveryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.Delete(id); err != nil {
		http.Error(w, `{"error":"delete failed"}`, 500)
		return
	}
	w.WriteHeader(204)
}

func (h *DiscoveryHandler) ListExcluded(w http.ResponseWriter, r *http.Request) {
	providerID := r.URL.Query().Get("provider_id")
	if providerID == "" {
		providerID = r.URL.Query().Get("provider")
	}
	list, err := h.Service.ListExcluded(providerID, r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if list == nil {
		list = []models.ExcludedModel{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"data": list, "total": len(list)})
}

func (h *DiscoveryHandler) Restore(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.Restore(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": newID, "status": "restored"})
}

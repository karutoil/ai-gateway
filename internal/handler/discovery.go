package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"ai-gateway/internal/discovery"
	"ai-gateway/internal/models"

	"github.com/go-chi/chi/v5"
)

type DiscoveryHandler struct {
	Service *discovery.Service
}

func (h *DiscoveryHandler) Routes(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/discover-all", h.DiscoverAll)
	r.Post("/", h.AddManual)
	r.Put("/{id}", h.Update)
	r.Post("/{id}/enrich", h.Enrich)
	r.Delete("/{id}", h.Delete)
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

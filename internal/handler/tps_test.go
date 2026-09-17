package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTpsForLog(t *testing.T) {
	cases := []struct {
		name       string
		latency    int64
		ttft       int64
		completion int
		want       float64
	}{
		{"streaming uses generation time", 1000, 200, 100, 125},
		{"non-streaming uses latency", 1000, 0, 100, 100},
		{"no completion is zero", 1000, 200, 0, 0},
		{"no latency is zero", 0, 0, 100, 0},
		{"ttft past latency falls back to latency", 200, 500, 100, 500},
	}
	for _, c := range cases {
		if got := tpsForLog(c.latency, c.ttft, c.completion); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestStatsTpsIgnoresPromptTokens(t *testing.T) {
	h, closer := newTestAdminHandler(t)
	defer closer()
	at := time.Now().UTC()
	// Large prompt, small completion: old total_tokens formula gave 20100
	// tok/s; output speed is 100 completion / 0.8s generation = 125.
	_, err := h.DB.Exec(`INSERT INTO request_logs(id,key_prefix,provider_id,model,endpoint,status,latency_ms,ttft_ms,created_at,prompt_tokens,completion_tokens,total_tokens,cost_usd,is_stream) VALUES('tps-1','pfx','prov-1','m','chat',200,1000,200,?,20000,100,20100,0,1)`, at.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.Stats(w, adminReq("/api/stats?range=7d"))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out["range_tps_avg"], 125.0; got != want {
		t.Errorf("range_tps_avg = %v want %v", got, want)
	}
}

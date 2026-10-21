package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/models"
)

// TestGrokImagineVideoIsNotAnImageModel is a P1 capability-isolation regression
// guard.
//
// imagegen's family fallback matches `strings.HasPrefix(s, "grok-imagine")`,
// which also matches xAI's *video* models ("grok-imagine-video",
// "grok-imagine-video-1.5"). Consequences found while building the isolation
// matrix:
//
//  1. `POST /v1/images/generations` with model "grok-imagine-video" is NOT
//     refused: the model is "known to be an image model", so rejectNonImageModelOnImages
//     returns false and the request is forwarded to the provider's
//     /images/generations endpoint — a video model served by the images API,
//     which is exactly the invariant the brief says must hold.
//  2. `POST /v1/chat/completions` with the same model is refused as
//     "an image-only model" (correct outcome, wrong reason), so a client is
//     told to use the images API for a video model.
//
// The test pins the CORRECT behaviour so the fix is verifiable; it is expected
// to fail until imagegen's grok family rule is narrowed to image models only.
func TestGrokImagineVideoIsNotAnImageModel(t *testing.T) {
	for _, id := range []string{"grok-imagine-video", "grok-imagine-video-1.5", "grok-imagine-video-1.5-lite"} {
		t.Run(id, func(t *testing.T) {
			var gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AAA"}]}`))
			}))
			defer upstream.Close()
			srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)

			resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
				`{"model":"xaip/`+id+`","prompt":"a cat"}`)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s on /v1/images/generations = %d (upstream %q), want 400: a video model must never be served by the images API\n%s",
					id, resp.StatusCode, gotPath, body)
			}
			if gotPath != "" {
				t.Errorf("%s reached the image upstream at %q", id, gotPath)
			}
			if !strings.Contains(string(body), "video") {
				t.Errorf("%s refusal should name the video endpoint: %s", id, body)
			}

			// On chat the refusal must be the *video* guard, not the image one.
			resp, body = postJSONImage(t, srv.URL+"/v1/chat/completions", key,
				`{"model":"xaip/`+id+`","messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s on chat = %d, want 400", id, resp.StatusCode)
			}
			if !strings.Contains(string(body), "video-generation model") {
				t.Errorf("%s on chat must be refused as a video model, got: %s", id, body)
			}
			if strings.Contains(string(body), "/v1/images/generations") {
				t.Errorf("%s on chat must not be pointed at the images API: %s", id, body)
			}
		})
	}
}

// TestGrokImageModelsStayImageModels is the positive half: the real Grok image
// models must keep resolving as image models after the fix.
func TestGrokImageModelsStayImageModels(t *testing.T) {
	for _, id := range []string{"grok-2-image", "grok-imagine-image", "grok-imagine-image-2.0", "grok-2-image-1212"} {
		var gotPath string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AAA"}]}`))
		}))
		srv, _, key := imageTestServer(t, "xaip", models.ProviderXAI, upstream)
		resp, body := postJSONImage(t, srv.URL+"/v1/images/generations", key,
			`{"model":"xaip/`+id+`","prompt":"a cat"}`)
		upstream.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s on /v1/images/generations = %d: %s", id, resp.StatusCode, body)
		}
		if !strings.HasSuffix(gotPath, "/images/generations") {
			t.Errorf("%s upstream path = %q", id, gotPath)
		}
	}
}

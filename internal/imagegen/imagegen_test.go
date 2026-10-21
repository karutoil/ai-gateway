package imagegen

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func pngBytes() []byte {
	// 1x1 transparent PNG.
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	return b
}

func dataURL(b []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

func TestParseRequestValid(t *testing.T) {
	body := `{"model":"gpt-image-1","prompt":"a cat","n":3,"size":"1536x1024","response_format":"b64_json","quality":"high"}`
	n, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Model != "gpt-image-1" || n.Prompt != "a cat" || n.N != 3 {
		t.Fatalf("unexpected: %+v", n)
	}
	if n.Size != "1536x1024" || n.ResponseFormat != "b64_json" || n.Quality != "high" {
		t.Fatalf("unexpected: %+v", n)
	}
	if n.AspectRatioFor() != "3:2" {
		t.Fatalf("aspect ratio = %q, want 3:2", n.AspectRatioFor())
	}
}

func TestParseRequestDefaultsN(t *testing.T) {
	n, err := ParseRequest([]byte(`{"model":"gpt-image-1","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if n.N != 1 {
		t.Fatalf("default n = %d, want 1", n.N)
	}
}

func TestParseRequestRejectsBadInput(t *testing.T) {
	cases := []struct {
		name, body, field string
	}{
		{"missing model", `{"prompt":"x"}`, "model"},
		{"empty model", `{"model":"","prompt":"x"}`, "model"},
		{"missing prompt", `{"model":"gpt-image-1"}`, "prompt"},
		{"blank prompt", `{"model":"gpt-image-1","prompt":"   "}`, "prompt"},
		{"n too large", `{"model":"m","prompt":"x","n":11}`, "n"},
		{"n zero", `{"model":"m","prompt":"x","n":0}`, "n"},
		{"bad size", `{"model":"m","prompt":"x","size":"99x99"}`, "size"},
		{"bad response_format", `{"model":"m","prompt":"x","response_format":"png"}`, "response_format"},
		{"not json", `nope`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(c.body))
			if err == nil {
				t.Fatal("expected error")
			}
			ve, ok := err.(*ValidateError)
			if !ok {
				t.Fatalf("expected ValidateError, got %T", err)
			}
			if c.field != "" && ve.Field != c.field {
				t.Fatalf("field = %q, want %q (%v)", ve.Field, c.field, err)
			}
		})
	}
}

func TestParseRequestDecodesReferenceImages(t *testing.T) {
	raw := pngBytes()
	body := `{"model":"gemini-2.5-flash-image","prompt":"edit","images":["` + dataURL(raw) + `",{"b64_json":"` + base64.StdEncoding.EncodeToString(raw) + `","mime_type":"image/jpeg"}]}`
	n, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(n.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(n.Images))
	}
	if n.Images[0].MimeType != "image/png" {
		t.Fatalf("mime[0] = %q", n.Images[0].MimeType)
	}
	if n.Images[1].MimeType != "image/jpeg" {
		t.Fatalf("mime[1] = %q (explicit mime must win)", n.Images[1].MimeType)
	}
	if string(n.Images[0].Data) != string(raw) {
		t.Fatal("decoded bytes mismatch")
	}
}

func TestParseRequestRejectsRemoteImageURL(t *testing.T) {
	_, err := ParseRequest([]byte(`{"model":"m","prompt":"x","image":"https://evil.example/a.png"}`))
	if err == nil || !strings.Contains(err.Error(), "remote image URLs") {
		t.Fatalf("expected remote-URL rejection, got %v", err)
	}
}

func TestLookupCoversFamilies(t *testing.T) {
	cases := map[string]Dialect{
		"gpt-image-1":                       DialectOpenAI,
		"openai/gpt-image-1":                DialectOpenAI,
		"dall-e-3":                          DialectOpenAI,
		"gemini-2.5-flash-image":            DialectGemini,
		"nano-banana":                       DialectGemini,
		"google/gemini-3-pro-image-preview": DialectGemini,
		"imagen-4.0-generate-001":           DialectImagen,
		"grok-2-image":                      DialectOpenAI,
		"xai/grok-imagine-image-2.0":        DialectOpenAI,
	}
	for id, want := range cases {
		m, ok := Lookup(id)
		if !ok {
			t.Errorf("%s: not found", id)
			continue
		}
		if m.Dialect != want {
			t.Errorf("%s: dialect = %q, want %q", id, m.Dialect, want)
		}
	}
	// Unknown versions of a known family resolve to the family default.
	m, ok := Lookup("gpt-image-9")
	if !ok || m.Dialect != DialectOpenAI {
		t.Fatalf("gpt-image-9 should resolve as an OpenAI image model, got %+v ok=%v", m, ok)
	}
	if _, ok := Lookup("gpt-4o"); ok {
		t.Fatal("gpt-4o must not be an image model")
	}
	if _, ok := Lookup("clef"); ok {
		t.Fatal("clef (decision model) must not be an image model")
	}
}

func TestIsImageModel(t *testing.T) {
	if !IsImageModel("gemini-2.5-flash-image") {
		t.Fatal("nano banana should be an image model")
	}
	if IsImageModel("gpt-4o-mini") {
		t.Fatal("chat model must not be an image model")
	}
}

func TestGeminiRequestBody(t *testing.T) {
	n := &Normalized{Model: "gemini-2.5-flash-image", Prompt: "a cat", N: 2, Images: []InputImage{{MimeType: "image/png", Data: pngBytes()}}}
	b, err := GeminiRequestBody(n)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	gc := req["generationConfig"].(map[string]any)
	mods := gc["responseModalities"].([]any)
	if len(mods) != 2 || mods[0] != "TEXT" || mods[1] != "IMAGE" {
		t.Fatalf("responseModalities = %v", mods)
	}
	if gc["candidateCount"].(float64) != 2 {
		t.Fatalf("candidateCount = %v", gc["candidateCount"])
	}
	contents := req["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want text + inlineData", len(parts))
	}
	inline := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" {
		t.Fatalf("inlineData mime = %v", inline["mimeType"])
	}
}

func TestGeminiRequestBodyAspectRatio(t *testing.T) {
	// An explicit aspect_ratio is honored.
	n := &Normalized{Prompt: "x", AspectRatio: "16:9"}
	b, _ := GeminiRequestBody(n)
	if !strings.Contains(string(b), `"aspectRatio":"16:9"`) {
		t.Fatalf("explicit aspect ratio missing: %s", b)
	}
	// A pixel `size` must NOT be turned into imageConfig: not every Gemini
	// image model accepts the field, and deriving one would 400 on those.
	n2 := &Normalized{Prompt: "x", Size: "1792x1024"}
	b2, _ := GeminiRequestBody(n2)
	if strings.Contains(string(b2), "imageConfig") {
		t.Fatalf("size must not synthesize imageConfig: %s", b2)
	}
}

func TestImagenRequestBodyRejectsReferenceImages(t *testing.T) {
	n := &Normalized{Prompt: "x", Images: []InputImage{{MimeType: "image/png", Data: pngBytes()}}}
	if _, err := ImagenRequestBody(n); err == nil {
		t.Fatal("imagen must reject reference images")
	}
}

func TestGeminiToShape(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"Here you go"},{"inlineData":{"mimeType":"image/png","data":"` + b64 + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":1290,"totalTokenCount":1301},"modelVersion":"gemini-2.5-flash-image"}`
	out := GeminiToOpenAI([]byte(body), "gemini-2.5-flash-image", "b64_json")
	if out == nil {
		t.Fatal("expected a translated response")
	}
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].B64JSON != b64 {
		t.Fatal("b64_json mismatch")
	}
	if resp.Data[0].MimeType != "image/png" {
		t.Fatalf("mime = %q", resp.Data[0].MimeType)
	}
	if resp.Data[0].RevisedPrompt != "Here you go" {
		t.Fatalf("revised_prompt = %q", resp.Data[0].RevisedPrompt)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 1290 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestGeminiToURLFormat(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	body := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"` + b64 + `"}}]}}]}`
	out := GeminiToOpenAI([]byte(body), "gemini-2.5-flash-image", "url")
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data[0].URL != "data:image/jpeg;base64,"+b64 {
		t.Fatalf("url = %q", resp.Data[0].URL)
	}
	if resp.Data[0].B64JSON != "" {
		t.Fatal("url format must not also carry b64_json")
	}
}

func TestGeminiToReturnsNilOnErrorBody(t *testing.T) {
	if out := GeminiToOpenAI([]byte(`{"error":{"code":400,"message":"bad"}}`), "m", "b64_json"); out != nil {
		t.Fatalf("expected nil for an error body, got %s", out)
	}
	if out := GeminiToOpenAI([]byte(`not json`), "m", "b64_json"); out != nil {
		t.Fatal("expected nil for non-JSON")
	}
}

func TestImagenToShape(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes())
	body := `{"predictions":[{"bytesBase64Encoded":"` + b64 + `","mimeType":"image/png"},{"bytesBase64Encoded":"` + b64 + `","mimeType":"image/png"}]}`
	out := ImagenToOpenAI([]byte(body), "imagen-4.0-generate-001", "b64_json")
	if out == nil {
		t.Fatal("expected translated response")
	}
	var resp Response
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data = %d, want 2", len(resp.Data))
	}
}

func TestImageCount(t *testing.T) {
	if n := ImageCount([]byte(`{"data":[{"b64_json":"a"},{"b64_json":"b"}]}`)); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	if n := ImageCount([]byte(`{}`)); n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}

func TestCostPerImage(t *testing.T) {
	if c, ok := CostPerImage("gpt-image-1"); !ok || c <= 0 {
		t.Fatalf("gpt-image-1 cost = %v ok=%v", c, ok)
	}
	if _, ok := CostPerImage("gpt-4o"); ok {
		t.Fatal("chat model must not have an image cost")
	}
}

func TestImageModality(t *testing.T) {
	if !ImageModality(`{"input":["text"],"output":["image"]}`) {
		t.Fatal("should detect image output modality")
	}
	if ImageModality(`{"input":["text"],"output":["text"]}`) {
		t.Fatal("text output is not image")
	}
	if ImageModality("") {
		t.Fatal("empty modalities")
	}
}

func TestRegistryNamespaces(t *testing.T) {
	for _, ns := range []string{"openai", "google", "xai"} {
		if len(ForNamespace(ns)) == 0 {
			t.Errorf("namespace %s has no curated models", ns)
		}
	}
	if len(ForNamespace("nope")) != 0 {
		t.Error("unknown namespace should be empty")
	}
}

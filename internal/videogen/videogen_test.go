package videogen

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func pngBytes() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	return b
}

func TestParseRequestValid(t *testing.T) {
	n, err := ParseRequest([]byte(`{"model":"sora-2","prompt":"a cat","seconds":"8","size":"1280x720"}`))
	if err != nil {
		t.Fatal(err)
	}
	if n.Model != "sora-2" || n.Prompt != "a cat" || n.Seconds != "8" || n.Size != "1280x720" {
		t.Fatalf("unexpected: %+v", n)
	}
}

// TestParseRequestDefaults locks the OpenAI defaults applied when omitted.
func TestParseRequestDefaults(t *testing.T) {
	n, err := ParseRequest([]byte(`{"model":"sora-2","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if n.Seconds != DefaultSeconds || n.Size != DefaultSize {
		t.Fatalf("defaults = %q/%q, want %q/%q", n.Seconds, n.Size, DefaultSeconds, DefaultSize)
	}
}

// TestParseRequestAcceptsNumericSeconds proves both the OpenAI string form and
// the numeric form some clients (and the xAI dialect) send are accepted.
func TestParseRequestAcceptsNumericSeconds(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","prompt":"x","seconds":8}`,
		`{"model":"m","prompt":"x","seconds":"8"}`,
	} {
		n, err := ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if n.Seconds != "8" {
			t.Fatalf("%s: seconds = %q, want \"8\"", body, n.Seconds)
		}
	}
}

func TestParseRequestRejectsBadInput(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"missing model", `{"prompt":"x"}`, "model"},
		{"empty model", `{"model":"","prompt":"x"}`, "model"},
		{"missing prompt", `{"model":"m"}`, "prompt"},
		{"blank prompt", `{"model":"m","prompt":"  "}`, "prompt"},
		{"bad seconds", `{"model":"m","prompt":"x","seconds":"7"}`, "seconds"},
		{"fractional seconds", `{"model":"m","prompt":"x","seconds":4.5}`, "seconds"},
		{"bad size", `{"model":"m","prompt":"x","size":"1920x1080"}`, "size"},
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

// TestNormalizeStatus is the regression guard for dialect status vocabularies:
// xAI reports "done" where OpenAI reports "completed", and an unfamiliar status
// must NOT be reported as a failure.
func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"queued": StatusQueued, "pending": StatusQueued, "submitted": StatusQueued,
		"in_progress": StatusInProgress, "processing": StatusInProgress, "running": StatusInProgress,
		"completed": StatusCompleted, "done": StatusCompleted, "succeeded": StatusCompleted, "success": StatusCompleted,
		"failed": StatusFailed, "error": StatusFailed, "cancelled": StatusFailed,
		"": StatusQueued,
		// Unknown vocabularies must be treated as still-running, never failed.
		"weird_new_state": StatusInProgress,
	}
	for in, want := range cases {
		if got := NormalizeStatus(in); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []string{StatusCompleted, StatusFailed} {
		if !IsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []string{StatusQueued, StatusInProgress} {
		if IsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestLookupCoversDialects(t *testing.T) {
	cases := map[string]Dialect{
		"sora-2":                     DialectOpenAI,
		"openai/sora-2-pro":          DialectOpenAI,
		"veo-3.1-generate-preview":   DialectVeo,
		"google/veo-3.1-fast":        DialectVeo,
		"grok-imagine-video":         DialectXAI,
		"xai/grok-imagine-video-1.5": DialectXAI,
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
	// Unknown versions resolve to the family default.
	if m, ok := Lookup("sora-3"); !ok || m.Dialect != DialectOpenAI {
		t.Fatalf("sora-3 should resolve as an OpenAI video model, got %+v ok=%v", m, ok)
	}
	// Non-video models must not resolve.
	for _, id := range []string{"gpt-4o", "dall-e-3", "clef", "gpt-image-1"} {
		if IsVideoModel(id) {
			t.Errorf("%s must not be a video model", id)
		}
	}
}

// TestVideoModelsAreVideoOnly locks the curated registry to the models.dev
// reality: every video model outputs video and no text.
func TestVideoModelsAreVideoOnly(t *testing.T) {
	for _, m := range All() {
		if m.TextOutput {
			t.Errorf("%s: TextOutput=true, but no curated video model emits text", m.ID)
		}
		if !IsVideoOnlyModel(m.ID) {
			t.Errorf("%s: should be video-only", m.ID)
		}
		if !VideoOnlyFromModalities(m.ModalitiesJSON()) {
			t.Errorf("%s: ModalitiesJSON not recognized as video-only: %s", m.ID, m.ModalitiesJSON())
		}
	}
	if IsVideoOnlyModel("gpt-4o") {
		t.Fatal("gpt-4o must not be video-only")
	}
}

func TestVideoOnlyFromModalities(t *testing.T) {
	cases := []struct {
		mods      string
		wantVideo bool
		wantOnly  bool
	}{
		{`{"input":["text"],"output":["video"]}`, true, true},
		{`{"input":["text","image"],"output":["video","pdf"]}`, true, true},
		{`{"input":["text"],"output":["text","video"]}`, true, false},
		{`{"input":["text"],"output":["text"]}`, false, false},
		{`{"input":["text"],"output":["image"]}`, false, false},
		{``, false, false},
		{`not json`, false, false},
	}
	for _, c := range cases {
		if got := VideoModality(c.mods); got != c.wantVideo {
			t.Errorf("VideoModality(%s) = %v, want %v", c.mods, got, c.wantVideo)
		}
		if got := VideoOnlyFromModalities(c.mods); got != c.wantOnly {
			t.Errorf("VideoOnlyFromModalities(%s) = %v, want %v", c.mods, got, c.wantOnly)
		}
	}
}

// TestCostPerSecond proves video bills by duration, not by a flat clip price.
func TestCostPerSecond(t *testing.T) {
	if c, ok := CostForModelSeconds("sora-2", "8"); !ok || c != 0.80 {
		t.Fatalf("sora-2 8s cost = %v ok=%v, want 0.80", c, ok)
	}
	if c, ok := CostForModelSeconds("sora-2", "4"); !ok || c != 0.40 {
		t.Fatalf("sora-2 4s cost = %v, want 0.40", c)
	}
	// Longer clip costs proportionally more.
	c4, _ := CostForModelSeconds("veo-3.1-generate-preview", "4")
	c8, _ := CostForModelSeconds("veo-3.1-generate-preview", "8")
	if !(c8 > c4) {
		t.Fatalf("8s (%v) should cost more than 4s (%v)", c8, c4)
	}
	if _, ok := CostForModelSeconds("gpt-4o", "8"); ok {
		t.Fatal("a chat model must not have a video cost")
	}
	if _, ok := CostForModelSeconds("sora-2", "abc"); ok {
		t.Fatal("unparseable seconds must not produce a cost")
	}
}

func TestXAICreateBody(t *testing.T) {
	n := &Normalized{Model: "grok-imagine-video", Prompt: "a cat", Seconds: "8", Size: "1280x720"}
	b, err := XAICreateBody(n)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if req["aspect_ratio"] != "16:9" || req["resolution"] != "720p" {
		t.Fatalf("xAI body = %v", req)
	}
	if req["seconds"] != "8" {
		t.Fatalf("seconds = %v", req["seconds"])
	}
}

func TestVeoCreateBody(t *testing.T) {
	n := &Normalized{Model: "veo-3.1-generate-preview", Prompt: "a cat", Seconds: "8", Size: "1280x720"}
	b, err := VeoCreateBody(n)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	instances := req["instances"].([]any)
	if len(instances) != 1 || instances[0].(map[string]any)["prompt"] != "a cat" {
		t.Fatalf("instances = %v", instances)
	}
	params := req["parameters"].(map[string]any)
	if params["durationSeconds"].(float64) != 8 || params["aspectRatio"] != "16:9" {
		t.Fatalf("parameters = %v", params)
	}
}

func TestVeoCreateBodyWithImage(t *testing.T) {
	ref, _ := json.Marshal("data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes()))
	n := &Normalized{Model: "veo-3.1-generate-preview", Prompt: "x", Seconds: "4", InputReference: ref}
	b, err := VeoCreateBody(n)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	inst := req["instances"].([]any)[0].(map[string]any)
	img, ok := inst["image"].(map[string]any)
	if !ok {
		t.Fatalf("reference image not inlined: %v", inst)
	}
	if img["mimeType"] != "image/png" || img["bytesBase64Encoded"] == "" {
		t.Fatalf("image = %v", img)
	}
}

// TestDecodeInputReference proves remote URLs are reported as non-inline
// (rather than rejected) so providers that accept URLs still work.
func TestDecodeInputReference(t *testing.T) {
	// data URL -> inline
	ref, _ := json.Marshal("data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes()))
	img, inline, err := DecodeInputReference(ref)
	if err != nil || !inline || img.MimeType != "image/png" {
		t.Fatalf("data URL: img=%+v inline=%v err=%v", img, inline, err)
	}
	// remote URL -> not inline, no error
	ref, _ = json.Marshal("https://example.com/a.png")
	_, inline, err = DecodeInputReference(ref)
	if err != nil || inline {
		t.Fatalf("remote URL should be non-inline without error: inline=%v err=%v", inline, err)
	}
	// object form
	ref, _ = json.Marshal(map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngBytes()), "mime_type": "image/jpeg"})
	img, inline, err = DecodeInputReference(ref)
	if err != nil || !inline || img.MimeType != "image/jpeg" {
		t.Fatalf("object form: img=%+v inline=%v err=%v", img, inline, err)
	}
	// malformed base64 -> error
	ref, _ = json.Marshal("!!!not base64!!!")
	if _, _, err := DecodeInputReference(ref); err == nil {
		t.Fatal("malformed base64 should error")
	}
}

func TestXAIRequestIDAndPoll(t *testing.T) {
	if id := XAIRequestID([]byte(`{"request_id":"abc-123"}`)); id != "abc-123" {
		t.Fatalf("request id = %q", id)
	}
	// xAI says "done"; the gateway must report "completed".
	body := []byte(`{"status":"done","model":"grok-imagine-video","video":{"url":"https://vidgen.x.ai/x.mp4","duration":6,"respect_moderation":true}}`)
	v := XAIPollToVideo(body, "video_1", "grok-imagine-video", "p", "8", "1280x720")
	if v == nil {
		t.Fatal("expected a video")
	}
	if v.Status != StatusCompleted || v.Progress != 100 {
		t.Fatalf("status=%q progress=%d, want completed/100", v.Status, v.Progress)
	}
	if XAIVideoURL(body) != "https://vidgen.x.ai/x.mp4" {
		t.Fatalf("url = %q", XAIVideoURL(body))
	}
}

// TestXAIPollModerationRejection proves a "done" job with no video is reported
// as a failure rather than a completed job with nothing to download.
func TestXAIPollModerationRejection(t *testing.T) {
	body := []byte(`{"status":"done","video":{"url":"","duration":6,"respect_moderation":false}}`)
	v := XAIPollToVideo(body, "id", "m", "p", "8", "1280x720")
	if v.Status != StatusFailed || v.Error == nil {
		t.Fatalf("expected failure, got %+v", v)
	}
}

func TestVeoOperationAndPoll(t *testing.T) {
	if name := VeoOperationName([]byte(`{"name":"models/veo-3.1/operations/abc"}`)); name != "models/veo-3.1/operations/abc" {
		t.Fatalf("operation name = %q", name)
	}
	// pending
	v := VeoPollToVideo([]byte(`{"name":"op","done":false}`), "id", "veo", "p", "8", "1280x720")
	if v.Status != StatusInProgress {
		t.Fatalf("pending status = %q", v.Status)
	}
	// done with a sample
	done := `{"name":"op","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://generativelanguage.googleapis.com/v1beta/files/x:download"}}]}}}`
	v = VeoPollToVideo([]byte(done), "id", "veo", "p", "8", "1280x720")
	if v.Status != StatusCompleted {
		t.Fatalf("done status = %q", v.Status)
	}
	if uri := VeoVideoURI([]byte(done)); !strings.Contains(uri, "files/x:download") {
		t.Fatalf("uri = %q", uri)
	}
	// error
	v = VeoPollToVideo([]byte(`{"name":"op","done":true,"error":{"code":3,"message":"bad prompt","status":"INVALID_ARGUMENT"}}`), "id", "veo", "p", "8", "1280x720")
	if v.Status != StatusFailed || v.Error == nil || v.Error.Message != "bad prompt" {
		t.Fatalf("error handling: %+v", v)
	}
}

func TestSizeMappings(t *testing.T) {
	if AspectRatioFor("1280x720") != "16:9" || AspectRatioFor("720x1280") != "9:16" {
		t.Fatal("aspect ratio mapping wrong")
	}
	if ResolutionFor("1280x720") != "720p" || ResolutionFor("1792x1024") != "1080p" {
		t.Fatal("resolution mapping wrong")
	}
	if AspectRatioFor("nope") != "" {
		t.Fatal("unknown size should map to empty")
	}
}

func TestVideoToJSONShape(t *testing.T) {
	v := &Video{ID: "video_1", Object: "video", Model: "sora-2", Status: StatusQueued, CreatedAt: 1}
	b := VideoToJSON(v)
	s := string(b)
	// The OpenAI schema serializes these as explicit nulls on a fresh job.
	for _, want := range []string{`"object":"video"`, `"status":"queued"`, `"completed_at":null`, `"expires_at":null`, `"error":null`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

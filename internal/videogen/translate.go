package videogen

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SizeToAspectRatio maps an OpenAI video size onto an aspect ratio. Used for the
// providers that take a ratio rather than pixel dimensions (Veo, xAI).
var SizeToAspectRatio = map[string]string{
	"1280x720":  "16:9",
	"720x1280":  "9:16",
	"1792x1024": "16:9",
	"1024x1792": "9:16",
}

// SizeToResolution maps an OpenAI video size onto xAI's resolution tiers.
var SizeToResolution = map[string]string{
	"1280x720":  "720p",
	"720x1280":  "720p",
	"1792x1024": "1080p",
	"1024x1792": "1080p",
}

// AspectRatioFor returns the aspect ratio for a request's size ("" when the
// size is unknown).
func AspectRatioFor(size string) string { return SizeToAspectRatio[size] }

// ResolutionFor returns the xAI resolution tier for a request's size.
func ResolutionFor(size string) string { return SizeToResolution[size] }

// InputImage is a decoded reference image for image-to-video.
type InputImage struct {
	MimeType string
	Data     []byte
}

// DecodeInputReference accepts the several shapes clients use for a reference
// frame: a data URL string, a bare base64 string, or an object with
// url/image_url/b64_json/data (+ optional mime_type). Remote http(s) URLs are
// rejected: the gateway does not fetch arbitrary user URLs server-side.
//
// It returns (image, true, nil) when a usable inline image was found,
// (zero, false, nil) when the reference is a remote URL (which some providers
// accept directly, so the caller forwards it rather than failing), and an
// error only for malformed input.
func DecodeInputReference(raw json.RawMessage) (InputImage, bool, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return InputImage{}, false, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return decodeRefString(s)
	}
	var obj struct {
		URL       string `json:"url"`
		ImageURL  string `json:"image_url"`
		B64JSON   string `json:"b64_json"`
		Data      string `json:"data"`
		MimeType  string `json:"mime_type"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return InputImage{}, false, fmt.Errorf("input_reference must be a data URL, base64 string, or image object")
	}
	mime := firstNonEmpty(obj.MimeType, obj.MediaType)
	switch {
	case obj.Data != "":
		return decodeB64Ref(stripDataURLPrefix(obj.Data), mime)
	case obj.B64JSON != "":
		return decodeB64Ref(stripDataURLPrefix(obj.B64JSON), mime)
	case obj.URL != "":
		return decodeRefString(obj.URL)
	case obj.ImageURL != "":
		return decodeRefString(obj.ImageURL)
	}
	return InputImage{}, false, fmt.Errorf("input_reference object requires url, b64_json, or data")
}

func decodeRefString(s string) (InputImage, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return InputImage{}, false, nil
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		// A remote reference is legitimate for providers that accept URLs;
		// the caller decides whether to forward it.
		return InputImage{}, false, nil
	}
	mime := ""
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		if i := strings.Index(s, ";"); i > 5 {
			mime = s[5:i]
		}
	}
	return decodeB64Ref(stripDataURLPrefix(s), mime)
}

func decodeB64Ref(b64, mime string) (InputImage, bool, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return InputImage{}, false, fmt.Errorf("input_reference is not valid base64")
	}
	return InputImage{MimeType: firstNonEmpty(mime, sniffMime(data), "image/png"), Data: data}, true, nil
}

func stripDataURLPrefix(s string) string {
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		if i := strings.Index(s, ","); i >= 0 {
			return s[i+1:]
		}
	}
	return s
}

func sniffMime(b []byte) string {
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// XAICreateBody renders an xAI Grok Imagine create request. xAI accepts
// `seconds` for OpenAI compatibility, plus aspect_ratio/resolution and an
// optional image reference.
//
// The body's `model` is n.Model, which the caller must have set to the model id
// actually being sent upstream. Pre-fix the handler passed the normalized
// request through unchanged, so an alias or an lb_rules model_override never
// reached the provider: the upstream ran (and billed) a different model than
// the one the gateway billed the client for.
func XAICreateBody(n *Normalized) ([]byte, error) {
	req := map[string]any{
		"model":   n.Model,
		"prompt":  n.Prompt,
		"seconds": n.Seconds,
	}
	if ar := AspectRatioFor(n.Size); ar != "" {
		req["aspect_ratio"] = ar
	}
	if res := ResolutionFor(n.Size); res != "" {
		req["resolution"] = res
	}
	if len(n.InputReference) > 0 {
		img, inline, err := DecodeInputReference(n.InputReference)
		if err != nil {
			return nil, err
		}
		if inline {
			req["image"] = map[string]any{
				"url": "data:" + img.MimeType + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
			}
		} else {
			// Remote URL: forward it verbatim (xAI fetches it).
			var s string
			if json.Unmarshal(n.InputReference, &s) == nil && s != "" {
				req["image"] = map[string]any{"url": s}
			} else {
				req["image"] = json.RawMessage(n.InputReference)
			}
		}
	}
	return json.Marshal(req)
}

// VeoCreateBody renders a Google Veo predictLongRunning request:
//
//	{"instances":[{"prompt":..., "image":{...}}], "parameters":{...}}
func VeoCreateBody(n *Normalized) ([]byte, error) {
	instance := map[string]any{"prompt": n.Prompt}
	if len(n.InputReference) > 0 {
		img, inline, err := DecodeInputReference(n.InputReference)
		if err != nil {
			return nil, err
		}
		if inline {
			instance["image"] = map[string]any{
				"bytesBase64Encoded": base64.StdEncoding.EncodeToString(img.Data),
				"mimeType":           img.MimeType,
			}
		} else {
			var s string
			if json.Unmarshal(n.InputReference, &s) == nil && s != "" {
				instance["image"] = map[string]any{"gcsUri": s, "mimeType": "image/png"}
			}
		}
	}
	params := map[string]any{}
	if n.Seconds != "" {
		if secs, ok := atoiStrict(n.Seconds); ok {
			params["durationSeconds"] = secs
		}
	}
	if ar := AspectRatioFor(n.Size); ar != "" {
		params["aspectRatio"] = ar
	}
	req := map[string]any{"instances": []any{instance}}
	if len(params) > 0 {
		req["parameters"] = params
	}
	return json.Marshal(req)
}

func atoiStrict(s string) (int, bool) {
	if strings.TrimSpace(s) == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// xaiCreateResponse is the xAI create reply: just a request id.
type xaiCreateResponse struct {
	RequestID string `json:"request_id"`
	ID        string `json:"id"`
}

// XAIRequestID extracts the poll id from an xAI create response.
func XAIRequestID(body []byte) string {
	var r xaiCreateResponse
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	return firstNonEmpty(r.RequestID, r.ID)
}

// XAIPollToVideo converts an xAI poll response into the OpenAI Video resource.
// xAI reports status "done" where OpenAI reports "completed", and nests the
// result under `video`.
//
// CompletedAt is deliberately NOT synthesized here: it is set exactly once, by
// the job state machine in the proxy, on the transition into a terminal state.
// Synthesizing time.Now() per poll made completed_at drift on every poll and
// rewrote the stored row each time.
func XAIPollToVideo(body []byte, id, model, prompt, seconds, size string) *Video {
	var r struct {
		Status   string `json:"status"`
		Progress *int   `json:"progress"`
		Model    string `json:"model"`
		Video    *struct {
			URL               string `json:"url"`
			Duration          int    `json:"duration"`
			RespectModeration bool   `json:"respect_moderation"`
		} `json:"video"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	v := &Video{
		ID:        id,
		Object:    "video",
		Model:     firstNonEmpty(r.Model, model),
		Status:    NormalizeStatus(r.Status),
		CreatedAt: time.Now().Unix(),
		Size:      size,
		Seconds:   seconds,
		Prompt:    prompt,
	}
	if r.Progress != nil {
		v.Progress = *r.Progress
	}
	if v.Status == StatusCompleted {
		v.Progress = 100
	}
	if r.Error != nil && (r.Error.Code != "" || r.Error.Message != "") {
		v.Status = StatusFailed
		v.Error = &VideoError{Code: r.Error.Code, Message: r.Error.Message}
	}
	// A "completed" job that carries no video and no error is a moderation
	// rejection: xAI sets respect_moderation=false and leaves url empty.
	if v.Status == StatusCompleted && (r.Video == nil || r.Video.URL == "") {
		v.Status = StatusFailed
		v.Error = &VideoError{Code: "moderation_rejected", Message: "the provider did not return a video (content moderation or an empty result)"}
	}
	return v
}

// XAIVideoURL extracts the finished video URL from an xAI poll response.
func XAIVideoURL(body []byte) string {
	var r struct {
		Video *struct {
			URL string `json:"url"`
		} `json:"video"`
	}
	if json.Unmarshal(body, &r) != nil || r.Video == nil {
		return ""
	}
	return r.Video.URL
}

// veoOperation is a Google long-running operation envelope.
type veoOperation struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Response *struct {
		GenerateVideoResponse *struct {
			GeneratedSamples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	Metadata json.RawMessage `json:"metadata"`
}

// VeoOperationName extracts the operation name from a predictLongRunning
// response.
func VeoOperationName(body []byte) string {
	var op veoOperation
	if json.Unmarshal(body, &op) != nil {
		return ""
	}
	return op.Name
}

// VeoPollToVideo converts a Veo operation into the OpenAI Video resource. The
// operation reports only done/not-done, so progress is synthesized (0 while
// pending, 100 when done) — OpenAI clients expect a percentage and Veo does not
// provide one.
//
// A `done` operation that carries no generated sample is NOT a success: Veo
// signals content-policy refusals and empty results as done + no
// generatedSamples, so reporting "completed" produced a job that answered 200
// on every poll and then 502'd forever on the content route with no state
// change. It is reported as failed with an actionable message (mirroring xAI's
// moderation_rejected handling).
//
// CompletedAt is deliberately NOT synthesized here: the proxy's job state
// machine sets it exactly once, on the transition into a terminal state.
func VeoPollToVideo(body []byte, id, model, prompt, seconds, size string) *Video {
	var op veoOperation
	if json.Unmarshal(body, &op) != nil {
		return nil
	}
	v := &Video{
		ID:        id,
		Object:    "video",
		Model:     model,
		CreatedAt: time.Now().Unix(),
		Size:      size,
		Seconds:   seconds,
		Prompt:    prompt,
	}
	switch {
	case op.Error != nil && (op.Error.Message != "" || op.Error.Code != 0):
		v.Status = StatusFailed
		v.Error = &VideoError{Code: op.Error.Status, Message: op.Error.Message}
	case op.Done:
		if uri := veoOperationVideoURI(&op); uri != "" {
			v.Status = StatusCompleted
			v.Progress = 100
			break
		}
		// done, but nothing to download: a terminal provider-side outcome, not
		// a transient state. Staying in_progress would keep the client polling
		// a job that can never complete and re-hitting the provider forever.
		v.Status = StatusFailed
		v.Progress = 100
		v.Error = &VideoError{
			Code:    "no_output",
			Message: "the provider finished the operation without returning a video (content moderation, an unsupported prompt, or an empty result); retry with a different prompt",
		}
	default:
		v.Status = StatusInProgress
	}
	return v
}

// veoOperationVideoURI extracts the first generated sample's URI from an
// already-decoded operation ("" when the operation carries none).
func veoOperationVideoURI(op *veoOperation) string {
	if op == nil || op.Response == nil || op.Response.GenerateVideoResponse == nil {
		return ""
	}
	samples := op.Response.GenerateVideoResponse.GeneratedSamples
	if len(samples) == 0 {
		return ""
	}
	return samples[0].Video.URI
}

// VeoVideoURI extracts the generated video URI from a finished Veo operation.
func VeoVideoURI(body []byte) string {
	var op veoOperation
	if json.Unmarshal(body, &op) != nil {
		return ""
	}
	return veoOperationVideoURI(&op)
}

// VideoToJSON marshals a Video resource.
func VideoToJSON(v *Video) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// SecondsToInt parses an OpenAI seconds string to an int (0 when unparseable).
func SecondsToInt(s string) int {
	n, _ := atoiStrict(s)
	return n
}

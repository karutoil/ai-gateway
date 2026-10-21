// Package videogen implements the OpenAI-compatible Videos API at the gateway
// and translates it to the native video-generation dialects of the upstream
// providers the gateway supports.
//
// The standardized endpoints are:
//
//	POST   /v1/videos              create a generation job (JSON)
//	GET    /v1/videos/{id}         poll the job
//	GET    /v1/videos/{id}/content download the finished video bytes
//	GET    /v1/videos              list jobs
//	DELETE /v1/videos/{id}         forget a job
//
// WHY ASYNC: video generation takes tens of seconds to minutes — far longer
// than any sane HTTP request budget, and far longer than the chat first-byte
// window. So the API is job-based: create returns immediately with a job in
// state "queued", the client polls the job until it reaches a terminal state,
// and the bytes are fetched from a dedicated content endpoint. A blocking
// request would either time out or pin a connection for minutes. The same
// reason rules out returning the video inline: a clip is megabytes, so base64
// in a JSON body is impractical.
//
// The wire shape is the OpenAI Videos API (job resource with
// status/progress/seconds/size), so official SDKs work by changing baseURL.
// The gateway speaks whichever dialect the target provider needs:
//
//   - openai: POST {base}/videos — OpenAI, Azure, and OpenAI-compatible resellers.
//     Passthrough.
//   - xai:    POST {base}/videos/generations -> {request_id}, then
//     GET {base}/videos/{request_id}. xAI reports status "done"; the gateway
//     normalizes that to the OpenAI "completed".
//   - veo:    POST {base}/v1beta/models/{model}:predictLongRunning -> an
//     operation name, then GET {base}/v1beta/{operation} until done, then
//     download the generated sample's URI.
//
// The package is deliberately dependency-free (standard library only) so the
// proxy, discovery and catalog layers can share the curated registry without
// an import cycle.
package videogen

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Dialect identifies the upstream wire protocol used to generate a video.
type Dialect string

const (
	// DialectOpenAI is the OpenAI Videos API (POST {base}/videos). Azure and
	// OpenAI-compatible resellers speak it too.
	DialectOpenAI Dialect = "openai"
	// DialectXAI is xAI's Grok Imagine video API
	// (POST {base}/videos/generations -> request_id, GET {base}/videos/{id}).
	DialectXAI Dialect = "xai"
	// DialectVeo is Google's long-running video operation
	// (POST {base}/v1beta/models/{model}:predictLongRunning -> operation).
	DialectVeo Dialect = "veo"
)

// Job lifecycle states, matching the OpenAI Videos API exactly.
const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// IsTerminal reports whether a status ends the job's lifecycle.
func IsTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed
}

// Protocol limits, mirroring the upstream schemas so malformed requests fail
// fast at the gateway with a 400 naming the field.
const (
	// MaxPromptLen bounds the prompt in BYTES, not characters. The error names
	// bytes so a multi-byte UTF-8 prompt is not rejected at a third of the
	// advertised "character" length.
	MaxPromptLen = 32000
	// MaxContentBytes bounds how much video content the gateway will relay.
	MaxContentBytes = 512 << 20 // 512 MiB
)

// Request is the OpenAI-compatible video-creation request body.
type Request struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Seconds string `json:"seconds,omitempty"`
	Size    string `json:"size,omitempty"`
	// InputReference is an optional reference image (data URL / base64 /
	// object) guiding generation. Providers that support image-to-video use it.
	InputReference json.RawMessage `json:"input_reference,omitempty"`
	// Remix/extend extensions, passed through where supported.
	RemixVideoID string `json:"remixed_from_video_id,omitempty"`
}

// VideoError explains why a job failed.
type VideoError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Video is the OpenAI Videos job resource.
type Video struct {
	ID        string `json:"id"`
	Object    string `json:"object"` // always "video"
	Model     string `json:"model"`
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	CreatedAt int64  `json:"created_at"`
	Size      string `json:"size,omitempty"`
	Seconds   string `json:"seconds,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	// Pointer fields serialize as null when unset, which is what the OpenAI
	// schema does (completed_at / expires_at / error are explicitly null on a
	// fresh job).
	CompletedAt        *int64      `json:"completed_at"`
	ExpiresAt          *int64      `json:"expires_at"`
	Error              *VideoError `json:"error"`
	RemixedFromVideoID string      `json:"remixed_from_video_id,omitempty"`
}

// ValidateError is a 400-class request validation failure naming the field.
type ValidateError struct{ Field, Msg string }

func (e *ValidateError) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

func invalid(field, msg string) error { return &ValidateError{Field: field, Msg: msg} }

// validSeconds / validSizes are the OpenAI Videos vocabulary. Unknown values are
// rejected rather than forwarded, because providers disagree on what an
// unsupported value means (some 400, some silently default).
var (
	validSeconds = map[string]bool{"4": true, "8": true, "12": true}
	validSizes   = map[string]bool{
		"720x1280":  true,
		"1280x720":  true,
		"1024x1792": true,
		"1792x1024": true,
	}
)

// DefaultSeconds / DefaultSize are the OpenAI defaults applied when omitted.
const (
	DefaultSeconds = "4"
	DefaultSize    = "720x1280"
)

// Normalized is the parsed, dialect-independent view of a create request.
type Normalized struct {
	Model          string
	Prompt         string
	Seconds        string
	Size           string
	InputReference json.RawMessage
	RemixVideoID   string
}

// ParseRequest validates and normalizes an OpenAI Videos create body.
func ParseRequest(body []byte) (*Normalized, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, invalid("", "body must be a valid JSON object")
	}
	var model string
	if raw, ok := top["model"]; !ok || json.Unmarshal(raw, &model) != nil || strings.TrimSpace(model) == "" {
		return nil, invalid("model", "required non-empty string")
	}
	var prompt string
	if raw, ok := top["prompt"]; !ok || json.Unmarshal(raw, &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return nil, invalid("prompt", "required non-empty string")
	}
	if len(prompt) > MaxPromptLen {
		return nil, invalid("prompt", fmt.Sprintf("exceeds %d bytes", MaxPromptLen))
	}
	n := &Normalized{Model: strings.TrimSpace(model), Prompt: prompt, Seconds: DefaultSeconds, Size: DefaultSize}
	if raw, ok := top["seconds"]; ok && len(raw) > 0 && string(raw) != "null" {
		s, err := secondsValue(raw)
		if err != nil {
			return nil, err
		}
		if !validSeconds[s] {
			return nil, invalid("seconds", "must be 4, 8, or 12")
		}
		n.Seconds = s
	}
	if raw, ok := top["size"]; ok && len(raw) > 0 && string(raw) != "null" {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if !validSizes[strings.ToLower(s)] {
				return nil, invalid("size", "unsupported size "+s+" (expected 720x1280, 1280x720, 1024x1792 or 1792x1024)")
			}
			n.Size = strings.ToLower(s)
		}
	}
	if raw, ok := top["input_reference"]; ok && len(raw) > 0 && string(raw) != "null" {
		n.InputReference = raw
	}
	if raw, ok := top["remixed_from_video_id"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			n.RemixVideoID = s
		}
	}
	return n, nil
}

// secondsValue accepts both the OpenAI string form ("8") and the numeric form
// (8) that some clients and the xAI dialect send, normalizing to a string.
func secondsValue(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s), nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		if f != float64(int(f)) {
			return "", invalid("seconds", "must be a whole number of seconds")
		}
		return fmt.Sprintf("%d", int(f)), nil
	}
	return "", invalid("seconds", "must be 4, 8, or 12")
}

// Validate enforces the create-request rules. It mirrors the upstream schemas
// so malformed requests fail fast at the gateway instead of burning an
// upstream round trip on an opaque error.
func Validate(body []byte) error {
	_, err := ParseRequest(body)
	return err
}

// BaseName reduces a provider-qualified model id to its bare selector:
//
//	"xai/grok-imagine-video" -> "grok-imagine-video"
//	"veo-3.1-generate-preview" -> "veo-3.1-generate-preview"
func BaseName(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// ParseVideo decodes a Video resource, tolerating the null-heavy OpenAI shape.
func ParseVideo(body []byte) (*Video, error) {
	var v Video
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// NormalizeStatus maps a provider's status vocabulary onto the OpenAI states.
// xAI reports "done" where OpenAI reports "completed", and various resellers use
// "succeeded"/"processing"/"pending". Unknown values become "in_progress"
// rather than "failed": a job that is merely described in an unfamiliar
// vocabulary must not be reported as broken.
func NormalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "queued", "pending", "created", "submitted", "starting":
		return StatusQueued
	case "in_progress", "processing", "running", "generating":
		return StatusInProgress
	case "completed", "complete", "done", "succeeded", "success", "finished":
		return StatusCompleted
	case "failed", "failure", "error", "cancelled", "canceled", "rejected":
		return StatusFailed
	case "":
		return StatusQueued
	default:
		return StatusInProgress
	}
}

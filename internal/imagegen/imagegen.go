// Package imagegen implements the OpenAI-compatible Images API at the gateway
// and translates it to the native image-generation dialects of the upstream
// providers the gateway supports.
//
// The standardized endpoints are:
//
//	POST /v1/images/generations   (JSON)
//	POST /v1/images/edits         (multipart/form-data)
//
// The wire shape is the OpenAI Images API, so any OpenAI SDK (or the xAI /
// Azure images clients) works by changing baseURL and using a gateway
// sk-gw-* key. The gateway accepts one request shape and speaks whichever
// dialect the target provider needs:
//
//   - openai:  POST {base}/images/generations  — OpenAI, Azure, xAI Grok, and
//     any OpenAI-compatible reseller. Passthrough.
//   - gemini:  POST {base}/v1beta/models/{model}:generateContent with
//     generationConfig.responseModalities = ["TEXT","IMAGE"] — Google's
//     Gemini image models, a.k.a. Nano Banana. Inline base64 parts.
//   - imagen:  POST {base}/v1beta/models/{model}:predict — Google's Imagen
//     family (instances/parameters).
//
// The package is deliberately dependency-free (it imports only the standard
// library) so the proxy, discovery and catalog layers can all share the
// curated model registry without an import cycle.
package imagegen

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Dialect identifies the upstream wire protocol used to generate an image.
type Dialect string

const (
	// DialectOpenAI is the OpenAI Images API (POST {base}/images/generations).
	// xAI Grok and Azure deployments speak this dialect too.
	DialectOpenAI Dialect = "openai"
	// DialectGemini is Google's generateContent endpoint with image response
	// modalities (Nano Banana / gemini-*-image).
	DialectGemini Dialect = "gemini"
	// DialectImagen is Google's Imagen :predict endpoint.
	DialectImagen Dialect = "imagen"
)

// Protocol limits. They mirror the upstream schemas so malformed requests
// fail fast at the gateway with a 400 naming the field.
const (
	MaxImagesPerRequest = 10
	// MaxPromptLen bounds the prompt in BYTES, not characters. The OpenAI Images
	// schema talks in characters, but the gateway enforces a byte budget (the
	// thing that actually bounds memory) and says so in the error — labelling a
	// byte count "characters" made a 3-byte UTF-8 prompt hit the limit at a
	// third of the advertised length.
	MaxPromptLen = 32000
	// MaxReferenceImages bounds inline reference images (edits / image-to-image).
	MaxReferenceImages = 4
)

// Request is the OpenAI-compatible Images API request body. Not every provider
// honors every field; the translator keeps the ones the target dialect
// understands and drops the rest.
type Request struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	Style          string `json:"style,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Background     string `json:"background,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	// Grok/Gemini extensions.
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	// Reference images for image-to-image / edits. `image` is a single entry
	// (string data URL or object), `images` is an array of the same.
	Image  json.RawMessage   `json:"image,omitempty"`
	Images []json.RawMessage `json:"images,omitempty"`
	Mask   json.RawMessage   `json:"mask,omitempty"`
	User   string            `json:"user,omitempty"`
}

// ImageDatum is one generated image. Providers return either a hosted URL or
// inline base64; the gateway normalizes both.
type ImageDatum struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
	MimeType      string `json:"mime_type,omitempty"`
}

// Usage mirrors the OpenAI Images usage object. Image models may bill by image
// or by token; both are surfaced so the gateway can meter either way.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Response is the normalized Images API response the gateway returns for every
// dialect.
type Response struct {
	Created int64        `json:"created"`
	Model   string       `json:"model,omitempty"`
	Data    []ImageDatum `json:"data"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// InputImage is a decoded reference image (edits / image-to-image).
type InputImage struct {
	MimeType string
	Data     []byte
}

// Normalized is the parsed, dialect-independent view of an Images request.
type Normalized struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	Quality        string
	Style          string
	ResponseFormat string
	AspectRatio    string
	Resolution     string
	Images         []InputImage
	Mask           *InputImage
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

// validSizes is the OpenAI Images size vocabulary plus the aspect-ratio presets
// used by Grok/Gemini. Unknown sizes are rejected rather than forwarded.
var validSizes = map[string]bool{
	"auto":      true,
	"256x256":   true,
	"512x512":   true,
	"1024x1024": true,
	"1536x1024": true,
	"1024x1536": true,
	"1792x1024": true,
	"1024x1792": true,
	"2048x2048": true,
	"2048x1152": true,
	"1152x2048": true,
	"4096x4096": true,
}

// ValidSize reports whether size belongs to the Images size vocabulary. It is
// exported so the multipart edit path can apply the SAME rule as the JSON path
// instead of maintaining a second, drifting list.
func ValidSize(size string) bool {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return true // omitted is valid; the provider picks its default
	}
	return validSizes[s]
}

// ValidResponseFormat reports whether response_format is one of the two
// documented values ("url" or "b64_json"). Empty means "provider default" and
// is valid.
func ValidResponseFormat(format string) bool {
	switch strings.TrimSpace(format) {
	case "", "url", "b64_json":
		return true
	}
	return false
}

// sizeToAspectRatio maps an OpenAI size to a Gemini/Grok aspect ratio. It is
// only a hint: callers pass an explicit aspect_ratio through untouched.
var sizeToAspectRatio = map[string]string{
	"1024x1024": "1:1",
	"1536x1024": "3:2",
	"1024x1536": "2:3",
	"1792x1024": "16:9",
	"1024x1792": "9:16",
	"2048x2048": "1:1",
	"2048x1152": "16:9",
	"1152x2048": "9:16",
	"4096x4096": "1:1",
}

// Validate enforces the OpenAI Images request rules. It mirrors the upstream
// schemas so malformed requests fail fast at the gateway instead of burning an
// upstream round trip on an opaque error.
func Validate(body []byte) error {
	_, err := ParseRequest(body)
	return err
}

// ParseRequest validates and normalizes an OpenAI Images JSON body.
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
	n := 1
	if raw, ok := top["n"]; ok && len(raw) > 0 && string(raw) != "null" {
		var num int
		if err := json.Unmarshal(raw, &num); err != nil {
			return nil, invalid("n", "must be an integer")
		}
		if num < 1 || num > MaxImagesPerRequest {
			return nil, invalid("n", fmt.Sprintf("must be between 1 and %d", MaxImagesPerRequest))
		}
		n = num
	}
	nf := &Normalized{Model: strings.TrimSpace(model), Prompt: prompt, N: n}
	if raw, ok := top["size"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if !validSizes[strings.ToLower(s)] {
				return nil, invalid("size", "unsupported size "+s)
			}
			nf.Size = strings.ToLower(s)
		}
	}
	if raw, ok := top["response_format"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			switch s {
			case "url", "b64_json":
				nf.ResponseFormat = s
			default:
				return nil, invalid("response_format", "must be \"url\" or \"b64_json\"")
			}
		}
	}
	if raw, ok := top["quality"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			nf.Quality = s
		}
	}
	if raw, ok := top["style"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			nf.Style = s
		}
	}
	if raw, ok := top["aspect_ratio"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			nf.AspectRatio = s
		}
	}
	if raw, ok := top["resolution"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			nf.Resolution = s
		}
	}
	refs, err := decodeReferences(top)
	if err != nil {
		return nil, err
	}
	nf.Images = refs
	if raw, ok := top["mask"]; ok && len(raw) > 0 && string(raw) != "null" {
		img, err := decodeImage(raw)
		if err != nil {
			return nil, invalid("mask", err.Error())
		}
		nf.Mask = &img
	}
	return nf, nil
}

// decodeReferences collects `image` (single) and `images` (array) into a
// decoded list, bounding the count.
func decodeReferences(top map[string]json.RawMessage) ([]InputImage, error) {
	var raws []json.RawMessage
	if raw, ok := top["image"]; ok && len(raw) > 0 && string(raw) != "null" {
		raws = append(raws, raw)
	}
	if raw, ok := top["images"]; ok && len(raw) > 0 && string(raw) != "null" {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, invalid("images", "must be an array of images")
		}
		raws = append(raws, arr...)
	}
	if len(raws) > MaxReferenceImages {
		return nil, invalid("images", fmt.Sprintf("at most %d reference images are allowed", MaxReferenceImages))
	}
	out := make([]InputImage, 0, len(raws))
	for i, raw := range raws {
		img, err := decodeImage(raw)
		if err != nil {
			return nil, invalid(fmt.Sprintf("images[%d]", i), err.Error())
		}
		out = append(out, img)
	}
	return out, nil
}

// decodeImage accepts the several shapes clients use for inline images:
// a data URL string, a bare base64 string, or an object with
// url/image_url/b64_json/data (+ optional mime_type/media_type). Remote
// http(s) URLs are rejected: the gateway does not fetch arbitrary user URLs
// server-side.
func decodeImage(raw json.RawMessage) (InputImage, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return decodeImageString(s)
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
		return InputImage{}, fmt.Errorf("must be a data URL, base64 string, or image object")
	}
	mime := firstNonEmpty(obj.MimeType, obj.MediaType)
	switch {
	case obj.Data != "":
		data, err := base64.StdEncoding.DecodeString(stripDataURLPrefix(obj.Data))
		if err != nil {
			return InputImage{}, fmt.Errorf("invalid base64 image data")
		}
		return InputImage{MimeType: firstNonEmpty(mime, sniffMime(data), "image/png"), Data: data}, nil
	case obj.B64JSON != "":
		data, err := base64.StdEncoding.DecodeString(stripDataURLPrefix(obj.B64JSON))
		if err != nil {
			return InputImage{}, fmt.Errorf("invalid base64 image data")
		}
		return InputImage{MimeType: firstNonEmpty(mime, sniffMime(data), "image/png"), Data: data}, nil
	case obj.URL != "":
		return decodeImageString(obj.URL)
	case obj.ImageURL != "":
		return decodeImageString(obj.ImageURL)
	}
	return InputImage{}, fmt.Errorf("image object requires url, b64_json, or data")
}

func decodeImageString(s string) (InputImage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return InputImage{}, fmt.Errorf("image must not be empty")
	}
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		meta, data, ok := splitDataURL(s)
		if !ok {
			return InputImage{}, fmt.Errorf("malformed data URL")
		}
		mime := "image/png"
		if i := strings.Index(meta, ";"); i >= 0 {
			mime = meta[:i]
		} else if meta != "" {
			mime = meta
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return InputImage{}, fmt.Errorf("invalid base64 image data")
		}
		return InputImage{MimeType: firstNonEmpty(mime, sniffMime(decoded), "image/png"), Data: decoded}, nil
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return InputImage{}, fmt.Errorf("remote image URLs are not supported; inline the image as a data URL or base64")
	}
	decoded, err := base64.StdEncoding.DecodeString(stripDataURLPrefix(s))
	if err != nil {
		return InputImage{}, fmt.Errorf("image must be a data URL or base64 string")
	}
	return InputImage{MimeType: firstNonEmpty(sniffMime(decoded), "image/png"), Data: decoded}, nil
}

// splitDataURL splits "data:<meta>,<payload>".
func splitDataURL(s string) (meta, data string, ok bool) {
	i := strings.Index(s, ",")
	if i < 0 {
		return "", "", false
	}
	return s[len("data:"):i], s[i+1:], true
}

func stripDataURLPrefix(s string) string {
	if i := strings.Index(s, ","); strings.HasPrefix(strings.ToLower(s), "data:") && i >= 0 {
		return s[i+1:]
	}
	return s
}

// sniffMime detects png/jpeg/webp/gif from magic bytes; "" when unknown.
func sniffMime(b []byte) string {
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 6 && string(b[0:6]) == "GIF89a":
		return "image/gif"
	case len(b) >= 6 && string(b[0:6]) == "GIF87a":
		return "image/gif"
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

// AspectRatioFor returns the aspect ratio for a request, preferring an
// explicit aspect_ratio over one derived from size. Empty when neither maps.
func (n *Normalized) AspectRatioFor() string {
	if n.AspectRatio != "" {
		return n.AspectRatio
	}
	return sizeToAspectRatio[n.Size]
}

// BaseName reduces a provider-qualified model id to its bare selector:
//
//	"google/gemini-2.5-flash-image" -> "gemini-2.5-flash-image"
//	"gemini-2.5-flash-image"        -> "gemini-2.5-flash-image"
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

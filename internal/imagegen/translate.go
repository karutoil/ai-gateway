package imagegen

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GeminiRequestBody translates a normalized Images request into a Google
// generateContent body:
//
//	{"contents":[{"role":"user","parts":[{"text":...},{"inlineData":{...}}]}],
//	 "generationConfig":{"responseModalities":["TEXT","IMAGE"], ...}}
//
// Reference images (edits / image-to-image) ride along as inlineData parts.
func GeminiRequestBody(n *Normalized) ([]byte, error) {
	parts := make([]map[string]any, 0, 1+len(n.Images))
	if strings.TrimSpace(n.Prompt) != "" {
		parts = append(parts, map[string]any{"text": n.Prompt})
	}
	for _, img := range n.Images {
		parts = append(parts, map[string]any{"inlineData": map[string]any{
			"mimeType": img.MimeType,
			"data":     base64.StdEncoding.EncodeToString(img.Data),
		}})
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("prompt or reference image required")
	}
	gen := map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}
	if n.N > 1 {
		gen["candidateCount"] = n.N
	}
	// Only send imageConfig when the client explicitly asked for an aspect
	// ratio: not every Gemini image model accepts the field, and silently
	// deriving one from a pixel `size` would 400 on the models that do not.
	if ar := strings.TrimSpace(n.AspectRatio); ar != "" && ar != "auto" {
		gen["imageConfig"] = map[string]any{"aspectRatio": ar}
	}
	req := map[string]any{
		"contents":         []map[string]any{{"role": "user", "parts": parts}},
		"generationConfig": gen,
	}
	return json.Marshal(req)
}

// ImagenRequestBody translates a normalized Images request into an Imagen
// :predict body ({"instances":[{"prompt":...}],"parameters":{...}}).
func ImagenRequestBody(n *Normalized) ([]byte, error) {
	if strings.TrimSpace(n.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required for imagen models")
	}
	if len(n.Images) > 0 {
		return nil, fmt.Errorf("imagen models do not support reference images (use a Gemini image model for edits)")
	}
	params := map[string]any{}
	if n.N > 0 {
		params["sampleCount"] = n.N
	}
	if ar := n.AspectRatioFor(); ar != "" && ar != "auto" {
		params["aspectRatio"] = ar
	}
	req := map[string]any{
		"instances":  []map[string]any{{"prompt": n.Prompt}},
		"parameters": params,
	}
	return json.Marshal(req)
}

type geminiPart struct {
	Text       string `json:"text"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Role  string       `json:"role"`
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string          `json:"modelVersion"`
	Error        json.RawMessage `json:"error"`
}

// GeminiToOpenAI converts a Gemini generateContent response into the normalized
// OpenAI Images response. It returns nil when the body is not a recognizable
// Gemini response (the caller then relays the upstream body verbatim).
func GeminiToOpenAI(body []byte, model, responseFormat string) []byte {
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil
	}
	if len(gr.Candidates) == 0 && len(gr.Error) > 0 {
		return nil
	}
	resp := Response{Created: time.Now().Unix(), Model: firstNonEmpty(gr.ModelVersion, model)}
	var revised string
	for _, cand := range gr.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				resp.Data = append(resp.Data, imageDatum(part.InlineData.Data, part.InlineData.MimeType, responseFormat, revised))
				continue
			}
			if strings.TrimSpace(part.Text) != "" && revised == "" {
				revised = strings.TrimSpace(part.Text)
			}
		}
	}
	if len(resp.Data) == 0 {
		return nil
	}
	if gr.UsageMetadata != nil {
		resp.Usage = &Usage{
			InputTokens:  gr.UsageMetadata.PromptTokenCount,
			OutputTokens: gr.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  gr.UsageMetadata.TotalTokenCount,
		}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return out
}

type imagenResponse struct {
	Predictions []struct {
		BytesBase64Encoded string `json:"bytesBase64Encoded"`
		MimeType           string `json:"mimeType"`
		RaiFilteredReason  string `json:"raiFilteredReason"`
	} `json:"predictions"`
	Error json.RawMessage `json:"error"`
}

// ImagenToOpenAI converts an Imagen :predict response into the normalized OpenAI
// Images response. It returns nil when the body is unrecognizable.
func ImagenToOpenAI(body []byte, model, responseFormat string) []byte {
	var ir imagenResponse
	if err := json.Unmarshal(body, &ir); err != nil {
		return nil
	}
	if len(ir.Predictions) == 0 {
		return nil
	}
	resp := Response{Created: time.Now().Unix(), Model: model}
	for _, p := range ir.Predictions {
		if p.BytesBase64Encoded == "" {
			continue
		}
		resp.Data = append(resp.Data, imageDatum(p.BytesBase64Encoded, p.MimeType, responseFormat, ""))
	}
	if len(resp.Data) == 0 {
		return nil
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return out
}

// imageDatum builds one normalized datum. Gemini/Imagen always return inline
// base64; when the client asked for a URL we surface a data: URL (the gateway
// does not host generated images).
func imageDatum(b64, mime, responseFormat, revised string) ImageDatum {
	mime = firstNonEmpty(mime, "image/png")
	d := ImageDatum{MimeType: mime, RevisedPrompt: revised}
	if responseFormat == "url" {
		d.URL = "data:" + mime + ";base64," + b64
	} else {
		d.B64JSON = b64
	}
	return d
}

// ImageCount returns how many images a normalized response carries.
func ImageCount(respBody []byte) int {
	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0
	}
	return len(resp.Data)
}

// CostPerImage returns the curated USD-per-image price for a model id.
func CostPerImage(model string) (float64, bool) {
	m, ok := Lookup(model)
	if !ok || m.CostPerImage <= 0 {
		return 0, false
	}
	return m.CostPerImage, true
}

// DialectForModel returns the registry dialect for a model id, defaulting to
// the OpenAI-compatible images dialect for unknown ids.
func DialectForModel(model string) Dialect {
	if m, ok := Lookup(model); ok {
		return m.Dialect
	}
	return DialectOpenAI
}

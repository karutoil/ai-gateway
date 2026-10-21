package translate

import (
	"encoding/json"
	"testing"
)

// OpenAI clients (and the Responses API's input_image parts) may carry the
// image URL as a plain STRING: {"type":"image_url","image_url":"data:..."}.
// The old converter only understood the map form, so the block was forwarded
// verbatim to Anthropic — which rejects unknown part types. The image was
// silently lost from the conversation.
func TestOpenAIToAnthropicStringImageURL(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"what is in this image?"},
			{"type":"image_url","image_url":"data:image/png;base64,QUJDRA=="}
		]}]}`)
	out, _, err := OpenAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("content should stay a block array: %v", err)
	}
	found := false
	for _, b := range blocks {
		if b["type"] != "image" {
			continue
		}
		src, _ := b["source"].(map[string]interface{})
		if src["type"] != "base64" {
			t.Fatalf("image source type = %v, want base64", src["type"])
		}
		if src["media_type"] != "image/png" {
			t.Fatalf("media_type = %v, want image/png", src["media_type"])
		}
		if src["data"] != "QUJDRA==" {
			t.Fatalf("data = %v, want the untouched base64 payload", src["data"])
		}
		found = true
	}
	if !found {
		t.Fatalf("string image_url was dropped; body: %s", out)
	}
}

// The same string-form image must survive as a remote (non-data) URL too,
// forwarded as Anthropic's url source instead of being dropped.
func TestOpenAIToAnthropicStringImageRemoteURL(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":"https://cdn.example.com/cat.png"}
		]}]}`)
	out, _, err := OpenAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil || len(req.Messages) != 1 {
		t.Fatalf("unmarshal: %v, messages=%d", err, len(req.Messages))
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "image" {
		t.Fatalf("expected one image block, got %v", blocks)
	}
	src, _ := blocks[0]["source"].(map[string]interface{})
	if src["type"] != "url" || src["url"] != "https://cdn.example.com/cat.png" {
		t.Fatalf("url source = %v", src)
	}
}

// Responses-style input_image parts ({"type":"input_image","image_url":...})
// in an OpenAI chat body must convert to Anthropic image blocks, not ride
// through as an unknown part type.
func TestOpenAIToAnthropicInputImagePart(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[
		{"role":"user","content":[
			{"type":"input_text","text":"describe"},
			{"type":"input_image","image_url":"data:image/jpeg;base64,QUJD"}
		]}]}`)
	out, _, err := OpenAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	hasText, hasImage := false, false
	for _, b := range blocks {
		if b["type"] == "text" && b["text"] == "describe" {
			hasText = true
		}
		if b["type"] == "image" {
			if src, _ := b["source"].(map[string]interface{}); src["media_type"] == "image/jpeg" && src["data"] == "QUJD" {
				hasImage = true
			}
		}
	}
	if !hasText || !hasImage {
		t.Fatalf("input_image/input_text conversion lost payload: text=%v image=%v body=%s", hasText, hasImage, out)
	}
}

// Assistant-turn histories carrying image parts must convert too.
func TestOpenAIToAnthropicAssistantImagePart(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"here you go"},
			{"type":"image_url","image_url":"data:image/png;base64,QUJD"}
		]}]}`)
	out, _, err := OpenAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range blocks {
		if b["type"] == "image" {
			found = true
		}
	}
	if !found {
		t.Fatalf("assistant image part dropped: %s", out)
	}
}

// A bare map-shaped image_url (the classic form) keeps working.
func TestOpenAIToAnthropicMapImageURLStillWorks(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
		]}]}`)
	out, _, err := OpenAIToAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(req.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "image" {
		t.Fatalf("map image_url regression: %s", out)
	}
}

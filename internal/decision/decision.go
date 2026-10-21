// Package decision implements the "System One" decision-model protocol.
//
// Decision models are a distinct class of model from chat LLMs: instead of
// generating text, they read an input `state` plus a map of typed `questions`
// and return a calibrated probability for every allowed answer. The wire
// protocol was introduced by TypeSafe AI's Jev ("System One API"), and is now
// spoken by Cloudflare's Clef family (clef / clef-flash / clef-omni), the
// open-weight Kev family, llama.cpp's /v1/systemone, and several other
// "Jev-style" models.
//
// This package is deliberately dependency-free so both the proxy (request
// validation + provider translation) and the catalog (curated model seeding)
// can import it without an import cycle.
package decision

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Question types accepted by the System One API.
const (
	TypeNoul   = "noul"   // yes/no, returns P(yes) in [0,1]
	TypeChoice = "choice" // one of N options, returns the full distribution
	TypeScore  = "score"  // ordered rubric, returns a probability-weighted score
)

// Protocol limits, from the TypeSafe / Workers AI schemas.
const (
	MinQuestions     = 1
	MaxQuestions     = 64
	MaxQuestionIDLen = 100
	MaxChoiceOptions = 255
	MaxScoreLevels   = 10
	MinScoreLevels   = 2
	// MaxMediaRefs bounds each media array on the request (images / audio /
	// videos). Clef supports images; Clef-omni adds audio and video. All three
	// were previously unbounded: only `images` was capped (at a literal 4),
	// while `audio` and `videos` accepted an arbitrary number of arbitrarily
	// large base64 blobs, so a single request could force the gateway to buffer
	// hundreds of megabytes before the upstream ever saw it. The cap is per
	// array, matching the per-array shape of the protocol.
	MaxMediaRefs = 4
)

// Question is one typed entry in the request's `questions` map. Instructions
// and criteria are kept as raw JSON because the API accepts strings, objects,
// or arrays for both (structured prompts that reference nested `state`).
type Question struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Request is the System One evaluation request body.
type Request struct {
	Model     string              `json:"model"`
	State     json.RawMessage     `json:"state"`
	Questions map[string]Question `json:"questions"`
	// Media extensions. Clef supports images; Clef-omni adds audio and video.
	Images []json.RawMessage `json:"images,omitempty"`
	Audio  []json.RawMessage `json:"audio,omitempty"`
	Videos []json.RawMessage `json:"videos,omitempty"`
}

// Answer is one typed answer, keyed by the same id as its question.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// Usage mirrors the System One usage object. Decision models bill input
// tokens only (output tokens are free / too cheap to meter).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the System One evaluation response body.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// ExtractModel reads just the `model` field from a request body. It is a thin
// convenience over the generic proxy extractor, kept here so callers that only
// need the selector do not pull in the translate package.
func ExtractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
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

// Validate enforces the System One request rules. It intentionally mirrors the
// upstream schemas (TypeSafe and Workers AI) so malformed requests fail fast
// at the gateway with a 400 naming the offending field, instead of burning an
// upstream round trip to get an opaque 422.
func Validate(body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return invalid("", "body must be a valid JSON object")
	}
	// model: required non-empty string
	var model string
	if raw, ok := top["model"]; !ok || json.Unmarshal(raw, &model) != nil || strings.TrimSpace(model) == "" {
		return invalid("model", "required non-empty string")
	}
	// state: required, may be any JSON value except null/absent
	if raw, ok := top["state"]; !ok || strings.TrimSpace(string(raw)) == "null" {
		return invalid("state", "required (string, object, or array)")
	}
	// questions: object with 1..64 entries
	rawQ, ok := top["questions"]
	if !ok {
		return invalid("questions", "required object of typed questions")
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(rawQ, &questions); err != nil {
		return invalid("questions", "must be an object mapping question id to a typed question")
	}
	if len(questions) < MinQuestions {
		return invalid("questions", "at least one question is required")
	}
	if len(questions) > MaxQuestions {
		return invalid("questions", fmt.Sprintf("at most %d questions are allowed", MaxQuestions))
	}
	// Deterministic error ordering keeps tests stable.
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := validateQuestionID(id); err != nil {
			return err
		}
		if err := validateQuestion(id, questions[id]); err != nil {
			return err
		}
	}
	if raw, ok := top["images"]; ok {
		if err := validateMediaArray("images", raw); err != nil {
			return err
		}
	}
	if raw, ok := top["audio"]; ok {
		if err := validateMediaArray("audio", raw); err != nil {
			return err
		}
	}
	if raw, ok := top["videos"]; ok {
		if err := validateMediaArray("videos", raw); err != nil {
			return err
		}
	}
	return nil
}

// validateMediaArray bounds one media array on the request.
//
// `images` used to be capped inline at a literal 4 while `audio` and `videos`
// were never validated at all, so a request could carry an unbounded number of
// media entries and the gateway would buffer them all before dispatch. The same
// error shape is used for every array so a client sees which field was refused.
func validateMediaArray(field string, raw json.RawMessage) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return invalid(field, "must be an array")
	}
	if len(items) > MaxMediaRefs {
		return invalid(field, fmt.Sprintf("at most %d %s entries are allowed", MaxMediaRefs, field))
	}
	return nil
}

func validateQuestionID(id string) error {
	if id == "" {
		return invalid("questions", "question ids must be non-empty")
	}
	if len(id) > MaxQuestionIDLen {
		return invalid("questions."+id, fmt.Sprintf("question id exceeds %d characters", MaxQuestionIDLen))
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-'
		if !ok {
			return invalid("questions."+id, "question id may only use letters, digits, '_', '.' and '-'")
		}
	}
	return nil
}

func validateQuestion(id string, raw json.RawMessage) error {
	var q Question
	if err := json.Unmarshal(raw, &q); err != nil {
		return invalid("questions."+id, "must be a typed question object")
	}
	field := "questions." + id
	switch q.Type {
	case TypeNoul, TypeChoice, TypeScore:
	case "":
		return invalid(field+".type", "required (noul, choice, or score)")
	default:
		return invalid(field+".type", "unknown question type "+q.Type)
	}
	if err := validateInstructions(field, q.Instructions); err != nil {
		return err
	}
	switch q.Type {
	case TypeNoul:
		return validateNoulCriteria(field, q.Criteria)
	case TypeChoice:
		return validateChoiceCriteria(field, q.Criteria)
	case TypeScore:
		return validateScoreCriteria(field, q.Criteria)
	}
	return nil
}

func validateInstructions(field string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return invalid(field+".instructions", "required")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return invalid(field+".instructions", "must be a non-empty string, object, or array")
		}
		return nil
	}
	// object / array / other JSON: accept as long as it is not empty.
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return invalid(field+".instructions", "must be a string, object, or array")
	}
	switch v := generic.(type) {
	case map[string]interface{}:
		if len(v) == 0 {
			return invalid(field+".instructions", "must not be an empty object")
		}
	case []interface{}:
		if len(v) == 0 {
			return invalid(field+".instructions", "must not be an empty array")
		}
	default:
		return invalid(field+".instructions", "must be a string, object, or array")
	}
	return nil
}

func validateNoulCriteria(field string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil // optional
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return invalid(field+".criteria", "must be an object with optional true/false descriptions")
	}
	for k := range obj {
		if k != "true" && k != "false" {
			return invalid(field+".criteria", "keys must be \"true\" and/or \"false\"")
		}
	}
	return nil
}

func validateChoiceCriteria(field string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return invalid(field+".criteria", "required map of option to description")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return invalid(field+".criteria", "must be an object mapping option to description")
	}
	if len(obj) < 2 {
		return invalid(field+".criteria", "a choice needs at least 2 options")
	}
	if len(obj) > MaxChoiceOptions {
		return invalid(field+".criteria", fmt.Sprintf("a choice allows at most %d options", MaxChoiceOptions))
	}
	for k := range obj {
		if strings.TrimSpace(k) == "" {
			return invalid(field+".criteria", "option keys must be non-empty strings")
		}
	}
	return nil
}

func validateScoreCriteria(field string, raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return invalid(field+".criteria", "required ordered array of level descriptions")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return invalid(field+".criteria", "must be an ordered array of levels")
	}
	if len(arr) < MinScoreLevels {
		return invalid(field+".criteria", fmt.Sprintf("a score needs at least %d levels", MinScoreLevels))
	}
	if len(arr) > MaxScoreLevels {
		return invalid(field+".criteria", fmt.Sprintf("a score allows at most %d levels", MaxScoreLevels))
	}
	return nil
}

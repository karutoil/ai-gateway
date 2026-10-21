package decision

import (
	"encoding/json"
	"strings"
	"testing"
)

func validBody() []byte {
	return []byte(`{
		"model": "clef",
		"state": {"ticket": "Checkout is down for every customer."},
		"questions": {
			"urgent": {"type": "noul", "instructions": "Is this urgent?"},
			"team": {"type": "choice", "instructions": "Which team?", "criteria": {"billing": "payments", "technical": "outages"}},
			"severity": {"type": "score", "instructions": "How severe?", "criteria": ["none", "minor", "major"]}
		}
	}`)
}

func TestValidateAcceptsWellFormedRequest(t *testing.T) {
	if err := Validate(validBody()); err != nil {
		t.Fatalf("expected valid request, got %v", err)
	}
}

func TestValidateRejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "body must be a valid JSON object"},
		{"missing model", `{"state":"x","questions":{"a":{"type":"noul","instructions":"q"}}}`, "model"},
		{"missing state", `{"model":"clef","questions":{"a":{"type":"noul","instructions":"q"}}}`, "state"},
		{"null state", `{"model":"clef","state":null,"questions":{"a":{"type":"noul","instructions":"q"}}}`, "state"},
		{"no questions", `{"model":"clef","state":"x","questions":{}}`, "at least one question"},
		{"questions not object", `{"model":"clef","state":"x","questions":[]}`, "must be an object"},
		{"missing questions", `{"model":"clef","state":"x"}`, "required object"},
		{"too many questions", `{"model":"clef","state":"x","questions":{` + manyQuestions(65) + `}}`, "at most 64"},
		{"bad question id", `{"model":"clef","state":"x","questions":{"bad id":{"type":"noul","instructions":"q"}}}`, "letters, digits"},
		{"unknown type", `{"model":"clef","state":"x","questions":{"a":{"type":"rank","instructions":"q"}}}`, "unknown question type"},
		{"missing type", `{"model":"clef","state":"x","questions":{"a":{"instructions":"q"}}}`, "type"},
		{"empty instructions", `{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":""}}}`, "instructions"},
		{"choice one option", `{"model":"clef","state":"x","questions":{"a":{"type":"choice","instructions":"q","criteria":{"only":"one"}}}}`, "at least 2 options"},
		{"choice no criteria", `{"model":"clef","state":"x","questions":{"a":{"type":"choice","instructions":"q"}}}`, "criteria"},
		{"choice criteria array", `{"model":"clef","state":"x","questions":{"a":{"type":"choice","instructions":"q","criteria":["x","y"]}}}`, "criteria"},
		{"score one level", `{"model":"clef","state":"x","questions":{"a":{"type":"score","instructions":"q","criteria":["only"]}}}`, "at least 2 levels"},
		{"score eleven levels", `{"model":"clef","state":"x","questions":{"a":{"type":"score","instructions":"q","criteria":[` + tenPlusOne() + `]}}}`, "at most 10 levels"},
		{"noul criteria bad key", `{"model":"clef","state":"x","questions":{"a":{"type":"noul","instructions":"q","criteria":{"maybe":"?"}}}}`, "true"},
		{"too many images", `{"model":"clef","state":"x","images":["a","b","c","d","e"],"questions":{"a":{"type":"noul","instructions":"q"}}}`, "at most 4 images"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestValidateAcceptsOptionalAndStructuredShapes(t *testing.T) {
	// noul without criteria, structured instructions object, structured state,
	// and the Clef-omni media extensions.
	body := `{
		"model": "clef-omni",
		"state": ["line one", {"role": "user", "text": "hi"}],
		"images": ["data:image/png;base64,AAAA"],
		"audio": ["data:audio/mpeg;base64,AAAA"],
		"videos": ["data:video/mp4;base64,AAAA"],
		"questions": {
			"label": {"type": "noul", "instructions": {"question": "Visible?", "meta": {"frame": 3}}},
			"team": {"type": "choice", "instructions": "Which team?", "criteria": {"a": null, "b": {"detail": "x"}}},
			"severity": {"type": "score", "instructions": "Severity?", "criteria": ["low", {"label": "high", "weight": 2}]}
		}
	}`
	if err := Validate([]byte(body)); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestLookupAndIsDecisionModel(t *testing.T) {
	yes := []string{
		"clef", "clef-flash", "clef-omni",
		"@cf/cloudflare/clef", "@cf/cloudflare/clef-flash",
		"cloudflare/clef", "kev-4b", "kev-27b", "kev-99b",
		"jev-latest", "jev", "systemone/kev-4b", "openjev", "laya",
	}
	for _, id := range yes {
		if !IsDecisionModel(id) {
			t.Errorf("expected %q to be a decision model", id)
		}
	}
	no := []string{"", "gpt-4o", "claude-3-5-sonnet", "gpt-oss-120b", "gemini-2.5-pro"}
	for _, id := range no {
		if IsDecisionModel(id) {
			t.Errorf("expected %q NOT to be a decision model", id)
		}
	}
}

func TestLookupPricingAndCapabilities(t *testing.T) {
	m, ok := Lookup("@cf/cloudflare/clef")
	if !ok {
		t.Fatal("clef not found")
	}
	if m.ID != "clef" || m.InputCostPerM != 0.240 || m.OutputCostPerM != 0 || !m.Vision {
		t.Fatalf("unexpected clef entry: %+v", m)
	}
	if m.CatalogID() != "cloudflare/clef" {
		t.Fatalf("catalog id = %q", m.CatalogID())
	}
	// Decision models never bill output tokens.
	for _, dm := range All() {
		if dm.OutputCostPerM != 0 {
			t.Errorf("%s has non-zero output cost %v", dm.ID, dm.OutputCostPerM)
		}
	}
	// Family fallback keeps unknown versions recognized.
	if m, ok := Lookup("kev-5b"); !ok || m.Family != "kev" {
		t.Fatalf("kev-5b fallback failed: %+v ok=%v", m, ok)
	}
}

func TestExtractModel(t *testing.T) {
	if got := ExtractModel([]byte(`{"model":" kev-4b ","state":"x"}`)); got != "kev-4b" {
		t.Fatalf("ExtractModel = %q", got)
	}
	if got := ExtractModel([]byte(`{`)); got != "" {
		t.Fatalf("ExtractModel on bad json = %q", got)
	}
}

func TestAnswerRoundTrip(t *testing.T) {
	// The response shape must survive a marshal/unmarshal with the documented
	// fields (guards against a typo in a json tag).
	raw := `{"model":"clef","answers":{
		"urgent":{"type":"noul","noul":0.95},
		"team":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81},
		"severity":{"type":"score","score":1.05,"legend":{"0":"calm","1":"angry"},"probabilities":{"0":0.0,"1":0.95},"confidence":0.92}
	},"usage":{"input_tokens":296,"output_tokens":20}}`
	var resp Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Answers["urgent"].Noul == nil || *resp.Answers["urgent"].Noul != 0.95 {
		t.Fatalf("noul not decoded: %+v", resp.Answers["urgent"])
	}
	if resp.Answers["team"].Choice != "billing" || len(resp.Answers["team"].Probabilities) != 2 {
		t.Fatalf("choice not decoded: %+v", resp.Answers["team"])
	}
	if resp.Answers["severity"].Legend["1"] != "angry" || resp.Usage.InputTokens != 296 {
		t.Fatalf("score/usage not decoded: %+v", resp)
	}
}

func manyQuestions(n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, `"q`+itoa(i)+`":{"type":"noul","instructions":"x"}`)
	}
	return strings.Join(parts, ",")
}

func tenPlusOne() string {
	parts := make([]string, 11)
	for i := range parts {
		parts[i] = `"l` + itoa(i) + `"`
	}
	return strings.Join(parts, ",")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestValidateBoundsEveryMediaArray is the P3-12 guard.
//
// `images` was capped at a literal 4 while `audio` and `videos` were never
// validated at all, so a single request could carry an unbounded number of
// media entries. The gateway buffers the whole body before dispatch, so an
// unbounded array is a memory-exhaustion vector, not just a protocol nicety.
// All three arrays now share MaxMediaRefs and name the offending field.
func TestValidateBoundsEveryMediaArray(t *testing.T) {
	// Build an array with one entry over the cap.
	over := make([]string, MaxMediaRefs+1)
	for i := range over {
		over[i] = `"data:application/octet-stream;base64,AAAA"`
	}
	overJSON := "[" + strings.Join(over, ",") + "]"

	for _, field := range []string{"images", "audio", "videos"} {
		body := `{"model":"clef-omni","state":"x","` + field + `":` + overJSON +
			`,"questions":{"q":{"type":"noul","instructions":"ok?"}}}`
		err := Validate([]byte(body))
		if err == nil {
			t.Fatalf("%s over the cap must be rejected", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%s rejection must name the field, got: %v", field, err)
		}
	}

	// Exactly at the cap is still accepted for every array.
	atCap := make([]string, MaxMediaRefs)
	for i := range atCap {
		atCap[i] = `"data:application/octet-stream;base64,AAAA"`
	}
	atCapJSON := "[" + strings.Join(atCap, ",") + "]"
	body := `{"model":"clef-omni","state":"x","images":` + atCapJSON +
		`,"audio":` + atCapJSON + `,"videos":` + atCapJSON +
		`,"questions":{"q":{"type":"noul","instructions":"ok?"}}}`
	if err := Validate([]byte(body)); err != nil {
		t.Fatalf("arrays at the cap must be accepted: %v", err)
	}

	// A non-array media field is still a clear 400, not a silent pass.
	bad := `{"model":"clef","state":"x","audio":"not-an-array","questions":{"q":{"type":"noul","instructions":"ok?"}}}`
	if err := Validate([]byte(bad)); err == nil {
		t.Fatal("a non-array audio field must be rejected")
	}
}

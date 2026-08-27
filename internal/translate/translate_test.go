package translate

import "testing"

func TestAnthropicToOpenAI(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"system":"you are helpful","max_tokens":100}`)
	out, model, err := AnthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-4o" {
		t.Fatalf("model mismatch %s", model)
	}
	if string(out) == "" {
		t.Fatal("empty")
	}
	// should contain system message
	if string(out) != "" && !contains(string(out), "you are helpful") {
		t.Fatalf("missing system %s", string(out))
	}
}

func TestResponsesToChat(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-mini","input":"Hello","instructions":"Be helpful"}`)
	out, _, err := ResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(out), "Hello") {
		t.Fatalf("missing input %s", string(out))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

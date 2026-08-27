package httperr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusTooManyRequests, "over_quota", TypeOverQuota)
	if rec.Code != 429 {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"]["type"] != TypeOverQuota {
		t.Fatalf("type %v", body["error"])
	}
	if body["error"]["message"] != "over_quota" {
		t.Fatalf("message %v", body["error"])
	}
}

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-gateway/internal/antigravity"
)

func TestOAuthCommitWriterBuffersFailuresAndCommitsSuccess(t *testing.T) {
	dst := httptest.NewRecorder()
	w := newOAuthCommitWriter(dst)
	w.Header().Set("X-Test", "yes")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("failure"))
	if w.committed {
		t.Fatal("failure must remain uncommitted for failover")
	}
	w.flushFailure()
	if dst.Code != http.StatusBadGateway || dst.Body.String() != "failure" {
		t.Fatalf("buffered failure was not flushed: code=%d body=%q", dst.Code, dst.Body.String())
	}

	dst = httptest.NewRecorder()
	w = newOAuthCommitWriter(dst)
	w.Header().Set("X-Test", "yes")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes.Repeat([]byte("ok"), 2))
	if !w.committed || dst.Code != http.StatusOK || dst.Body.String() != "okok" {
		t.Fatalf("success was not committed: committed=%v code=%d body=%q", w.committed, dst.Code, dst.Body.String())
	}
	if dst.Header().Get("X-Test") != "yes" {
		t.Fatal("buffered response headers were not copied")
	}
}

func TestQuotaAllowsUsesConsumedThreshold(t *testing.T) {
	used := 89.9
	q := &antigravity.Quota{Models: []antigravity.ModelQuota{{ModelID: "gemini-3.8-flash", UsedPct: &used}}}
	if !quotaAllows(q, "gemini-3.8-flash", 90) {
		t.Fatal("quota below threshold should remain eligible")
	}
	used = 90
	if quotaAllows(q, "gemini-3.8-flash", 90) {
		t.Fatal("quota at threshold should be skipped")
	}
}

func TestQuotaAllowsFailsOpenForUnknownSnapshotModel(t *testing.T) {
	used := 100.0
	q := &antigravity.Quota{Models: []antigravity.ModelQuota{{ModelID: "other-model", UsedPct: &used}}}
	if !quotaAllows(q, "gemini-3.8-flash", 90) {
		t.Fatal("an incomplete snapshot must not black-hole an unreported model")
	}
}

func TestQuotaAllowsExhausted(t *testing.T) {
	q := &antigravity.Quota{Models: []antigravity.ModelQuota{{ModelID: "gemini-3.8-flash", Exhausted: true}}}
	if quotaAllows(q, "gemini-3.8-flash", 90) {
		t.Fatal("exhausted model should be skipped")
	}
}

package budget

import (
	"errors"
	"testing"
	"time"
)

// TestMemoryLimiterReturnsTypedQuotaError pins the shape of MemoryLimiter's
// over-quota failure: the typed QuotaError (limit kind, limit, used, prefix)
// is returned to the caller, not just the bare ErrOverQuota wrapper.
//
// Regression: Check used to invoke quotaErr purely for its webhook side
// effect, discard the result, and return a plain fmt.Errorf — the middleware
// still saw over_quota, but every caller that wanted to know WHICH limit
// tripped and by how much got nothing.
func TestMemoryLimiterReturnsTypedQuotaError(t *testing.T) {
	m := NewMemoryLimiter(100)
	m.RecordUsage("pfx", 95, 0, time.Now())
	err := m.Check("pfx", 10)
	if err == nil || !IsOverQuota(err) {
		t.Fatalf("expected over_quota, got %v", err)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("error is not a *QuotaError: %T (%v)", err, err)
	}
	if qe.LimitKind != "daily_token" {
		t.Fatalf("limit kind = %q, want daily_token", qe.LimitKind)
	}
	if qe.Limit != 100 {
		t.Fatalf("limit = %d, want 100", qe.Limit)
	}
	if qe.Used != 105 {
		t.Fatalf("used = %d, want 105 (95 recorded + 10 requested)", qe.Used)
	}
	if qe.Prefix != "pfx" {
		t.Fatalf("prefix = %q, want pfx", qe.Prefix)
	}
	if !errors.Is(err, ErrOverQuota) {
		t.Fatal("QuotaError must unwrap to ErrOverQuota")
	}
}

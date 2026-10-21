package config

import "testing"

// TestGlobalDailyTokenLimitEnv pins the config knob behind the in-memory
// global token cap (budget.MemoryLimiter composed in front of the DB ledger
// on the production admission path): GLOBAL_DAILY_TOKEN_LIMIT parses as an
// integer, defaults to 0 (disabled) when unset, and falls back to the
// default on non-numeric garbage.
func TestGlobalDailyTokenLimitEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", 0},
		{"set", "5000", 5000},
		{"garbage", "not-a-number", 0},
		{"zero", "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GLOBAL_DAILY_TOKEN_LIMIT", tc.env)
			t.Setenv("DATABASE_URL", ":memory:")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.GlobalDailyTokenLimit != tc.want {
				t.Fatalf("GlobalDailyTokenLimit = %d, want %d", cfg.GlobalDailyTokenLimit, tc.want)
			}
		})
	}
}

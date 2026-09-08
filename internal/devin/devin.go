package devin

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// APIURL returns the Devin web-API base URL (OAuth token exchange).
func APIURL() string {
	if v := strings.TrimSpace(os.Getenv("DEVIN_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.devin.ai"
}

// apiURL is the internal alias kept for symmetry with Host/WebappURL.
func apiURL() string { return APIURL() }

// Host returns the Connect-proto transport base URL.
func Host() string {
	if v := strings.TrimSpace(os.Getenv("DEVIN_HOST")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://server.codeium.com"
}

// WebappURL returns the browser authorize base URL.
func WebappURL() string {
	if v := strings.TrimSpace(os.Getenv("DEVIN_WEBAPP_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://app.devin.ai"
}

// CallbackPath is the loopback OAuth callback path on the user's machine.
const CallbackPath = "/callback"

// DefaultCallbackPort is the preferred loopback port (59653), matching the
// reference extension. In gateway paste mode nothing listens locally — the
// browser fails to load and the user pastes the callback URL instead.
const DefaultCallbackPort = 59653

// PasteRedirectURI is advertised for Devin paste-mode logins.
const PasteRedirectURI = "http://127.0.0.1:59653/callback"

const sessionTokenPrefix = "devin-session-token$"

// NormalizeSessionToken ensures the session-token prefix the backend expects.
func NormalizeSessionToken(apiKey string) string {
	if strings.HasPrefix(apiKey, sessionTokenPrefix) {
		return apiKey
	}
	return sessionTokenPrefix + apiKey
}

// TokenExpiry derives token expiry from a JWT exp claim, falling back to one
// year for opaque tokens (mirrors the reference extension).
func TokenExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) >= 2 && parts[1] != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp *float64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp != nil {
				exp := *claims.Exp
				if exp > 0 && exp < 1e15 {
					return int64(exp)*1000 - 5*60*1000
				}
			}
		}
	}
	return time.Now().UnixMilli() + 365*24*60*60*1000
}

package antigravity

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Endpoints and wire constants ported from pi-antigravity.
const (
	DefaultEndpoint  = "https://daily-cloudcode-pa.googleapis.com"
	SandboxEndpoint  = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	FallbackEndpoint = "https://cloudcode-pa.googleapis.com"
	DefaultUserAgent = "antigravity/cli/1.1.23 (aidev_client; os_type=linux; arch=amd64; cl=974125021; auth_method=consumer)"
	discoveryTimeout = 8 * time.Second
)

// EndpointCandidates returns the API base override or the built-in priority list.
func EndpointCandidates() []string {
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_BASE_URL")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	if v := strings.TrimSpace(os.Getenv("NOAGY_BASE_URL")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	return []string{DefaultEndpoint, SandboxEndpoint, FallbackEndpoint}
}

// UserAgent returns the request User-Agent.
func UserAgent() string {
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_USER_AGENT")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NOAGY_USER_AGENT")); v != "" {
		return v
	}
	return DefaultUserAgent
}

// Headers builds Antigravity API headers.
func Headers(token string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
		"User-Agent":    UserAgent(),
	}
}

// StableProjectID derives a UUID-shaped fallback id from a seed (email preferred).
func StableProjectID(seed string) string {
	h := sha1.Sum([]byte("antigravity:" + seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", b)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// DefaultProjectID prefers ANTIGRAVITY_PROJECT_ID, else a stable seed.
func DefaultProjectID(seed string) string {
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NOAGY_PROJECT_ID")); v != "" {
		return v
	}
	if seed == "" {
		seed = "antigravity-default"
	}
	return StableProjectID(seed)
}

func doJSON(client *http.Client, endpoint, path, token string, payload any) (map[string]any, int, error) {
	c := client
	if c == nil {
		c = &http.Client{Timeout: discoveryTimeout}
	}
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+path, body)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range Headers(token) {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("antigravity %s: %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// ExtractProjectID walks loadCodeAssist-style payloads for a project id.
func ExtractProjectID(data any) string {
	switch v := data.(type) {
	case map[string]any:
		for _, k := range []string{"antigravityProjectId", "projectId", "backendProjectId", "userDefinedCloudaicompanionProject", "cloudaicompanionProject", "project"} {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
			if m, ok := v[k].(map[string]any); ok {
				if s, ok := m["id"].(string); ok && s != "" {
					return s
				}
			}
		}
		for _, k := range []string{"projects", "projectIds", "cloudaicompanionProjects"} {
			if arr, ok := v[k].([]any); ok {
				for _, item := range arr {
					if s, ok := item.(string); ok && s != "" {
						return s
					}
					if id := ExtractProjectID(item); id != "" {
						return id
					}
				}
			}
		}
	case []any:
		for _, item := range v {
			if id := ExtractProjectID(item); id != "" {
				return id
			}
		}
	case string:
		return v
	}
	return ""
}

// LoadCodeAssist discovers the Cloud Code Assist project id for an access token.
func LoadCodeAssist(token string, client *http.Client) string {
	for _, ep := range EndpointCandidates() {
		data, _, err := doJSON(client, ep, "/v1internal:loadCodeAssist", token, map[string]any{
			"metadata": map[string]any{"ideType": "ANTIGRAVITY"},
		})
		if err != nil || data == nil {
			continue
		}
		if id := ExtractProjectID(data); id != "" {
			return id
		}
		if id := listCompanionProjects(token, client); id != "" {
			return id
		}
		return ""
	}
	return ""
}

func listCompanionProjects(token string, client *http.Client) string {
	for _, ep := range EndpointCandidates() {
		data, _, err := doJSON(client, ep, "/v1internal:listCloudAICompanionProjects", token, map[string]any{})
		if err != nil || data == nil {
			continue
		}
		if id := ExtractProjectID(data); id != "" {
			return id
		}
	}
	return ""
}

// FetchUserEmail resolves the Google account email for an access token.
func FetchUserEmail(token string, client *http.Client) string {
	c := client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequest(http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return ""
	}
	return data.Email
}

// DecodedDefaultClientID is the public Antigravity desktop OAuth client
// (a public client id, safe to ship; also published in pi-antigravity).
// Prefer ANTIGRAVITY_CLIENT_ID/SECRET env when running a private OAuth app.
func DecodedDefaultClientID() string {
	// Halves kept on separate lines for readability, mirroring upstream.
	part1 := "MTA3MTAwNjA2MDU5MS10bWhzc2luMmgyMWxjcmUyMzV2dG9sb2poNGc0MDNlc"
	part2 := "C5hcHBzLmdvb2dsZXVzZXJjb250ZW50LmNvbQ=="
	if b, err := base64.StdEncoding.DecodeString(part1 + part2); err == nil {
		return string(b)
	}
	return ""
}

// DecodedDefaultClientSecret returns the bundled public-client credential.
// This is Google's published desktop-client value (identical bytes ship in
// the public pi-antigravity package), not a private server secret — it only
// enables the zero-setup paste flow. Operators who prefer their own OAuth app
// should set ANTIGRAVITY_CLIENT_ID/ANTIGRAVITY_CLIENT_SECRET instead.
func DecodedDefaultClientSecret() string {
	part1 := "R09DU1BYLUs1OEZXUjQ"
	part2 := "4NkxkTEoxbUxCOHNYQzR6NnFEQWY="
	if b, err := base64.StdEncoding.DecodeString(part1 + part2); err == nil {
		return string(b)
	}
	return ""
}

package mcp

import (
	"os"
	"strings"
)

// AllowPrivateUpstreams reports whether MCP servers may point at private
// (loopback / RFC1918 / link-local / metadata) addresses.
//
// The registry is an outbound-fetch primitive driven by an operator-supplied
// URL, so without a guard any `mcp:write` caller can make the gateway probe its
// own network — and the failure text (a dial error, a timeout) is reflected back
// as a port scanner. Production refuses private addresses by default;
// MCP_ALLOW_PRIVATE_UPSTREAMS=true is the explicit opt-in for a sidecar MCP
// server, and non-production keeps the permissive behaviour so local dev and the
// test suite (httptest loopback servers) keep working.
func AllowPrivateUpstreams() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_ALLOW_PRIVATE_UPSTREAMS"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	env := os.Getenv("ENV")
	if env == "" {
		env = os.Getenv("GO_ENV")
	}
	return env != "production"
}

// StdioEnabled reports whether this deployment permits stdio-transport MCP
// servers, i.e. whether the gateway may spawn local processes.
//
// Default is DISABLED. A stdio server executes an arbitrary local binary as the
// gateway's own UID, so it is an admin-grade capability (see
// handler.requireStdioAllowed, which additionally requires a global admin), and
// a deployment that never needs it should not be reachable that way at all.
// Set MCP_STDIO_ENABLED=true to allow it.
func StdioEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MCP_STDIO_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// childEnvAllowlist is the set of environment variables a spawned MCP server
// inherits. Everything else in the gateway's environment stays in the gateway.
//
// Why: the gateway process holds MASTER_KEY (which decrypts every provider API
// key and every MCP credential), JWT_SECRET, the database DSN and often the
// provider keys themselves. A third-party MCP server is typically
// `npx -y @vendor/some-mcp-server` — freshly downloaded code from a public
// registry. Handing it os.Environ() means one malicious or merely chatty server
// package exfiltrates the master key and the whole deployment's credentials.
var childEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
	"TMPDIR": true, "TEMP": true, "TMP": true,
	"SHELL": true, "TERM": true, "PWD": true,
	// Node/npx and Python/uvx runtime knobs that are not secrets and that a
	// spawn will otherwise re-derive badly (cache/proxy locations).
	"NODE_PATH": true, "NPM_CONFIG_CACHE": true, "NPM_CONFIG_REGISTRY": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"PYTHONPATH": true, "VIRTUAL_ENV": true, "UV_CACHE_DIR": true,
	"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
}

// secretEnvMarkers catch credential-shaped names even if the allowlist ever
// drifts: a name containing any of these is never forwarded.
var secretEnvMarkers = []string{
	"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL",
	"DATABASE", "DSN", "MASTER", "JWT", "SESSION", "COOKIE", "AUTH",
	"PRIVATE", "SIGNING", "ENCRYPTION", "WEBHOOK",
}

// childEnv builds the environment for a spawned stdio MCP server: an allowlist
// of harmless process variables, minus anything credential-shaped, plus the
// server's own configured env (which the operator entered deliberately and
// which is stored encrypted at rest).
func childEnv(extra map[string]string) []string {
	out := make([]string, 0, len(childEnvAllowlist)+len(extra))
	for _, kv := range os.Environ() {
		eq := strings.Index(kv, "=")
		if eq <= 0 {
			continue
		}
		name := kv[:eq]
		if !childEnvAllowlist[name] {
			continue
		}
		if looksSecret(name) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// looksSecret reports whether an environment variable NAME is
// credential-shaped. Deliberately conservative: a false positive only costs a
// spawned server one variable it probably did not need.
func looksSecret(name string) bool {
	up := strings.ToUpper(name)
	for _, m := range secretEnvMarkers {
		if strings.Contains(up, m) {
			return true
		}
	}
	return false
}

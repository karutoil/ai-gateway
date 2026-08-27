package passkey

import (
	"os"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/rs/zerolog/log"
)

func NewWebAuthn() (*webauthn.WebAuthn, error) {
	// Determine RPID and origins from PUBLIC_URL, else defaults
	publicURL := strings.TrimSpace(os.Getenv("PUBLIC_URL"))
	corsOrigins := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	rpid := "localhost"
	origins := []string{"http://localhost:8080", "http://localhost:5173", "http://localhost:3000"}
	displayName := "AI Gateway"

	if publicURL != "" {
		// Extract host for RPID, keep scheme+host for origin
		host := publicURL
		if idx := strings.Index(publicURL, "://"); idx != -1 {
			host = publicURL[idx+3:]
			if slash := strings.Index(host, "/"); slash != -1 {
				host = host[:slash]
			}
			// Origin is scheme + host
			origin := publicURL[:idx+3] + host
			origins = []string{origin}
			// RPID is host without port
			if colon := strings.Index(host, ":"); colon != -1 {
				host = host[:colon]
			}
			rpid = host
			// Also include localhost for dev
			origins = append(origins, "http://localhost:8080", "http://localhost:5173")
		} else {
			origins = []string{publicURL}
			rpid = publicURL
		}
	}
	if corsOrigins != "" && corsOrigins != "*" {
		for _, o := range strings.Split(corsOrigins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				origins = append(origins, o)
				// also derive RPID from first cors origin if not set
			}
		}
	}
	// Dedupe origins
	seen := map[string]bool{}
	var uniq []string
	for _, o := range origins {
		if !seen[o] {
			seen[o] = true
			uniq = append(uniq, o)
		}
	}
	origins = uniq

	log.Debug().Str("rpid", rpid).Strs("origins", origins).Msg("webauthn config")

	w, err := webauthn.New(&webauthn.Config{
		RPDisplayName: displayName,
		RPID:          rpid,
		RPOrigins:     origins,
	})
	return w, err
}

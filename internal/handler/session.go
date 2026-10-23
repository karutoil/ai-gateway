package handler

import (
	"net/http"
	"os"
	"strings"
)

// SessionCookieName is the dashboard session cookie. HttpOnly always; Secure
// is derived from the request/public URL so local http dev still works.
const SessionCookieName = "gw_token"

// ClientIsHTTPS reports whether the request arrived over TLS as far as we can
// tell (direct TLS, or an https X-Forwarded-Proto/PUBLIC_URL behind a proxy).
func clientIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	if strings.HasPrefix(strings.ToLower(os.Getenv("PUBLIC_URL")), "https://") {
		return true
	}
	return false
}

// setSessionCookie writes the session cookie with hardened flags:
// HttpOnly (not readable by JS/XSS), Secure on https, SameSite=Lax.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   clientIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// SetSessionCookie is the exported form of setSessionCookie, for authentication
// surfaces that live outside this package (passkey/recovery login). Every path
// that mints a dashboard JWT MUST set this cookie: the SPA never persists a
// token from a response body, so a login that only returns JSON leaves the
// browser unauthenticated.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	setSessionCookie(w, r, token)
}

// clearSessionCookie removes the session cookie (logout).
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   clientIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

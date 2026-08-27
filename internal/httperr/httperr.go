package httperr

import (
	"encoding/json"
	"net/http"
)

const (
	TypeAuth       = "authentication_error"
	TypeInvalid    = "invalid_request_error"
	TypeRateLimit  = "rate_limit_error"
	TypeOverQuota  = "over_quota_error"
	TypeProxy      = "proxy_error"
	TypeNotFound   = "not_found_error"
	TypePermission = "permission_error"
)

// Write sends the unified error envelope used by /v1 and /api.
func Write(w http.ResponseWriter, status int, message, typ string) {
	if typ == "" {
		typ = TypeInvalid
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
		},
	})
}

func Auth(w http.ResponseWriter, msg string) {
	Write(w, http.StatusUnauthorized, msg, TypeAuth)
}

func Invalid(w http.ResponseWriter, msg string) {
	Write(w, http.StatusBadRequest, msg, TypeInvalid)
}

func NotFound(w http.ResponseWriter, msg string) {
	Write(w, http.StatusNotFound, msg, TypeNotFound)
}

func Proxy(w http.ResponseWriter, status int, msg string) {
	if status < 400 {
		status = http.StatusBadGateway
	}
	Write(w, status, msg, TypeProxy)
}

func Forbidden(w http.ResponseWriter, msg string) {
	Write(w, http.StatusForbidden, msg, TypePermission)
}

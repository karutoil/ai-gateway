package passkey

import (
	"ai-gateway/internal/user"

	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnUser wraps DashboardUser to implement webauthn.User
type WebAuthnUser struct {
	*user.DashboardUser
	Creds []webauthn.Credential
}

func (u WebAuthnUser) WebAuthnID() []byte   { return []byte(u.ID) }
func (u WebAuthnUser) WebAuthnName() string { return u.Username }
func (u WebAuthnUser) WebAuthnDisplayName() string {
	if u.DisplayName != nil && *u.DisplayName != "" {
		return *u.DisplayName
	}
	return u.Username
}
func (u WebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.Creds }
func (u WebAuthnUser) WebAuthnIcon() string                       { return "" }

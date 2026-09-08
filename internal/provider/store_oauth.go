package provider

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
)

// loadOAuthMeta fills the non-secret OAuth status fields best-effort.
// Missing columns (pre-017 DBs) leave the provider unmarked rather than failing.
func (s *Store) loadOAuthMeta(p *models.Provider) {
	if s == nil || s.db == nil || p == nil {
		return
	}
	var defID, email, project sql.NullString
	var expires sql.NullInt64
	err := s.db.QueryRow(db.Q(`SELECT oauth_def_id, oauth_email, oauth_project_id, oauth_expires_at FROM providers WHERE id=?`), p.ID).Scan(&defID, &email, &project, &expires)
	if err != nil {
		return
	}
	if defID.Valid {
		p.OAuthDefID = defID.String
	}
	if email.Valid {
		p.OAuthEmail = email.String
	}
	if project.Valid {
		p.OAuthProject = project.String
	}
	if expires.Valid && expires.Int64 > 0 {
		ts := time.UnixMilli(expires.Int64).UTC().Format(time.RFC3339)
		p.OAuthExpires = &ts
	}
	// Connected = oauth-backed and has a stored refresh token.
	if p.OAuthDefID != "" {
		var n int
		if err := s.db.QueryRow(db.Q(`SELECT COUNT(*) FROM providers WHERE id=? AND oauth_refresh_enc IS NOT NULL AND length(oauth_refresh_enc) > 0`), p.ID).Scan(&n); err == nil && n > 0 {
			p.OAuthConnected = true
		}
	}
}

// EnrichOAuth loads OAuth status for a list (N small — providers are few).
func (s *Store) EnrichOAuth(list []models.Provider) []models.Provider {
	for i := range list {
		s.loadOAuthMeta(&list[i])
	}
	return list
}

// EnrichOAuthOne loads OAuth status for a single provider.
func (s *Store) EnrichOAuthOne(p *models.Provider) *models.Provider {
	if p == nil {
		return p
	}
	s.loadOAuthMeta(p)
	return p
}

// SetOAuthTokens stores refreshed OAuth credentials (encrypted at rest).
func (s *Store) SetOAuthTokens(id, defID string, tok *oauth.Tokens) error {
	if tok == nil {
		return fmt.Errorf("nil tokens")
	}
	refreshEnc, err := Encrypt([]byte(tok.Refresh), s.masterKey)
	if err != nil {
		return err
	}
	accessEnc, err := Encrypt([]byte(tok.Access), s.masterKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(db.Q(`UPDATE providers SET oauth_def_id=?, oauth_refresh_enc=?, oauth_access_enc=?, oauth_expires_at=?, oauth_email=?, oauth_project_id=? WHERE id=?`),
		defID, refreshEnc, accessEnc, tok.ExpiresAt, tok.Email, tok.ProjectID, id)
	return err
}

// ClearOAuth disconnects OAuth (removes stored tokens, keeps the provider row).
func (s *Store) ClearOAuth(id string) error {
	_, err := s.db.Exec(db.Q(`UPDATE providers SET oauth_refresh_enc=NULL, oauth_access_enc=NULL, oauth_expires_at=NULL, oauth_email='', oauth_project_id='' WHERE id=?`), id)
	return err
}

// OAuthTokens loads decrypted OAuth tokens for a provider.
func (s *Store) OAuthTokens(p *models.Provider) (*oauth.Tokens, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("nil provider")
	}
	var defID, email, project sql.NullString
	var refreshEnc, accessEnc []byte
	var expires sql.NullInt64
	err := s.db.QueryRow(db.Q(`SELECT oauth_def_id, oauth_refresh_enc, oauth_access_enc, oauth_expires_at, oauth_email, oauth_project_id FROM providers WHERE id=?`), p.ID).Scan(&defID, &refreshEnc, &accessEnc, &expires, &email, &project)
	if err != nil {
		return nil, "", err
	}
	if len(refreshEnc) == 0 {
		return nil, defID.String, fmt.Errorf("oauth not connected")
	}
	refresh, err := Decrypt(refreshEnc, s.masterKey)
	if err != nil {
		return nil, defID.String, fmt.Errorf("oauth refresh decrypt failed: %w", err)
	}
	tok := &oauth.Tokens{Refresh: string(refresh)}
	if len(accessEnc) > 0 {
		if access, err := Decrypt(accessEnc, s.masterKey); err == nil {
			tok.Access = string(access)
		}
	}
	if expires.Valid {
		tok.ExpiresAt = expires.Int64
	}
	if email.Valid {
		tok.Email = email.String
	}
	if project.Valid {
		tok.ProjectID = project.String
	}
	return tok, defID.String, nil
}

// EnsureFreshAccess returns a valid access token + project id, refreshing when expired.
// On refresh it persists the new tokens before returning.
func (s *Store) EnsureFreshAccess(ctx context.Context, p *models.Provider, httpClient *http.Client) (access, project, email string, err error) {
	tok, defID, err := s.OAuthTokens(p)
	if err != nil {
		return "", "", "", err
	}
	def, _, ok := oauth.Get(defID)
	if !ok {
		return "", "", "", fmt.Errorf("unknown oauth provider %q", defID)
	}
	if !tok.Expired(time.Now()) && tok.Access != "" {
		return tok.Access, tok.ProjectID, tok.Email, nil
	}
	refreshed, err := oauth.Refresh(ctx, def, tok.Refresh, httpClient)
	if err != nil {
		return "", "", "", fmt.Errorf("oauth refresh failed: %w", err)
	}
	// Preserve project/email across refresh when the provider hook left them empty.
	if refreshed.ProjectID == "" {
		refreshed.ProjectID = tok.ProjectID
	}
	if refreshed.Email == "" {
		refreshed.Email = tok.Email
	}
	if err := s.SetOAuthTokens(p.ID, defID, refreshed); err != nil {
		return "", "", "", err
	}
	return refreshed.Access, refreshed.ProjectID, refreshed.Email, nil
}

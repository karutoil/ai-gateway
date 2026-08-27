package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Password hashing policy.
//
// New hashes are bcrypt (cost BCRYPTCost). Legacy rows created before this
// migration stored unsalted SHA-256 hex digests; those still verify during the
// cutover window and are transparently upgraded to bcrypt on next successful
// use (see NeedsRehash callers in the user store).
const BCRYPTCost = 10

const legacySHA256Len = 64 // hex-encoded sha256 digest length

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) string {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), BCRYPTCost)
	if err != nil {
		// bcrypt errors only on passwords >72 bytes or cost overflow; fall back
		// to a prehashed variant so login never wedges on hashing failure.
		b, _ = bcrypt.GenerateFromPassword([]byte(prehash(pw)), BCRYPTCost)
		return "pre:" + string(b)
	}
	return string(b)
}

// VerifyPasswordHash checks pw against hash, supporting both current bcrypt
// hashes ("$2a$…"/"pre:$2a$…") and legacy unsalted SHA-256 hex digests.
// Comparison cost is deliberately non-constant-time between formats: bcrypt's
// built-in comparison is used where applicable and digest comparison is
// constant-time within its branch.
func VerifyPasswordHash(hash, pw string) bool {
	if hash == "" {
		return false
	}
	if strings.HasPrefix(hash, "$2") || strings.HasPrefix(hash, "pre:") {
		compare := pw
		h := hash
		if strings.HasPrefix(hash, "pre:") {
			h = strings.TrimPrefix(hash, "pre:")
			compare = prehash(pw)
		}
		return bcrypt.CompareHashAndPassword([]byte(h), []byte(compare)) == nil
	}
	// Legacy unsalted SHA-256 hex digest.
	if len(hash) == legacySHA256Len && isHex(hash) {
		digest := sha256Hex(pw)
		return subtle.ConstantTimeCompare([]byte(digest), []byte(hash)) == 1
	}
	return false
}

// NeedsRehash reports whether hash predates the bcrypt scheme and should be
// upgraded after a successful verification.
func NeedsRehash(hash string) bool {
	return hash != "" && !strings.HasPrefix(hash, "$2") && !strings.HasPrefix(hash, "pre:")
}

// HashPasswordLegacy returns the deprecated unsalted SHA-256 form.
// Retained ONLY for legacy-verification tests; never store new hashes with it.
func HashPasswordLegacy(pw string) string {
	return sha256Hex(pw)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// prehash reduces long inputs (>72 bytes, bcrypt's limit) before bcrypt.
// Uses sha256 so effectively-unlimited input lengths remain supported without
// truncation collisions between distinct long passwords.
func prehash(pw string) string {
	return "sha256:" + sha256Hex(pw)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// RandomSecret returns a 32-byte cryptographically random lowercase-hex string,
// suitable for unusable-password seeding and recovery material.
func RandomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

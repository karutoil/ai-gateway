package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMasterKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, ".master_key")
	jwtFile := filepath.Join(dir, ".jwt_secret")
	dbFile := filepath.Join(dir, "test.db")

	origMKF := os.Getenv("MASTER_KEY_FILE")
	origJF := os.Getenv("JWT_SECRET_FILE")
	origDB := os.Getenv("DATABASE_URL")
	origMK := os.Getenv("MASTER_KEY")
	origJWT := os.Getenv("JWT_SECRET")
	origAdmin := os.Getenv("ADMIN_PASSWORD")
	defer func() {
		os.Setenv("MASTER_KEY_FILE", origMKF)
		os.Setenv("JWT_SECRET_FILE", origJF)
		os.Setenv("DATABASE_URL", origDB)
		os.Setenv("MASTER_KEY", origMK)
		os.Setenv("JWT_SECRET", origJWT)
		os.Setenv("ADMIN_PASSWORD", origAdmin)
	}()

	os.Setenv("MASTER_KEY_FILE", keyFile)
	os.Setenv("JWT_SECRET_FILE", jwtFile)
	os.Setenv("DATABASE_URL", dbFile)
	os.Setenv("MASTER_KEY", "")
	os.Setenv("JWT_SECRET", "")
	os.Unsetenv("ADMIN_PASSWORD")
	// Ensure .env does not override the empty MASTER_KEY/JWT_SECRET set above
	// tryLoadDotEnv only sets if not exists, and we just set them to "" (exists with empty), so it won't override

	c1, err := Load()
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}
	if len(c1.MasterKey) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(c1.MasterKey))
	}
	first := make([]byte, 32)
	copy(first, c1.MasterKey)
	jwt1 := string(c1.JWTSecret)

	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("master key file not created at %s", keyFile)
	}

	c2, err := Load()
	if err != nil {
		t.Fatalf("second load failed: %v", err)
	}
	if string(first) != string(c2.MasterKey) {
		t.Fatalf("master key not persistent: first %x second %x", first[:4], c2.MasterKey[:4])
	}
	if jwt1 != string(c2.JWTSecret) {
		t.Fatalf("jwt not persistent")
	}
	t.Logf("persistence ok mk:%x jwt:%s", first[:4], jwt1[:8])
}

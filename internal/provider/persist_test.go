package provider

import (
	"os"
	"path/filepath"
	"testing"

	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
)

func TestProviderSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, ".master_key")
	jwtFile := filepath.Join(dir, ".jwt_secret")
	dbPath := filepath.Join(dir, "test_persist.db")

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
	os.Setenv("DATABASE_URL", dbPath)
	os.Unsetenv("MASTER_KEY")
	os.Unsetenv("JWT_SECRET")
	os.Unsetenv("ADMIN_PASSWORD")

	cfg1, err := config.Load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	database1, err := db.Open(cfg1.DatabaseURL)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	store1 := NewStore(database1, cfg1.MasterKey)
	prov, err := store1.Create("openai-test", models.ProviderOpenAI, "https://api.openai.com/v1", "sk-test-123")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created provider %s", prov.ID)
	p1, _ := store1.GetByID(prov.ID)
	key1, err := store1.DecryptKey(p1)
	if err != nil || key1 != "sk-test-123" {
		t.Fatalf("decrypt with first key failed: %v key=%q", err, key1)
	}
	database1.Close()

	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(cfg1.MasterKey) != string(cfg2.MasterKey) {
		t.Fatalf("master key not persistent across restart")
	}
	database2, err := db.Open(cfg2.DatabaseURL)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer database2.Close()
	store2 := NewStore(database2, cfg2.MasterKey)
	p2, err := store2.GetByID(prov.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	key2, err := store2.DecryptKey(p2)
	if err != nil {
		t.Fatalf("decrypt after restart failed: %v - this is the bug we fixed (master key was ephemeral)", err)
	}
	if key2 != "sk-test-123" {
		t.Fatalf("key mismatch after restart: got %q", key2)
	}
	t.Logf("provider survived restart with same master key")
}

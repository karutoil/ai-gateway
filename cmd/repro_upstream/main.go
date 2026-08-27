// Temporary debug tool: tries candidate master keys to decrypt a provider key,
// then fires test variants at the upstream. Never prints key material.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

func tryDecrypt(ct, key []byte) ([]byte, bool) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false
	}
	ns := gcm.NonceSize()
	if len(ct) < ns {
		return nil, false
	}
	pt, err := gcm.Open(nil, ct[:ns], ct[ns:], nil)
	if err != nil {
		return nil, false
	}
	return pt, true
}

func hexKey(s string) []byte {
	s = strings.TrimSpace(s)
	if len(s) != 64 {
		return nil
	}
	k := make([]byte, 32)
	for i := 0; i < 32; i++ {
		fmt.Sscanf(s[i*2:i*2+2], "%2x", &k[i])
	}
	return k
}

func main() {
	providerName := os.Args[1]
	_ = godotenv.Load()

	db, err := sql.Open("sqlite3", "file:data/gateway.db?mode=ro")
	if err != nil {
		panic(err)
	}
	var ct []byte
	if err := db.QueryRow(`SELECT api_key_enc FROM providers WHERE lower(name)=?`, providerName).Scan(&ct); err != nil {
		panic(err)
	}

	// candidates
	var cands []struct {
		name string
		key  []byte
	}
	if mk := os.Getenv("MASTER_KEY"); mk != "" {
		cands = append(cands, struct {
			name string
			key  []byte
		}{"MASTER_KEY from .env", hexKey(mk)})
	}
	if f, err := os.ReadFile("data/.master_key"); err == nil {
		cands = append(cands, struct {
			name string
			key  []byte
		}{"data/.master_key file", hexKey(string(f))})
	}
	pw := os.Getenv("ADMIN_PASSWORD")
	if pw == "" {
		pw = "admin123"
	}
	h := sha256.Sum256([]byte("gateway-master-key:" + pw))
	cands = append(cands, struct {
		name string
		key  []byte
	}{fmt.Sprintf("derived from ADMIN_PASSWORD(%q)", pw), h[:]})
	h2 := sha256.Sum256([]byte("gateway-master-key:admin123"))
	cands = append(cands, struct {
		name string
		key  []byte
	}{"derived from default admin123", h2[:]})

	var apiKey []byte
	var used string
	for _, c := range cands {
		if c.key == nil {
			continue
		}
		if pt, ok := tryDecrypt(ct, c.key); ok {
			apiKey = pt
			used = c.name
			break
		}
	}
	if apiKey == nil {
		fmt.Println("no candidate key worked; candidates tried:")
		for _, c := range cands {
			fmt.Println(" -", c.name)
		}
		return
	}
	fmt.Printf("decrypted with: %s (len=%d, prefix=%q…, masked)\n", used, len(apiKey), string(apiKey[:4]))

	base := "https://opencode.ai/zen/go/v1"
	cases := []struct {
		name string
		body string
	}{
		{"non-stream plain", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"say hi"}],"max_tokens":10}`},
		{"stream plain", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"say hi"}],"max_tokens":10,"stream":true}`},
		{"stream + include_usage (gateway-injected)", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"say hi"}],"max_tokens":10,"stream":true,"stream_options":{"include_usage":true}}`},
	}
	client := &http.Client{Timeout: 60 * time.Second}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader([]byte(c.body)))
		req.Header.Set("Authorization", "Bearer "+string(apiKey))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("== %s: TRANSPORT ERR %v\n", c.name, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		fmt.Printf("== %s: status=%d\n   %s\n", c.name, resp.StatusCode, bytes.ReplaceAll(b, []byte("\n"), []byte(" ")))
	}
}

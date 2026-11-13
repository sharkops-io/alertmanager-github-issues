package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKey(t *testing.T, key *rsa.PrivateKey, pkcs8 bool) string {
	t.Helper()
	var block *pem.Block
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	}
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAppTokenMintsNarrowsAndCaches(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			http.NotFound(w, r)
			return
		}
		mints++
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Fatalf("not a JWT: %q", jwt)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
			t.Fatalf("bad signature: %v", err)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		json.Unmarshal(raw, &claims)
		if claims["iss"] != "7" || claims["exp"].(float64) <= claims["iat"].(float64) {
			t.Errorf("claims = %v", claims)
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "alerts" || body.Permissions["issues"] != "write" {
			t.Errorf("token not narrowed: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": now.Add(time.Hour)})
	}))
	defer srv.Close()

	for _, pkcs8 := range []bool{false, true} {
		mints = 0
		a := &appToken{apiURL: srv.URL, appID: "7", installationID: "42", keyPath: writeKey(t, key, pkcs8),
			repo: "alerts", client: srv.Client(), now: func() time.Time { return now }}
		for i := 0; i < 3; i++ {
			tok, err := a.Token(context.Background())
			if err != nil || tok != "ghs_x" {
				t.Fatalf("pkcs8=%v: %q %v", pkcs8, tok, err)
			}
		}
		if mints != 1 {
			t.Errorf("pkcs8=%v: minted %d times, want 1 (cached)", pkcs8, mints)
		}
		// Within five minutes of expiry it mints again.
		later := now.Add(56 * time.Minute)
		a.now = func() time.Time { return later }
		a.Token(context.Background())
		if mints != 2 {
			t.Errorf("pkcs8=%v: did not refresh near expiry", pkcs8)
		}
	}
}

func TestFileTokenRejectsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t")
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := (fileToken{path: p}).Token(context.Background()); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/x?page=2" {
		t.Fatalf("got %q", got)
	}
	if nextLink(`<https://api.github.com/x?page=1>; rel="prev"`) != "" {
		t.Fatal("followed a non-next link")
	}
}

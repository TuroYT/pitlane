package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppClient(t *testing.T) {
	s := newStrictServer(t, fakeGitHub{})

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err := s.store.SetSecret("app_pem", keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Set("app_id", "42"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appClient(); err != nil {
		t.Fatalf("saved app: %v", err)
	}

	// Encrypted with another master key: the error says so, and the
	// installations listing reports it instead of "parse app private key".
	if err := s.store.Set("app_pem", "enc:"+base64.StdEncoding.EncodeToString(make([]byte, 64))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appClient(); err == nil || !strings.Contains(err.Error(), "decrypt app_pem") {
		t.Fatalf("undecryptable key: %v", err)
	}
	if err := s.store.Set("app_client_id", "Iv1.test"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleInstallations(w, httptest.NewRequest(http.MethodGet, "/api/installations", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "decrypt app_pem") {
		t.Fatalf("installations: %d %s", w.Code, w.Body.String())
	}

	if err := s.store.Set("app_id", "not-a-number"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSecret("app_pem", keyPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appClient(); err == nil || !strings.Contains(err.Error(), "App ID") {
		t.Fatalf("bad app id: %v", err)
	}
}

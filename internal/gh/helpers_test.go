package gh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// serveAPI starts a fake GitHub API answering with h and points the client at
// it until the test ends. It changes package state: a test using it must not
// call t.Parallel.
func serveAPI(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })
	return srv
}

// testKey is an RSA key generated once for the whole test binary.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// pkcs1PEM is the "RSA PRIVATE KEY" encoding GitHub gives for an App key.
func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// newTestClient returns an AppClient for app 12345 signing with testKey.
func newTestClient(t *testing.T) *AppClient {
	t.Helper()
	c, err := NewAppClient(12345, pkcs1PEM(testKey()))
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}
	return c
}

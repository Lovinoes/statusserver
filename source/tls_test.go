package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSigned writes a fresh self-signed cert/key pair for cn.
func writeSelfSigned(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func leafCN(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")

	if _, err := newCertReloader(certPath, keyPath); err == nil {
		t.Fatal("missing files should fail fast")
	}

	writeSelfSigned(t, certPath, keyPath, "one.test")
	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.GetCertificate(nil)
	if cn := leafCN(t, c); cn != "one.test" {
		t.Fatalf("CN = %q", cn)
	}

	// renew on disk with a later mod time.
	writeSelfSigned(t, certPath, keyPath, "two.test")
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(certPath, later, later)
	_ = os.Chtimes(keyPath, later, later)

	// within the check interval the old cert keeps being served.
	c, _ = r.GetCertificate(nil)
	if cn := leafCN(t, c); cn != "one.test" {
		t.Errorf("reloaded too early, CN = %q", cn)
	}

	r.mu.Lock()
	r.lastCheck = time.Time{}
	r.mu.Unlock()
	c, _ = r.GetCertificate(nil)
	if cn := leafCN(t, c); cn != "two.test" {
		t.Errorf("after renewal CN = %q, want two.test", cn)
	}

	// a broken renewal keeps the last good pair.
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	evenLater := later.Add(time.Minute)
	_ = os.Chtimes(keyPath, evenLater, evenLater)
	r.mu.Lock()
	r.lastCheck = time.Time{}
	r.mu.Unlock()
	c, err = r.GetCertificate(nil)
	if err != nil || leafCN(t, c) != "two.test" {
		t.Errorf("broken renewal should keep serving two.test, got err %v", err)
	}
}

func TestHardenedTLSServesTLS13Only(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeSelfSigned(t, certPath, keyPath, "localhost")
	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(http.HandlerFunc(handleHealthz))
	ts.TLS = hardenedTLSConfig(r.GetCertificate)
	ts.StartTLS()
	defer ts.Close()

	insecure := func(maxVer uint16) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
			MaxVersion:         maxVer,
		}}}
	}
	resp, err := insecure(0).Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("TLS 1.3 request failed: %v", err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("negotiated %v, want TLS 1.3", resp.TLS)
	}

	if resp, err := insecure(tls.VersionTLS12).Get(ts.URL + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("TLS 1.2 client should be rejected")
	}
}

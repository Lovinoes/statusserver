package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// hardenedTLSConfig is the profile used when serving TLS directly: TLS 1.3
// only, key exchange locked to a post-quantum hybrid then classical curves
// (in preference order), and ALPN offering h2 with an http/1.1 fallback that
// websocket upgrades negotiate down to.
func hardenedTLSConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768,
			tls.X25519,
			tls.CurveP384,
		},
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: getCert,
	}
}

// certReloadCheck is how often the cert/key files are checked for changes.
const certReloadCheck = 30 * time.Second

// certReloader serves a key pair from disk and picks up renewed files
// (e.g. from certbot or cert-manager) without a restart.
type certReloader struct {
	certPath, keyPath string

	mu               sync.Mutex
	cert             *tls.Certificate
	certMod, keyMod  time.Time
	lastCheck        time.Time
	lastErrLoggedFor [2]time.Time // mod times of the last pair that failed to load
}

// newCertReloader loads the key pair once, failing fast on a bad path.
func newCertReloader(certPath, keyPath string) (*certReloader, error) {
	r := &certReloader{certPath: certPath, keyPath: keyPath}
	certMod, keyMod, err := r.modTimes()
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair (cert %q, key %q): %w", certPath, keyPath, err)
	}
	r.cert, r.certMod, r.keyMod, r.lastCheck = &cert, certMod, keyMod, time.Now()
	return r, nil
}

func (r *certReloader) modTimes() (certMod, keyMod time.Time, err error) {
	ci, err := os.Stat(r.certPath)
	if err != nil {
		return
	}
	ki, err := os.Stat(r.keyPath)
	if err != nil {
		return
	}
	return ci.ModTime(), ki.ModTime(), nil
}

// GetCertificate is the tls.Config hook. It re-checks the files at most every
// certReloadCheck and keeps serving the old pair if the new one is broken
// (e.g. the cert was written but the key not yet).
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.lastCheck) < certReloadCheck {
		return r.cert, nil
	}
	r.lastCheck = time.Now()

	certMod, keyMod, err := r.modTimes()
	if err != nil {
		log.Printf("TLS reload: %v (keeping current certificate)", err)
		return r.cert, nil
	}
	if certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod) {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		if pair := [2]time.Time{certMod, keyMod}; pair != r.lastErrLoggedFor {
			r.lastErrLoggedFor = pair
			log.Printf("TLS reload: %v (keeping current certificate, will retry)", err)
		}
		return r.cert, nil
	}
	r.cert, r.certMod, r.keyMod = &cert, certMod, keyMod
	log.Printf("TLS certificate reloaded from %s", r.certPath)
	return r.cert, nil
}

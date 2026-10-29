package github

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePrivateKeyErrors(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"not PEM", []byte("hello"), "no PEM block"},
		{"empty", nil, "no PEM block"},
		{"corrupt PKCS#1", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("junk")}), "asn1"},
		{"corrupt PKCS#8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}), "asn1"},
		{"PKCS#8 but not RSA", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), "*ecdsa.PrivateKey, not RSA"},
		{"other block type", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}), `unexpected PEM block "EC PRIVATE KEY"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := ParsePrivateKey(tt.in)
			if err == nil || k != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParsePrivateKey = %v, %v; want error containing %q", k, err, tt.want)
			}
		})
	}
}

func TestLoadPrivateKey(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.pem")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadPrivateKey(good)
	if err != nil || !k.Equal(testKey) {
		t.Fatalf("LoadPrivateKey = %v, %v", k, err)
	}

	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(bad); err == nil || !strings.HasPrefix(err.Error(), bad+": ") {
		t.Errorf("bad file: err = %v, want the path as a prefix", err)
	}

	missing := filepath.Join(dir, "missing.pem")
	if _, err := LoadPrivateKey(missing); !os.IsNotExist(err) {
		t.Errorf("missing file: err = %v", err)
	}
}

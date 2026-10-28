package github

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
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// tokenMargin is how long before expiry an installation token is replaced.
const tokenMargin = 5 * time.Minute

// LoadPrivateKey reads a PEM-encoded RSA private key from path.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// ParsePrivateKey reads an RSA private key in PKCS#1 or PKCS#8 PEM form.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is %T, not RSA", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
	}
}

// appJWT returns a JWT that authenticates as the App, signed with RS256.
// It is backdated 60 seconds for clock drift and lasts nine minutes, inside
// GitHub's ten-minute limit.
func appJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{
		IAT: now.Add(-60 * time.Second).Unix(),
		EXP: now.Add(9 * time.Minute).Unix(),
		ISS: strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// installationToken returns a cached installation token, fetching a new one
// when there is none or it expires within tokenMargin. Only one fetch runs
// at a time; other callers wait for it.
func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.tokenExpiry.Add(-tokenMargin)) {
		return c.token, nil
	}
	jwt, err := appJWT(c.key, c.appID, now)
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", c.installationID)
	if _, err := c.send(ctx, http.MethodPost, c.baseURL+path, "Bearer "+jwt, nil, &out); err != nil {
		return "", fmt.Errorf("get installation token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("get installation token: empty token in response")
	}
	c.token, c.tokenExpiry = out.Token, out.ExpiresAt
	return c.token, nil
}

// TokenExpiry returns when the cached installation token expires, or the
// zero time if there is none yet.
func (c *Client) TokenExpiry() time.Time {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.tokenExpiry
}

// dropToken forgets the cached token if it is still tok, so the next call
// fetches a fresh one.
func (c *Client) dropToken(tok string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token == tok {
		c.token, c.tokenExpiry = "", time.Time{}
	}
}

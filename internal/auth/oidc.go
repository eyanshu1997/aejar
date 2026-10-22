// Package auth identifies callers: the owner (owner token or a verified
// Cloudflare Access login) or a session (per-session bearer token).
package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCVerifier verifies RS256 OIDC ID tokens against the issuer's JWKS. aejar
// uses it for Google ID tokens that oauth2-proxy hands to nginx, so identity
// rests on Google's signature rather than on a header anyone could set.
type OIDCVerifier struct {
	// Issuers lists accepted iss values. Google uses two spellings.
	Issuers []string
	// Audience is the OAuth client id the token must be issued for.
	Audience string
	JWKSURL  string
	// RequireEmailVerified rejects tokens whose email_verified is not true.
	RequireEmailVerified bool
	Client               *http.Client
	Now                  func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

const keyTTL = time.Hour

// GoogleVerifier returns a verifier for Google ID tokens issued to clientID.
func GoogleVerifier(clientID string) *OIDCVerifier {
	return &OIDCVerifier{
		Issuers:              []string{"https://accounts.google.com", "accounts.google.com"},
		Audience:             clientID,
		JWKSURL:              "https://www.googleapis.com/oauth2/v3/certs",
		RequireEmailVerified: true,
	}
}

// Email verifies token and returns its email claim, lowercased.
func (v *OIDCVerifier) Email(token string) (string, error) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", fmt.Errorf("header: %w", err)
	}
	if header.Alg != "RS256" {
		return "", fmt.Errorf("unexpected alg %q", header.Alg)
	}
	key, err := v.key(header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	var claims struct {
		Email         string          `json:"email"`
		EmailVerified any             `json:"email_verified"`
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Exp           int64           `json:"exp"`
		Nbf           int64           `json:"nbf"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", fmt.Errorf("claims: %w", err)
	}
	now := v.now().Unix()
	const skew = 60
	if claims.Exp == 0 || now > claims.Exp+skew {
		return "", errors.New("token expired")
	}
	if claims.Nbf != 0 && now+skew < claims.Nbf {
		return "", errors.New("token not yet valid")
	}
	if !contains(v.Issuers, claims.Iss) {
		return "", fmt.Errorf("unexpected issuer %q", claims.Iss)
	}
	if v.Audience == "" || !audContains(claims.Aud, v.Audience) {
		return "", errors.New("token is for a different application")
	}
	if claims.Email == "" {
		return "", errors.New("token has no email")
	}
	// Google sends a boolean; some issuers send the string "true".
	if v.RequireEmailVerified && claims.EmailVerified != true && claims.EmailVerified != "true" {
		return "", errors.New("email is not verified")
	}
	return strings.ToLower(claims.Email), nil
}

func (v *OIDCVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// key returns the public key for kid, refreshing the key set when it is
// stale or the kid is unknown (issuers rotate keys).
func (v *OIDCVerifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < keyTTL {
		return k, nil
	}
	// Refresh at most every 10 s so a flood of bad kids can't hammer Cloudflare.
	if time.Since(v.fetched) > 10*time.Second || v.keys == nil {
		if err := v.refresh(); err != nil {
			return nil, err
		}
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (v *OIDCVerifier) refresh() error {
	url := v.JWKSURL
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no RSA keys")
	}
	v.keys, v.fetched = keys, time.Now()
	return nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// aud is a string or an array of strings.
func audContains(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

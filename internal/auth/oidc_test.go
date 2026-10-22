package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const clientID = "123-abc.apps.googleusercontent.com"

type signer struct {
	key *rsa.PrivateKey
	kid string
}

func newSigner(t *testing.T, kid string) signer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{k, kid}
}

func (s signer) token(t *testing.T, claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": s.kid, "typ": "JWT"})
	body := enc(claims)
	digest := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwks(signers ...signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var keys []map[string]string
		for _, s := range signers {
			keys = append(keys, map[string]string{
				"kid": s.kid, "kty": "RSA", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes()),
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
}

func TestOIDCVerifier(t *testing.T) {
	good := newSigner(t, "k1")
	rogue := newSigner(t, "k1") // same kid, different key
	srv := httptest.NewServer(jwks(good))
	defer srv.Close()
	now := time.Now()
	v := GoogleVerifier(clientID)
	v.JWKSURL, v.Now = srv.URL, func() time.Time { return now }

	base := func() map[string]any {
		return map[string]any{"email": "Owner@Example.com", "email_verified": true, "iss": "https://accounts.google.com", "aud": clientID, "exp": now.Add(time.Hour).Unix()}
	}
	// nginx forwards oauth2-proxy's Authorization value, "Bearer <token>".
	email, err := v.Email("Bearer " + good.token(t, base()))
	if err != nil || email != "owner@example.com" {
		t.Fatalf("valid token: email=%q err=%v", email, err)
	}

	bad := map[string]func(map[string]any){
		"expired":       func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() },
		"wrong aud":     func(c map[string]any) { c["aud"] = "other-app" },
		"wrong issuer":  func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		"unverified":    func(c map[string]any) { c["email_verified"] = false },
		"missing email": func(c map[string]any) { delete(c, "email") },
	}
	for name, mutate := range bad {
		c := base()
		mutate(c)
		if _, err := v.Email(good.token(t, c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := v.Email(rogue.token(t, base())); err == nil {
		t.Error("token signed by an unknown key was accepted")
	}
	if _, err := v.Email("not.a.token"); err == nil {
		t.Error("garbage accepted")
	}
}

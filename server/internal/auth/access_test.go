package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	issuer = "https://team.cloudflareaccess.com"
	aud    = "app-aud-tag"
)

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
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func claims(email string, audience any, exp time.Time) map[string]any {
	return map[string]any{"iss": issuer, "aud": audience, "email": email,
		"iat": time.Now().Unix(), "exp": exp.Unix(), "sub": "user-id"}
}

func TestAccess(t *testing.T) {
	good := newSigner(t, "k1")
	rogue := newSigner(t, "k1") // same kid, different key
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&good.key.PublicKey}}
	v := newAccessVerifier(oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: aud}), []string{"Her@Example.com"})

	var seen string
	h := Access(v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = Email(r.Context()) }))
	hour := time.Now().Add(time.Hour)

	cases := []struct {
		name   string
		header string
		cookie string
		want   int
	}{
		{"valid header", good.token(t, claims("her@example.com", aud, hour)), "", 200},
		{"valid cookie", "", good.token(t, claims("HER@example.com", []string{aud}, hour)), 200},
		{"no token", "", "", 401},
		{"garbage", "not.a.jwt", "", 401},
		{"wrong audience", good.token(t, claims("her@example.com", "other-app", hour)), "", 401},
		{"expired", good.token(t, claims("her@example.com", aud, time.Now().Add(-time.Minute))), "", 401},
		{"forged signature", rogue.token(t, claims("her@example.com", aud, hour)), "", 401},
		{"email not allowed", good.token(t, claims("someone@else.com", aud, hour)), "", 401},
		{"no email", good.token(t, claims("", aud, hour)), "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequest("GET", "/", nil)
			if c.header != "" {
				req.Header.Set("Cf-Access-Jwt-Assertion", c.header)
			}
			if c.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: c.cookie})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req.WithContext(context.Background()))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			if c.want == 200 && seen != "her@example.com" {
				t.Fatalf("email in context = %q", seen)
			}
		})
	}
}

// The open side must not be a way into the app: a traversal attempt under an
// open prefix gets a redirect to the cleaned path (which then goes through
// the guarded side like any other request) or a 404, never app content.
func TestExemptDoesNotLeak(t *testing.T) {
	open := http.NewServeMux()
	open.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	h := Exempt([]string{"/healthz", "/feed/"}, open, guarded)
	for path, ok := range map[string]func(int) bool{
		"/healthz":              func(c int) bool { return c == 200 },
		"/settings":             func(c int) bool { return c == 401 },
		"/feed/../settings":     func(c int) bool { return c == 301 || c == 307 || c == 404 },
		"/feed/%2e%2e/settings": func(c int) bool { return c == 301 || c == 307 || c == 404 },
		"/feed/token/all.xml":   func(c int) bool { return c == 404 },
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if !ok(rec.Code) {
			t.Errorf("%s: unexpected %d", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" && loc != "/settings" {
			t.Errorf("%s: redirect to %q", path, loc)
		}
	}
}

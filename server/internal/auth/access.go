package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// AccessVerifier checks Cloudflare Access application tokens.
type AccessVerifier struct {
	v       *oidc.IDTokenVerifier
	allowed []string
}

// NewAccessVerifier verifies tokens issued by https://<teamDomain> for the
// Access application with the given AUD tag. Signing keys are fetched from
// the team's certs endpoint and cached; an unknown key id triggers a refetch,
// so Cloudflare's key rotation needs nothing from us.
func NewAccessVerifier(ctx context.Context, teamDomain, aud string, allowedEmails []string) *AccessVerifier {
	issuer := "https://" + strings.TrimSuffix(strings.TrimPrefix(teamDomain, "https://"), "/")
	keys := oidc.NewRemoteKeySet(ctx, issuer+"/cdn-cgi/access/certs")
	return newAccessVerifier(oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: aud}), allowedEmails)
}

func newAccessVerifier(v *oidc.IDTokenVerifier, allowed []string) *AccessVerifier {
	lower := make([]string, len(allowed))
	for i, e := range allowed {
		lower[i] = strings.ToLower(e)
	}
	return &AccessVerifier{v: v, allowed: lower}
}

// Verify returns the caller's email if the token is valid (signature,
// issuer, audience, expiry) and the email is on our allowlist. The allowlist
// duplicates the Access policy on purpose: a policy edited in the dashboard
// shouldn't be enough to let someone new in.
func (a *AccessVerifier) Verify(ctx context.Context, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	tok, err := a.v.Verify(ctx, raw)
	if err != nil {
		return "", false
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := tok.Claims(&claims); err != nil || claims.Email == "" {
		return "", false
	}
	email := strings.ToLower(claims.Email)
	if !slices.Contains(a.allowed, email) {
		return "", false
	}
	return email, true
}

// Access requires a valid Cloudflare Access token on every request. The app
// listens on the tailnet address, so any tailnet peer can reach it without
// passing through Access; checking the signature, not just the header's
// presence, is what makes that useless.
func Access(a *AccessVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Cf-Access-Jwt-Assertion")
		if raw == "" {
			if c, err := r.Cookie("CF_Authorization"); err == nil {
				raw = c.Value
			}
		}
		email, ok := a.Verify(r.Context(), raw)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithEmail(r.Context(), email)))
	})
}

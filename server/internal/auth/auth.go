// Package auth guards the public listener: who is calling (Cloudflare
// Access identity, or the loopback-only dev bypass) and same-origin checks
// for state-changing requests.
package auth

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey struct{}

// Email is the authenticated caller's address.
func Email(ctx context.Context) string {
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}

func WithEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, ctxKey{}, email)
}

// Dev authenticates every request as email. config refuses to start with
// this on a non-loopback bind.
func Dev(email string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithEmail(r.Context(), email)))
	})
}

// SameOrigin rejects state-changing requests whose Origin isn't ours. The
// Access cookie is the only session, so this is the CSRF defence.
func SameOrigin(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !strings.EqualFold(r.Header.Get("Origin"), origin) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Exempt serves paths with one of the given prefixes from bypass instead of
// guarded (the feed and health check skip Access).
func Exempt(prefixes []string, bypass, guarded http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				bypass.ServeHTTP(w, r)
				return
			}
		}
		guarded.ServeHTTP(w, r)
	})
}

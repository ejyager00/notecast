package config

import (
	"strings"
	"testing"
)

func env(over map[string]string) func(string) string {
	base := map[string]string{
		"DATA_DIR":            "/tmp/x",
		"PUBLIC_BIND":         "127.0.0.1:8790",
		"WORKER_BIND":         "127.0.0.1:8791",
		"PUBLIC_ORIGIN":       "http://localhost:8790",
		"WORKER_TOKEN_SHA256": strings.Repeat("ab", 32),
		"DEV_AUTH_EMAIL":      "dev@example.com",
	}
	for k, v := range over {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestDevAuthRequiresLoopback(t *testing.T) {
	if _, err := parse(env(nil)); err != nil {
		t.Fatalf("loopback dev config rejected: %v", err)
	}
	for _, bind := range []string{"100.97.104.119:8790", "0.0.0.0:8790", ":8790"} {
		_, err := parse(env(map[string]string{"PUBLIC_BIND": bind}))
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("PUBLIC_BIND=%s with DEV_AUTH_EMAIL: err = %v", bind, err)
		}
		_, err = parse(env(map[string]string{"WORKER_BIND": bind}))
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("WORKER_BIND=%s with DEV_AUTH_EMAIL: err = %v", bind, err)
		}
	}
}

func TestNoAuthRefused(t *testing.T) {
	if _, err := parse(env(map[string]string{"DEV_AUTH_EMAIL": ""})); err == nil {
		t.Fatal("config with no auth accepted")
	}
}

func TestAccessConfig(t *testing.T) {
	c, err := parse(env(map[string]string{
		"DEV_AUTH_EMAIL": "", "PUBLIC_BIND": "100.97.104.119:8790",
		"CF_ACCESS_TEAM_DOMAIN": "t.cloudflareaccess.com", "CF_ACCESS_AUD": "aud",
		"ALLOWED_EMAILS": " A@x.com , b@y.com,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedEmails) != 2 || c.AllowedEmails[0] != "a@x.com" {
		t.Fatalf("emails %v", c.AllowedEmails)
	}
}

func TestBadTokenHash(t *testing.T) {
	if _, err := parse(env(map[string]string{"WORKER_TOKEN_SHA256": "nothex"})); err == nil {
		t.Fatal("bad hash accepted")
	}
}

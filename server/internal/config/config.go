// Package config reads the server's environment (ARCHITECTURE §4.2).
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	DataDir           string
	PublicBind        string
	WorkerBind        string
	PublicOrigin      string
	WorkerTokenSHA256 []byte
	AllowedEmails     []string
	DevAuthEmail      string
	AccessTeamDomain  string
	AccessAUD         string
	NotifyCmd         string // e.g. the box's notify.sh; empty = no alerts
}

func FromEnv() (*Config, error) { return parse(os.Getenv) }

func parse(get func(string) string) (*Config, error) {
	c := &Config{
		DataDir:          get("DATA_DIR"),
		PublicBind:       get("PUBLIC_BIND"),
		WorkerBind:       get("WORKER_BIND"),
		PublicOrigin:     strings.TrimRight(get("PUBLIC_ORIGIN"), "/"),
		DevAuthEmail:     get("DEV_AUTH_EMAIL"),
		AccessTeamDomain: get("CF_ACCESS_TEAM_DOMAIN"),
		AccessAUD:        get("CF_ACCESS_AUD"),
		NotifyCmd:        get("NOTIFY_CMD"),
	}
	for _, e := range strings.Split(get("ALLOWED_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			c.AllowedEmails = append(c.AllowedEmails, e)
		}
	}
	var errs []error
	need := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	need("DATA_DIR", c.DataDir)
	need("PUBLIC_BIND", c.PublicBind)
	need("WORKER_BIND", c.WorkerBind)
	need("PUBLIC_ORIGIN", c.PublicOrigin)
	if u, err := url.Parse(c.PublicOrigin); c.PublicOrigin != "" && (err != nil || u.Scheme == "" || u.Host == "" || u.Path != "") {
		errs = append(errs, errors.New("PUBLIC_ORIGIN must look like https://host"))
	}
	if h := get("WORKER_TOKEN_SHA256"); h == "" {
		errs = append(errs, errors.New("WORKER_TOKEN_SHA256 is required"))
	} else if b, err := hex.DecodeString(h); err != nil || len(b) != 32 {
		errs = append(errs, errors.New("WORKER_TOKEN_SHA256 must be 64 hex characters"))
	} else {
		c.WorkerTokenSHA256 = b
	}

	access := c.AccessTeamDomain != "" || c.AccessAUD != ""
	switch {
	case c.DevAuthEmail != "" && access:
		errs = append(errs, errors.New("set DEV_AUTH_EMAIL or CF_ACCESS_*, not both"))
	case c.DevAuthEmail != "":
		// The dev bypass skips all authentication, so it must never be reachable
		// from another machine.
		for name, bind := range map[string]string{"PUBLIC_BIND": c.PublicBind, "WORKER_BIND": c.WorkerBind} {
			if bind != "" && !IsLoopback(bind) {
				errs = append(errs, fmt.Errorf("DEV_AUTH_EMAIL requires loopback binds; %s is %s", name, bind))
			}
		}
	case access:
		if c.AccessTeamDomain == "" || c.AccessAUD == "" {
			errs = append(errs, errors.New("CF_ACCESS_TEAM_DOMAIN and CF_ACCESS_AUD go together"))
		}
		if len(c.AllowedEmails) == 0 {
			errs = append(errs, errors.New("ALLOWED_EMAILS is required with Cloudflare Access"))
		}
	default:
		errs = append(errs, errors.New("no authentication configured: set CF_ACCESS_* (production) or DEV_AUTH_EMAIL (local dev)"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// IsLoopback reports whether a host:port bind address is loopback-only.
func IsLoopback(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

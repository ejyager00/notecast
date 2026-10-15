// Command notecast is the notecast web server: the public listener (UI and
// feeds, behind Cloudflare Access) and the worker listener (/worker/v1).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.ericbits.win/eric/notecast/server/internal/auth"
	"git.ericbits.win/eric/notecast/server/internal/config"
	"git.ericbits.win/eric/notecast/server/internal/core"
	"git.ericbits.win/eric/notecast/server/internal/files"
	"git.ericbits.win/eric/notecast/server/internal/store"
	"git.ericbits.win/eric/notecast/server/internal/web"
	"git.ericbits.win/eric/notecast/server/internal/workerapi"
)

// version is set at build time: -ldflags "-X main.version=<git sha>".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config:\n%w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "app.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	fs, err := files.New(cfg.DataDir)
	if err != nil {
		return err
	}
	svc := core.New(db, fs, nil)
	go svc.RunReaper(ctx, 30*time.Second, func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) })

	app, open, err := (&web.Web{Svc: svc, Log: log, Version: version, Origin: cfg.PublicOrigin}).Handlers()
	if err != nil {
		return err
	}
	app = auth.SameOrigin(cfg.PublicOrigin, app)
	if cfg.DevAuthEmail != "" {
		log.Warn("DEV_AUTH_EMAIL set: authentication is bypassed (loopback only)", "email", cfg.DevAuthEmail)
		app = auth.Dev(cfg.DevAuthEmail, app)
	} else {
		app = auth.Access(auth.NewAccessVerifier(ctx, cfg.AccessTeamDomain, cfg.AccessAUD, cfg.AllowedEmails), app)
	}
	public := auth.Exempt(web.OpenPrefixes, open, app)

	servers := []*http.Server{
		{
			Addr: cfg.PublicBind, Handler: public,
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute,
		},
		{
			Addr: cfg.WorkerBind, Handler: (&workerapi.API{Svc: svc, TokenSHA256: cfg.WorkerTokenSHA256, Log: log}).Handler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Minute, IdleTimeout: 2 * time.Minute,
		},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("listening", "addr", s.Addr, "version", version)
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		return err
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdownCtx)
	}
	return nil
}

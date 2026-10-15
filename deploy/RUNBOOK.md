# notecast runbook

Server on asahihome, worker on roundabits. Box-wide rules (hard rules,
SELinux, firewall, backups) are in `~/homelab-docs` on asahihome and win
over anything here.

## First deploy (Phase 5)

Steps marked **root** need Eric.

1. **Data directory** (asahihome):
   ```sh
   mkdir -p /mnt/data/volumes/notecast ~/opt/notecast/releases
   chmod 750 /mnt/data/volumes/notecast
   ```
2. **Worker token** (roundabits). The server only ever sees its hash:
   ```sh
   mkdir -p ~/.config/notecast
   (umask 077; head -c 32 /dev/urandom | base64 | tr -d '=+/\n' > ~/.config/notecast/token)
   sha256sum < ~/.config/notecast/token | cut -d' ' -f1   # -> WORKER_TOKEN_SHA256
   ```
   Save the token in Vaultwarden ("notecast worker token").
3. **Env file** (asahihome), mode 0600, outside git, backed up with the volume:
   ```sh
   (umask 077; cat > /mnt/data/volumes/notecast/notecast.env <<EOT
   ALLOWED_EMAILS=ejyager00@protonmail.com
   WORKER_TOKEN_SHA256=<hash from step 2>
   EOT
   )
   ```
   Add her address to `ALLOWED_EMAILS` at Phase 6, at the same time as the
   Access policy.
4. **Cloudflare**: follow `deploy/cloudflare.md`; put the team domain and AUD
   into `deploy/notecast.service`.
5. **Binary**: `just deploy` (builds, copies, repoints `current`, restarts).
   The first time, install the unit before restarting:
   ```sh
   scp deploy/notecast.service asahihome:.config/containers/systemd/
   ssh asahihome 'ln -sf ~/.config/containers/systemd/notecast.service ~/.config/systemd/user/ \
     && systemctl --user daemon-reload && systemctl --user enable --now notecast'
   ```
6. **Caddy**: add `deploy/Caddyfile.snippet` next to the `git.yager.me` block,
   then `systemctl --user restart caddy`.
7. **Backups**: add `deploy/dump-services.snippet.sh` to `dump-services.sh`
   (instructions in the file). **root**: append `deploy/restic-excludes.txt` to
   `/etc/restic/excludes`.
8. **Box docs**: `docs/services/notecast.md`, `INDEX.md`, `_sidebar.md`,
   changelog entry, and in `landing/services.json`:
   ```json
   {"name": "notecast", "host": "notecast.yager.me",
    "probe": "http://100.97.104.119:8790/healthz",
    "desc": "Handwritten notes to narrated audio. Public behind Cloudflare Access."}
   ```
9. **Verify** (each must hold):
   - `readelf -l` alignment ok and `systemctl --user status notecast` active.
   - `ausearch -m avc -ts recent` clean after a real upload (**root** to read).
   - From cellular: login works; a note goes upload → review → ready.
   - `curl -s -o /dev/null -w '%{http_code}' http://100.97.104.119:8790/` from a
     tailnet device → `401`.
   - `curl -sI https://notecast.yager.me/feed/x/all.xml` → `404` from the app.
   - `dump-services.sh` writes a `quick_check`-clean notecast snapshot.

## Routine deploy

```sh
just deploy        # build arm64, scp, repoint current, restart, health check
```

Before a release that adds a migration (`server/internal/store/migrations`):
`ssh asahihome '~/.config/containers/systemd/dump-services.sh upgrade-from-<old sha>'`.

## Rollback

```sh
ssh asahihome 'cd ~/opt/notecast/releases && ls -t | head'   # pick the previous one
ssh asahihome 'ln -sfn ~/opt/notecast/releases/notecast-<sha> ~/opt/notecast/releases/current \
  && systemctl --user restart notecast'
```

Across a migration, also restore the `upgrade-from-*` snapshot (migrations
are forward-only).

## Restore drill

```sh
mkdir -p /tmp/nc-restore && cd /tmp/nc-restore
# restic restore of /mnt/data/volumes/notecast and the newest dumps/notecast/*.db
cp <dump>.db app.db
DATA_DIR=/tmp/nc-restore PUBLIC_BIND=127.0.0.1:18790 WORKER_BIND=127.0.0.1:18791 \
  PUBLIC_ORIGIN=http://127.0.0.1:18790 DEV_AUTH_EMAIL=drill@localhost \
  WORKER_TOKEN_SHA256=$(printf x | sha256sum | cut -d' ' -f1) \
  ~/opt/notecast/releases/current &
curl -s http://127.0.0.1:18790/ | grep -c 'class="note"'   # matches production's note count
```

## Worker (roundabits)

- Config: `~/.config/notecast/worker.toml` (start from `deploy/worker.toml.example`).
- Token: `~/.config/notecast/token` (step 2 above; restore from Vaultwarden).
- Models: `just fetch-models` (sha256-verified, ~35 GB with all candidates).
- Run by hand: `just worker`. As a service: Phase 7 (Shepherd, via home-config.scm).
- **CPU only.** Never set a GPU device without `NOTECAST_ALLOW_GPU=1`, and
  don't set that until the amdgpu device-loss problem is fixed
  (ARCHITECTURE §11a).

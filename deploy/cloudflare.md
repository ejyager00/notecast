# Cloudflare setup for notecast.yager.me

All of this is dashboard state (the tunnel is remotely managed, like the rest
of asahihome's public hostnames). Record the values marked **record** here
when you do it, so it can be rebuilt by hand.

## 1. Tunnel public hostname

Zero Trust → Networks → Tunnels → (asahihome tunnel) → Public Hostname → Add:

| Field | Value |
|---|---|
| Subdomain | `notecast` |
| Domain | `yager.me` |
| Service | `HTTP` → `caddy:80` |

`caddy`, not `localhost`: cloudflared runs in its own container and reaches
Caddy by name over `home.network` (homelab-docs services/cloudflared.md).

## 2. Access application A: the app

Zero Trust → Access → Applications → Add → Self-hosted:

| Field | Value |
|---|---|
| Name | `notecast` |
| Domain | `notecast.yager.me` (no path) |
| Session duration | `1 month` |
| Identity providers | One-time PIN (email) |
| Policy | Allow, Include → Emails → **Eric's only at first**; add hers at Phase 6 |

After saving, open the application's overview and copy:

- **record** the AUD tag → `CF_ACCESS_AUD` in `notecast.service`
- **record** the team domain (`<team>.cloudflareaccess.com`) → `CF_ACCESS_TEAM_DOMAIN`

## 3. Access application B: the feed bypass

Add → Self-hosted:

| Field | Value |
|---|---|
| Name | `notecast feed` |
| Domain | `notecast.yager.me`, path `feed` |
| Policy | **Bypass**, Include → Everyone |

The more specific path wins, so B applies to `/feed/*` only. Podcast apps
can't log in; the 256-bit token in the feed URL is the protection there.

## 4. Check

From a phone on cellular (not the tailnet):

- `https://notecast.yager.me/` → Cloudflare login page, then the app.
- `https://notecast.yager.me/feed/nope/all.xml` → the app's own 404, without a
  login page in between (proves the bypass is scoped).

Values recorded:

- Team domain: _______________
- AUD tag (app A): _______________
- Date set up: _______________

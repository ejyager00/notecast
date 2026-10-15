# notecast — Architecture

Photos of handwritten notes in, narrated audio out. One listener (Eric's
girlfriend, iPhone, not on the tailnet), one admin (Eric). The web app lives on
`asahihome`; the GPU work runs on `roundabits` and pulls jobs from a queue.

**This document is the source of truth for *what* and *why*.**
`IMPLEMENTATION-PLAN.md` covers *in what order* and *how you know it worked*.
For anything that happens on `asahihome`, the box's own knowledge base
(`~/homelab-docs` on that machine, especially `docs/conventions.md`,
`docs/network.md` and `docs/backups.md`) takes precedence over this file. If
this file disagrees with it, this file is wrong. Say so; don't work around it.

Placeholders are marked **TBD**. They are listed in §12.

---

## 0. Goals and non-goals

Goals:

- She photographs pages on her iPhone, uploads them, and gets an MP3 she can
  play in the browser, download, or receive automatically in a podcast app.
- Audio is **narration**, not a verbatim read: bullets, arrows, abbreviations
  and headings get turned into speakable prose.
- A **review step** between OCR and TTS where she can fix misreads and edit the
  narration. It can be skipped with an auto-approve setting.
- Finding notes by topic: full-text search plus collections.
- Local vision model by default. A cloud model can be swapped in **per stage**
  through config, with no code change.
- Job status that is honest: "queued, processing machine offline since 14:02"
  rather than an endless spinner.
- Backed up through the box's existing restic and `dump-services.sh` path.

Non-goals:

- Multiple users or accounts. Access holds the allowlist; the app has no user
  table.
- Running OCR or TTS on `asahihome` (7.4 GB RAM, which is the box's binding
  constraint).
- A SPA, a JS build step, or a PWA install (see §7.4 for why).
- Voice cloning. It's a possible later TTS backend; nothing here precludes it.

---

## 1. Topology and trust boundaries

```
 her iPhone (Safari / podcast app)
        │ HTTPS
        ▼
 Cloudflare edge ── Access: email OTP allowlist on everything except /feed/*
        │ tunnel (outbound-only, existing cloudflared container)
        ▼
 asahihome: caddy container (http://notecast.yager.me block, plain HTTP)
        │ 100.97.104.119:8790   (public listener: UI + feed)
        ▼
 notecast-server (Go, systemd user unit) ── SQLite + files, /mnt/data/volumes/notecast
        ▲ 100.97.104.119:8791   (worker listener: /worker/v1/*, bearer token)
        │ tailnet, long-poll, outbound from roundabits only
 roundabits: notecast-worker (Python, Shepherd home service)
        ├── llama-server (Guix llama-cpp, Vulkan0 = RX 6900 XT), spawned on demand
        └── Kokoro-82M via onnxruntime (CPU)
```

Trust boundaries:

| Boundary | Control | Notes |
|---|---|---|
| Internet → UI | Cloudflare Access (email OTP) **and** the app verifying the `Cf-Access-Jwt-Assertion` signature | The app binds a tailnet address (Caddy is a rootless container and can't reach host loopback; see `homelab-docs/services/workout.md`). Any tailnet peer can therefore reach the port directly, and JWT verification is what makes going around Access useless. Checking that the header is present is **not** enough: verify signature, `aud` and `exp`, and check the email against the allowlist. |
| Internet → feed | Access **bypass** on `/feed/*`, with a 256-bit capability token in the path | Podcast apps can't complete an Access login. The same capability-URL model is what commercial private podcast feeds use. Rotatable. |
| roundabits → worker API | Separate listener, never routed through Caddy, bearer token | The server stores only `sha256(token)`, so the asahihome side holds no secret. |
| Worker → image decoding | Untrusted image bytes are decoded only on roundabits | The server sniffs magic bytes and never decodes. HEIC parsing (libheif) is the riskiest parser here, and it stays off the internet-facing box. |

---

## 2. Domain model

**Note**: one study note, made of one or more pages, with one narration and at
most one current audio file. A note belongs to zero or one **Collection**
(e.g. a class or subject; she creates these). **Tags** are free-form and
suggested by the narration stage.

**Page**: one uploaded photo plus its per-page transcript. Pages are ordered
within a note.

**Job**: one unit of worker execution against a note. A note has **at most one
active job** (enforced by a partial unique index, §4.4), which removes all
concurrency questions about editing a note while the worker writes to it.

### 2.1 Note status (authoritative, on `notes.status`)

```
          submit                     done (auto_approve=off)
uploading ───────▶ transcribing ─────────────────────────▶ review ◀──────────┐
                        │ done (auto_approve=on)              │  │            │
                        ▼                                     │  │ "rewrite   │ done
                     speaking ◀──────────── approve ──────────┘  │ narration" │
                        │ done                                   ▼            │
                        ▼                                    narrating ───────┘
                      ready ── edit transcript/narration ──▶ review
any active ── fails max_attempts ──▶ failed(failed_stage) ── retry ──▶ that stage
```

- Queued vs running is not a separate status. The UI shows it from the active
  job's state.
- While a job is active the note is **read-only** in the UI, with a Cancel
  button. Cancel marks the job `cancelled`, and the worker's next heartbeat
  gets 409 and aborts.
- Editing a `ready` note moves it to `review`. **The old audio stays live in the
  player and the feed until new audio replaces it.**
- Adding pages to a note in `review`/`ready` enqueues `transcribe` for only the
  pages with no transcript. Narration is then regenerated from the whole note.
- `narration_stale` is set when the transcript is edited after narration was
  generated. The review screen then pushes "Rewrite narration from transcript"
  instead of letting a stale narration be approved silently.

### 2.2 Job kinds

| Kind | Input | Output | Needs GPU |
|---|---|---|---|
| `transcribe` | page images (by page id) | normalized display JPEG + thumbnail per page, per-page transcript, **plus** a narration draft and title/summary/tags | yes (local) |
| `narrate` | full transcript | narration + title/summary/tags | yes (local) |
| `speak` | narration, voice, speed | MP3, duration, chapter list | no (CPU) |

`transcribe` chains OCR and narration in a single job because the same model is
already loaded. The two stages still have separate prompts and separate
provider config (§3.4), so they can point at different models.

---

## 3. Pipeline

### 3.1 Image normalization (worker)

- Decode: JPEG/PNG/WebP via Pillow; HEIC via libheif (`heif-dec`, Guix
  `libheif`). iOS Safari usually transcodes HEIC to JPEG on upload, but not
  always, so handle both.
- Apply the EXIF orientation, convert to RGB, and downscale the long edge to a
  configurable size (default 2048; Phase 0 picks the value that gives the best
  OCR).
- Emit `display.jpg` (long edge 2048, q85) and `thumb.jpg` (long edge 400) and
  post them back. The server serves these and keeps the original untouched.
- Phase 0 evaluates contrast or grayscale preprocessing. Don't add it unless
  it measurably helps.

### 3.2 OCR stage (vision model)

Prompt file: `worker/prompts/ocr.v1.md`. The output is **verbatim Markdown per
page**:

- Keep the structure: headings, bullets, numbering, indentation, underlines as
  emphasis.
- `[?word]` marks a word the model is unsure of. `[illegible]` marks text it
  can't read. `[diagram: one-line description]` marks drawings. The review UI
  highlights `[?…]` and `[illegible]` so she can find them quickly.
- Never correct spelling or content. This stage is evidence, not editing.

Call it with temperature 0 and one page per request; no multi-page context.
Pages are independent, which keeps retries cheap and errors local.

### 3.3 Narration stage (text model)

Prompt file: `worker/prompts/narrate.v1.md`. The input is the full transcript
in page order. The output is constrained JSON (llama-server supports
`response_format` with a JSON schema; cloud providers do too):

```json
{"title": "...", "summary": "1–2 sentences", "tags": ["..."],
 "narration": "## Section heading\nSpoken prose...\n\n## Next section\n..."}
```

Rules the prompt enforces:

- **Faithful.** Never add facts, examples or explanations that aren't in the
  notes. It rephrases for the ear and does not teach.
- Turn bullets into sentences with light transitions. Read headings as section
  introductions. Turn arrows and relationships into words ("which leads to").
- Expand abbreviations only when unambiguous in context, and otherwise leave
  them as written. Spell out symbols, units and simple math so they read
  naturally ("x squared", "approximately").
- Drop `[?word]` markers but keep the best-guess word. For `[illegible]`, either
  skip it or say "one word unreadable", whichever the setting asks for. For
  diagrams: "There's a diagram here showing …".
- `## ` lines split sections, which become audio chapters (§3.5).

`narration` is stored and edited as **plain text with `## ` headings**, a
format that's easy to edit in one textarea on a phone. Section boundaries are
recomputed from it at `speak` time, so editing can't desync the structure.

### 3.4 Provider seam

Each stage (`ocr`, `narrate`) names a provider in `worker.toml`. A provider is
an **OpenAI-compatible Chat Completions** endpoint: `base_url`, `model`, an
optional `api_key_file`, and optional `managed = true` (meaning the worker owns
the llama-server process, §6.3). That one interface covers llama-server, OpenAI,
Gemini's OpenAI-compatible endpoint, OpenRouter and vLLM. Images are sent as
`image_url` data URIs.

```toml
[providers.local]
kind = "openai"
base_url = "http://127.0.0.1:8092/v1"
model = "local"            # llama-server ignores the name
managed = true

[providers.cloud]
kind = "openai"            # or "anthropic" once that adapter exists
base_url = "https://…"
model = "…"
api_key_file = "~/.config/notecast/cloud_key"

[stages]
ocr = "local"
narrate = "local"
```

A native Anthropic adapter (Messages API) is a ~60-line second implementation
of the same `ChatProvider` protocol. Write it when it's needed, not before.
Every result records `{provider, model, prompt_version}` in
`notes.provenance`, so a note can be re-run later and compared against a
different model.

Privacy is a config decision, not a code path. **Switching a stage to `cloud`
sends her notes to that provider.** Don't do that without her agreement, and
record it in `docs/changelog` if it happens.

### 3.5 Speech stage (TTS)

- **Kokoro-82M** (ONNX, `kokoro-onnx`) on CPU. On a 13700K that's faster than
  real time, so the GPU is never needed for TTS. The 24 kHz mono output is fine
  for speech.
- Voice and speed are global defaults in Settings and can be overridden per
  note. She picks the voice in Phase 0.
- Synthesize sentence by sentence (Kokoro's phoneme limit is ~510 tokens per
  pass). Insert 0.5 s of silence between paragraphs and 1.0 s between
  sections. **Record the start time of each section**, which gives the chapter
  list.
- Encode with ffmpeg: `loudnorm=I=-16:TP=-1.5:LRA=11` (the usual podcast
  loudness target), mono, 44.1 kHz, `libmp3lame` **CBR** 64 kbps. CBR because
  podcast apps seek accurately in constant-bitrate MP3 and less so in VBR. MP3 rather than
  Opus, so it works in every iOS player, plain downloads included.
- Embed chapters as ID3 `CHAP`/`CTOC` frames (mutagen) **and** emit a
  Podcasting 2.0 chapters JSON. Podcast apps differ in which of the two they
  read, so Phase 6 checks what her actual app supports.
- The TTS engine sits behind a `SpeechEngine` protocol (`synthesize(text,
  voice, speed) -> pcm`), so a GPU voice-cloning model can come later without
  touching anything else.

---

## 4. Server (`server/`, Go)

### 4.1 Shape

One static binary (`CGO_ENABLED=0`), cross-compiled on roundabits with Guix's
Go (`GOOS=linux GOARCH=arm64`). This is easy in Go, unlike the Rust/musl
problem workout-pwa-rs ran into. Templates and static assets are embedded with
`embed.FS`. Dependencies are kept small:

- stdlib `net/http` (1.22+ method/path patterns), `html/template`, `log/slog`
- `modernc.org/sqlite`: pure Go, so no cgo, and it includes FTS5
- `github.com/coreos/go-oidc/v3`: verifies Access JWTs against the team's JWKS
- vendored `htmx.min.js`, served from the binary with no CDN

```
server/
  cmd/notecast/        main: config, two listeners, migrate, reaper, shutdown
  internal/store/      db open (WAL, busy_timeout, foreign_keys), embedded migrations
  internal/notes/      state machine: pure transition funcs, exhaustively unit-tested
  internal/jobs/       enqueue, claim (long-poll), heartbeat, complete, fail, reaper
  internal/files/      on-disk layout, atomic writes (tmp + fsync + rename)
  internal/web/        public UI handlers + templates
  internal/workerapi/  /worker/v1/* handlers
  internal/feed/       RSS + chapters JSON
  internal/auth/       Access JWT middleware, Origin check, dev bypass
  migrations/          0001_init.sql, … (forward-only; never edit an applied one)
  web/templates/ web/static/
```

### 4.2 Configuration (environment, set in the unit)

| Var | Example | Notes |
|---|---|---|
| `DATA_DIR` | `/mnt/data/volumes/notecast` | |
| `PUBLIC_BIND` | `100.97.104.119:8790` | **TBD**: confirm the port is free (`ss -ltn`) at deploy time |
| `WORKER_BIND` | `100.97.104.119:8791` | same |
| `PUBLIC_ORIGIN` | `https://notecast.yager.me` | Used for absolute feed URLs and the Origin check |
| `CF_ACCESS_TEAM_DOMAIN` | `<team>.cloudflareaccess.com` | **TBD** |
| `CF_ACCESS_AUD` | Access app AUD tag | Not a secret |
| `ALLOWED_EMAILS` | `ejyager00@…,<hers>` | Checked in addition to Access, as defense in depth |
| `WORKER_TOKEN_SHA256` | hex | The hash only, so the server unit holds no secret |
| `NTFY_URL` | existing ntfy topic | Used for the worker-offline alert (§4.8) |
| `DEV_AUTH_EMAIL` | dev only | Skips JWT verification. **The binary refuses to start if this is set and either bind address is not loopback.** |

### 4.3 On-disk layout

```
/mnt/data/volumes/notecast/
  app.db  (+ -wal, -shm: excluded from restic; the dump is the restorable copy)
  pages/<note_pid>/<page_id>.orig.{jpg,heic,png,webp}
  pages/<note_pid>/<page_id>.display.jpg
  pages/<note_pid>/<page_id>.thumb.jpg
  audio/<note_pid>/<audio_version>.mp3
  audio/<note_pid>/<audio_version>.chapters.json
  tmp/   same filesystem as the rest, so renames are atomic; swept at startup
```

`note_pid` is a random 12-character public id used in every URL. Integer ids
never leave the database.

### 4.4 Schema (0001_init.sql, abridged)

```sql
CREATE TABLE collections (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);

CREATE TABLE notes (
  id INTEGER PRIMARY KEY,
  pid TEXT NOT NULL UNIQUE,
  collection_id INTEGER REFERENCES collections(id) ON DELETE SET NULL,
  status TEXT NOT NULL CHECK (status IN
    ('uploading','transcribing','review','narrating','speaking','ready','failed')),
  failed_stage TEXT, error TEXT,
  title TEXT, summary TEXT, tags TEXT NOT NULL DEFAULT '[]',   -- JSON array
  narration TEXT, narration_stale INTEGER NOT NULL DEFAULT 0,
  voice TEXT, speed REAL,                                       -- NULL = defaults
  audio_version INTEGER, audio_bytes INTEGER, audio_seconds REAL,
  audio_chapters TEXT,                                          -- JSON
  audio_published_at TEXT,
  provenance TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL);

CREATE TABLE pages (
  id INTEGER PRIMARY KEY,
  note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  orig_ext TEXT NOT NULL, orig_sha256 TEXT NOT NULL, orig_bytes INTEGER NOT NULL,
  has_display INTEGER NOT NULL DEFAULT 0,
  transcript TEXT,
  UNIQUE (note_id, position));

CREATE TABLE jobs (
  id INTEGER PRIMARY KEY,
  note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('transcribe','narrate','speak')),
  state TEXT NOT NULL CHECK (state IN ('queued','leased','done','failed','cancelled')),
  params TEXT NOT NULL DEFAULT '{}',          -- e.g. page ids, voice, speed
  attempt INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 3,
  not_before TEXT,                            -- retry backoff
  lease_token TEXT, lease_expires_at TEXT, worker_id TEXT,
  progress TEXT, error TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX jobs_one_active_per_note ON jobs(note_id)
  WHERE state IN ('queued','leased');
CREATE INDEX jobs_claimable ON jobs(state, not_before, id);

CREATE TABLE workers (
  id TEXT PRIMARY KEY, version TEXT, last_seen_at TEXT NOT NULL,
  state TEXT,             -- idle | busy | gpu_unavailable | paused
  detail TEXT);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- auto_approve, default_voice, default_speed, illegible_policy,
-- feed_include_transcript, feed_token

CREATE VIRTUAL TABLE notes_fts USING fts5(
  title, tags, summary, transcript, narration,
  tokenize = 'porter unicode61');   -- rowid = notes.id
```

`notes_fts` is maintained by application code in the same transaction as every
note write (one `upsertFTS(tx, noteID)` call, never triggers spread across
tables). `transcript` in the FTS row is the pages' transcripts concatenated in
order.

### 4.5 Public routes (behind Access, except `/feed/`)

| Route | Purpose |
|---|---|
| `GET /` | Library: search box, collection filter, list (title, collection, status badge, duration, ▶) |
| `GET /search?q=&c=` | htmx partial. FTS5 `MATCH` with a prefix `*` on the last token, `bm25(notes_fts, 10, 5, 3, 1, 1)`, `snippet()` with highlights |
| `GET /new`, `POST /notes` | Create a note (choose a collection), then the page uploader |
| `POST /notes/{pid}/pages` | **One page per request**, so each photo uploads with its own progress and failure on cellular. `http.MaxBytesReader` at 25 MB; magic-byte sniff; streamed to `tmp/` then renamed |
| `POST /notes/{pid}/pages/{id}/move`, `DELETE …` | Reorder (up/down buttons) and remove |
| `POST /notes/{pid}/submit` | `uploading → transcribing` |
| `GET /notes/{pid}` | Note page. **Review mode**: each page image stacked above its editable transcript, `[?…]` highlighted, then the narration textarea, Save / Rewrite narration / Approve. **Ready mode**: player, speed buttons (1×, 1.25×, 1.5×, 2×), Download, Edit |
| `GET /notes/{pid}/status` | htmx partial polled `every 5s` only while a job is active; it stops itself by returning a version without `hx-trigger` once the note reaches a terminal state |
| `POST /notes/{pid}/{approve,rewrite,retry,cancel}` | Status transitions (§2.1) |
| `GET /notes/{pid}/audio` | `http.ServeContent`: Range + HEAD support, **which iOS requires for audio**. `?download=1` adds `Content-Disposition: attachment; filename="<title>.mp3"` |
| `GET /notes/{pid}/pages/{id}/{display,thumb,orig}` | Images. Before normalization `display` falls back to `orig` (Safari renders HEIC natively; other browsers show a placeholder) |
| `GET/POST /settings` | Worker status, auto-approve, voice and speed defaults, illegible policy, collections, feed URLs, **rotate feed token** |
| `GET /healthz` | `ok` plus DB ping, with no auth (for landing-page probes over the tailnet) |
| `GET /feed/{token}/…` | §4.7 |

Every non-GET request must pass the `Origin == PUBLIC_ORIGIN` check (CSRF).
Access's cookie is the only session, and the app sets no cookies of its own.

The player uses `<audio preload="metadata">` with Media Session metadata
(title, collection, artwork), so the iOS lock screen shows what's playing.
Background playback in Safari is supported for audio elements; Phase 6 confirms
it on her phone.

### 4.6 Worker API (`WORKER_BIND`, `/worker/v1`, `Authorization: Bearer`)

| Endpoint | Behavior |
|---|---|
| `POST /claim` `{worker_id, version, kinds:[…], state}` | Updates `workers.last_seen_at`. Long-polls up to 25 s for a claimable job of the offered `kinds` (`state='queued' AND (not_before IS NULL OR not_before <= now)`, oldest first). On a claim: `state=leased`, `attempt++`, a new `lease_token`, `lease_expires_at = now+120s`, and the note moves to its stage status. Returns `{job, lease_token, inputs}` or 204. Waiting is done with an in-process notify channel that enqueue signals, not by polling SQLite. |
| `GET /jobs/{id}/inputs/{name}` | Page originals for `transcribe`. Requires the current lease token. |
| `POST /jobs/{id}/heartbeat` `{lease_token, progress}` | Extends the lease by 120 s. Returns **409** if the lease is no longer theirs (cancelled or reaped), and the worker aborts. Also refreshes `last_seen_at`. |
| `POST /jobs/{id}/complete` multipart: `result.json` + files | Validated against the current lease token, so **a stale completion from a reaped lease is rejected, never applied**. Files land in `tmp/` and are renamed into place, then one transaction updates the note, pages, FTS and the job and applies the status transition. |
| `POST /jobs/{id}/fail` `{lease_token, error, retryable}` | Retryable with attempts left: `queued` with `not_before = now + 30s·2^attempt`. Otherwise the job is `failed` and the note moves to `failed(failed_stage)`. |

A reaper goroutine runs every 30 s and treats expired leases as retryable
failures. The worker sends `kinds` based on what it can do **right now**: it
leaves out `transcribe` and `narrate` when the GPU guard trips (§6.3), so
`speak` jobs keep flowing while Eric is gaming.

### 4.7 Feeds

- One feed token, 32 random bytes base64url. **Stored as-is, not hashed**
  (changed during Phase 6): anyone who can read the database can already read
  every note, so a hash protects nothing, and keeping the token lets Settings
  show the feed URLs whenever they're needed. Compared in constant time.
  Rotating it invalidates every feed URL; she then re-adds the feed.
- `GET /feed/{token}/all.xml` lists every `ready` note. `GET
  /feed/{token}/c/{collection_id}.xml` serves one feed per collection. **Each
  collection appears in her podcast app as its own show**, which is the main
  way to organize content inside a podcast app.
- RSS 2.0 with the `itunes` and `podcast` namespaces. `<itunes:block>Yes`
  and `<podcast:locked>yes` are set. There's static square cover art (PNG,
  1400×1400, embedded in the binary).
- Item: title; description = summary + tags (full transcript only if
  `feed_include_transcript`, default **off**, see below); `enclosure` (absolute
  URL, length, `audio/mpeg`); `itunes:duration`; `podcast:chapters`.
- **GUID = `<note_pid>-v<audio_version>`.** Regenerated audio shows up as a new
  episode and the old one drops off. Podcast apps don't reliably re-download
  an enclosure whose URL changes under the same GUID.
- Media responses carry `Cache-Control: private, max-age=0`, which keeps them
  out of Cloudflare's edge cache even though `.mp3` is a cacheable extension.
- Several podcast apps (Overcast, Pocket Casts, and possibly Apple) fetch feeds
  **from their own servers**, so that service sees the feed contents and the
  token. That's why the feed carries summaries rather than transcripts by
  default. Phase 6 logs who actually fetches the feed (User-Agent and
  `CF-Connecting-IP`).

**Search lives in the web app, not the feed.** A private feed is in no
directory, so nobody can find it, but she can't search it well either: inside
podcast apps, search covers little beyond episode titles. The feed is a
delivery and listening channel. Finding things means the library search
(FTS over everything, ranked, with snippets) and collections. Each result
links to the note, which has a player.

### 4.8 Worker-offline alerting

If any job has been `queued` for more than 2 h and no worker has been seen for
more than 2 h, POST once to `NTFY_URL` ("notecast: 3 notes waiting,
roundabits offline since …"). Re-arm after a worker is seen again. Eric gets
pinged to turn the desktop on, and she sees the same information on the note
page.

---

## 5. Concurrency and failure model, summarized

- Exactly one active job per note (the database enforces it). One worker
  process with one job at a time. Scaling past that is a non-goal.
- At-least-once execution with idempotent completion: a redelivered job
  recomputes and overwrites, and stale lease tokens can't commit.
- Worker crash mid-job: the lease expires after 120 s, the job is retried with
  backoff, and fails permanently after 3 attempts with the error shown on the
  note and a Retry button.
- Server restart mid-claim: long-poll requests drop, the worker reconnects
  after jittered 1–5 s backoff, and leases survive in SQLite.

---

## 6. Worker (`worker/`, Python, on roundabits)

### 6.1 Runtime

Everything here except two small Python libraries **is already in Guix**
(checked 2026-10-14):

| Need | Guix package |
|---|---|
| Vision/text LLM server | `llama-cpp` (llama-server, `--mmproj` supported; **Vulkan sees `Vulkan0: RX 6900 XT`**) |
| ONNX runtime (Python) | `onnxruntime:python` 1.22 |
| Phonemizer backend | `espeak-ng` |
| HTTP client | `python-httpx` |
| Images | `python-pillow`, `libheif` (`heif-dec`) |
| Audio | `python-numpy`, `python-soundfile`, `ffmpeg`, `python-mutagen` |

**Missing:** `kokoro-onnx` and `phonemizer-fork`, both small pure-Python
packages. `kokoro-onnx` normally pulls in `espeakng-loader`, which bundles a
prebuilt libespeak-ng; we want Guix's `espeak-ng` instead. kokoro-onnx accepts
an explicit espeak library/data path config; Phase 0 confirms this, and the
packages then either drop or stub `espeakng-loader`.

**Plan A (preferred):** package both in `~/.guix-channel` and run the worker
natively from a `manifest.scm` (dev) and later a channel package (deployed).
Timebox: one session.

**Plan B (accepted fallback, if A turns into a fight):** a `uv` venv with
`uv.lock` (hash-pinned) inside `guix shell --container --emulate-fhs --network
--expose=/dev/dri …`, following the same pattern as the workerd notes in
`~/.claude/CLAUDE.md`. That's less pure, but `uv.lock` plus a pinned Guix
manifest rebuilds it exactly, and it is written up in `worker/RUNBOOK.md`.

### 6.2 Process structure

```
worker/notecast_worker/
  main.py        loop: claim → dispatch → heartbeat thread → complete/fail
  client.py      worker API client (httpx, timeouts, jittered reconnect)
  images.py      normalize (§3.1)
  llm.py         ChatProvider protocol + OpenAIChat (+ AnthropicChat later)
  llama.py       managed llama-server lifecycle + GPU guard (§6.3)
  tts.py         SpeechEngine protocol + KokoroEngine
  audio.py       silence, concat, ffmpeg encode, loudnorm, ID3 chapters
  prompts/       ocr.v1.md, narrate.v1.md (versioned; version recorded in provenance)
  config.py      worker.toml loader
```

### 6.3 llama-server lifecycle and the GPU guard

- The worker **spawns llama-server itself** when a GPU job arrives, binding
  `127.0.0.1:8092`, with `--device Vulkan0` (otherwise llama.cpp may also use
  the Intel iGPU it detects as `Vulkan1`), `-ngl 99`, `--mmproj`, and a context
  size sized for one page plus its output. It waits for `/health` before
  dispatching.
- It **stops llama-server after 10 minutes idle** (configurable), which frees
  roughly 8–10 GB of VRAM for games. The cost is a model load of a few
  seconds on the next job.
- **GPU guard:** before advertising `transcribe`/`narrate` in `kinds`, the
  worker reads amdgpu's `mem_info_vram_total` and `mem_info_vram_used` from
  `/sys/class/drm/card*/device/`. If free VRAM is below
  `min_free_vram_mib` (default: model size + 2 GB), it advertises only `speak`
  and reports `state=gpu_unavailable`, which the UI shows as "waiting for the
  GPU". No manual pause is needed while gaming.
- On SIGTERM the worker stops the child, fails the current job as retryable,
  and exits. Shepherd's kill destructor signals the process group, so
  llama-server can't be orphaned either way.

### 6.4 Model files

Weights don't go in the store; they're too big for a 200 GB `/gnu/store` to
churn through. They live at `~/.local/share/notecast/models/`, described by a
committed `worker/models.lock`: name, URL, Hugging Face revision, sha256, size.
`just fetch-models` downloads them and verifies the hashes. At startup the
worker only checks file sizes against the lock, since hashing ~10 GB would
cost tens of seconds every time; `just verify-models` does the full check. That makes it
reproducible from the repo even though it isn't in Guix.

### 6.5 Deployed form

A Shepherd service in `~/.dots/home-config.scm`:

```scheme
(simple-service 'notecast-worker home-shepherd-service-type
  (list (shepherd-service
          (provision '(notecast-worker))
          (documentation "notecast OCR/TTS worker")
          (start #~(make-forkexec-constructor
                     (list #$(file-append notecast-worker "/bin/notecast-worker")
                           "--config"
                           (string-append (getenv "HOME") "/.config/notecast/worker.toml"))
                     #:log-file (string-append (getenv "HOME")
                                               "/.local/state/log/notecast-worker.log")))
          (stop #~(make-kill-destructor))
          (respawn? #t))))
```

- `worker.toml` holds no secrets and is placed declaratively through
  `home-files-service-type` from `~/.dots`.
- The token (`~/.config/notecast/token`, 0600) is **not** in any repo. Its
  canonical copy lives in Vaultwarden, and the runbook says how to put it back
  after a rebuild.
- `herd stop notecast-worker` works as a manual pause.

---

## 7. Ingress and auth

### 7.1 Cloudflare (dashboard; the tunnel is remotely managed)

1. Tunnel public hostname **`notecast.yager.me` (TBD)** → `http://caddy:80`
   (container name over `home.network`, never `localhost`; see
   `homelab-docs/services/cloudflared.md`).
2. Access application **A**: self-hosted, `notecast.yager.me`. Policy
   *Allow*: emails ∈ {Eric, her}. Login method: One-time PIN (email). Session
   duration: 1 month (re-login is an emailed code, about once a month).
3. Access application **B**: self-hosted, `notecast.yager.me/feed`. Policy
   *Bypass*: Everyone. The more specific path wins, so B overrides A for
   `/feed/*` only.
4. Copy A's **AUD tag** into `CF_ACCESS_AUD`.

All of this is dashboard state today, the same as the existing tunnel. Record
it in `deploy/cloudflare.md` with screenshots or exact values, so it can be
rebuilt by hand. Managing it with Terraform is a possible follow-up for the
whole `yager.me` zone and is out of scope here.

This follows the box's stated default ("Access remains the right default for
anything else that goes public, especially anything without its own
authentication", `homelab-docs/services/cloudflared.md`). workout-pwa-rs and
Forgejo skip Access because a PWA or API client can't complete the Access
login. The only client like that here is the podcast app, and the `/feed`
bypass handles it.

### 7.2 Caddy

A new **plain-HTTP site block** next to the existing `http://git.yager.me`
block. That's the established pattern for tunnel-routed hostnames, and it needs
no certificate, so the wildcard-cert rule isn't touched:

```caddy
http://notecast.yager.me {
	reverse_proxy 100.97.104.119:8790 {
		header_up X-Real-IP {header.CF-Connecting-IP}
	}
}
```

There's no `*.ericbits.win` staging hostname. Staging is the real hostname
with Access allowing Eric only (Phase 5), so the real auth path is tested from
the first deploy. Local development uses `DEV_AUTH_EMAIL` on loopback.

### 7.3 Access JWT verification in the app

`go-oidc` with a remote key set at
`https://<team>.cloudflareaccess.com/cdn-cgi/access/certs` (keys cached and
refreshed on an unknown `kid`), checking audience `CF_ACCESS_AUD`, issuer
`https://<team>.cloudflareaccess.com`, and expiry, and checking the `email`
claim against `ALLOWED_EMAILS`. It applies to every public route except
`/feed/` and `/healthz`. A failure returns 401 with no detail.

### 7.4 iPhone usage, and why not a PWA

Use it in Safari, from a bookmark or a home-screen shortcut **without**
`display: standalone` in the manifest. Standalone iOS web apps keep a separate
cookie store and handle the Access redirect badly, which is the same failure
workout-pwa-rs documented. Phase 6 tests a home-screen shortcut on her phone.
If it opens in Safari with the session intact, keep it.

The upload input is `<input type="file" accept="image/*" multiple>` with **no
`capture` attribute**, so iOS offers Take Photo, Photo Library and Files.

---

## 8. Deployment on asahihome

The same model as workout-pwa-rs (`homelab-docs/services/workout.md`): a bare
binary under a systemd **user** unit, not a container.

- Releases: `~/opt/notecast/releases/notecast-<gitsha>`, with a `current`
  symlink. Deploy means scp, repoint, `systemctl --user restart notecast`.
  Rollback means repointing and restarting. Before a deploy that carries a
  migration: `dump-services.sh upgrade-from-<sha>`.
- Unit: `~/.config/containers/systemd/notecast.service`, symlinked into
  `~/.config/systemd/user/` (that's the backup convention). It sets
  `Environment=` lines (§4.2), `OnFailure=notify-failure@%n.service`,
  `NoNewPrivileges=yes`, `PrivateTmp=yes`, `ProtectSystem=strict`,
  `ReadWritePaths=/mnt/data/volumes/notecast` and `MemoryMax=256M`.
- Data: `/mnt/data/volumes/notecast/`. Check for SELinux AVCs after the first
  write (`ausearch -m avc -ts recent`), as the workout deploy did. Fix any
  denial with a label, never `setenforce`.
- Expected footprint: about 20–40 MB RSS idle. Uploads stream to disk and never
  sit in memory.

### 8.1 Backups

- Already covered: `/mnt/data/volumes` is in restic's path list.
- Add a "notecast" block to `dump-services.sh` (online backup API plus
  `pragma quick_check`, pruned to 3, like the workout block).
- Add `notecast/app.db-wal` and `app.db-shm` to the restic excludes. That's the
  root-owned unit, **so it needs Eric's confirmation** (per the homelab docs).
- Photos and audio go along as normal files. Estimated growth: photos about
  3 MB/page, audio about 0.5 MB/min, so a heavy semester is a few GB, which is
  negligible against R2 and `/mnt/data`. Audio could be regenerated, but it's
  backed up anyway: regenerating costs GPU time and could come out in a
  different voice.
- Restore drill: restore `app.db` (from the dump), `pages/` and `audio/` into a
  scratch dir. Run the binary against it on loopback with `DEV_AUTH_EMAIL`.
  The note count and a spot-checked note must match production.

### 8.2 Box documentation

The `add-a-service` runbook applies, in particular:
`homelab-docs/docs/services/notecast.md`, `INDEX.md`, `_sidebar.md`, a
changelog entry, and `landing/services.json` with a probe at `/healthz` over
the tailnet address.

---

## 9. Repository

Forgejo `git.ericbits.win/eric/notecast`, checked out at
`~/development/homelab/notecast`. CI runs on the existing Forgejo runner.

```
notecast/
  CLAUDE.md                 footguns (written as they are found)
  justfile                  check, dev, build-release, deploy, fetch-models, eval
  manifest.scm              dev environment (go, just, sqlite, python + worker deps)
  docs/ARCHITECTURE.md  docs/IMPLEMENTATION-PLAN.md
  server/                   Go module
  worker/                   Python package, prompts/, models.lock, RUNBOOK.md
  eval/                     scripts only; data lives outside the repo
  deploy/                   notecast.service, Caddyfile.snippet, cloudflare.md, RUNBOOK.md
```

**Her notes are never committed.** That includes eval pages, ground-truth
transcripts and test fixtures. Eval data lives in `~/notecast-eval/` on
roundabits. Test fixtures are synthetic: Eric's own handwriting, or generated
images.

`just check` runs `gofmt -l` (empty), `go vet`, `staticcheck`,
`go test -race ./...`, `ruff check`, `ruff format --check`, and `pytest` (unit
tests only, with TTS and LLM faked). It's the single entry point, as in
workout-pwa-rs.

---

## 10. Security checklist

- [ ] JWT signature, `aud`, `iss` and `exp` verified, and email allowlisted, on
      every public non-feed route.
- [ ] Origin check on every non-GET request.
- [ ] Feed token: 256-bit, constant-time compare, rotatable (stored as-is; see §4.7).
- [ ] Worker token: hashed at rest on the server; the worker listener is never
      proxied.
- [ ] `DEV_AUTH_EMAIL` refuses non-loopback binds.
- [ ] Upload: size cap, magic-byte allowlist, no decoding server-side, random
      file names, served with `Content-Type` from the allowlist plus
      `X-Content-Type-Options: nosniff`.
- [ ] `html/template` everywhere. Transcript highlighting is done by escaping
      first, then wrapping matched markers, never by `template.HTML` on user
      text.
- [ ] Cloud provider keys only on roundabits, in 0600 files outside any repo.

---

## 11. Declarativeness ledger

| Piece | Where it's defined | Rebuild from scratch |
|---|---|---|
| Server binary | repo + Guix Go | `just build-release` |
| Server unit, Caddy block, dump block | Quadlet repo on asahihome (git, restic) | already backed up |
| Cloudflare tunnel hostname + Access apps | **dashboard** | `deploy/cloudflare.md` |
| Worker code + Guix deps | repo + `~/.guix-channel` (Plan A) or `uv.lock` (Plan B) | `guix home reconfigure` (A) / runbook (B) |
| Worker service + `worker.toml` | `~/.dots/home-config.scm` | `guix home reconfigure` |
| Model weights | `worker/models.lock` (hashes) | `just fetch-models` |
| Worker token, cloud keys | Vaultwarden | runbook step |
| Data | `/mnt/data/volumes/notecast` | restic restore (§8.1 drill) |

---

## 11a. Phase 0 findings that change the design

Recorded as they come in; the details and her page images live in
`~/notecast-eval/RESULTS.md`, outside the repo.

- **Her notes are clinical lab science fact lists** (hematology,
  microbiology, parasitology), almost every line a `cue -> answer` pair dense
  with Latin species names and abbreviations. Wrong readings are
  plausible-sounding medical terms (e.g. a fluke species copied from the next
  line), which is worse for exam study than an obvious garble.
- **Models don't flag their own uncertainty.** Qwen3-VL-8B never emitted
  `[?word]` even on lines it misread, so the review UI can't rely on the
  model's markers. Planned instead: **disagreement highlighting**. Run a
  second model on each page and highlight the words where the two readings
  differ. VRAM allows running them one after the other, and it catches the
  confident misreads. To be designed into `transcribe` once Phase 0 picks the
  two models.
- **A subject hint and a glossary** go into the OCR prompt: a per-collection
  subject line plus her own abbreviations (`plts = platelets`), editable in
  Settings.
- **Quiz-mode narration** is a candidate second style for `cue -> answer`
  notes: read the cue, pause, read the answer. Her call after hearing both.
- **Pronunciation of Latin names** with Kokoro needs a listening test. If it's
  bad, add a pronunciation lexicon to the speak stage.
- **Photo resolution matters.** The first test set was 640x480 (downscaled in
  transfer), about 15 px per handwritten line, which is too small to judge
  models fairly. The upload path keeps originals, so production uploads are
  full resolution. The eval needs originals too.
- **Never test the GPU next to a running game.** The first GPU load hit an
  amdgpu reset while Minecraft was running. The worker's GPU guard (§6.3) is
  the protection; the eval scripts must use it too.

## 12. Open decisions (TBD)

1. ~~Hostname~~: **decided 2026-10-14**: `notecast.yager.me`.
2. ~~Her email~~: **provided 2026-10-14**. Kept out of this repo on purpose; it
   goes into the unit's `ALLOWED_EMAILS` and the Access policy at deploy
   time.
3. **Ports** 8790/8791 on the tailnet address: confirm they're free at deploy.
4. **Voice, speed and narration style**: chosen by her in Phase 0.
5. **Illegible policy**: skip it or say "unreadable". Her preference, set in
   Settings.
6. **Model choice and image size**: decided by the Phase 0 eval, not up front.
7. **`feed_include_transcript`**: default off (§4.7).

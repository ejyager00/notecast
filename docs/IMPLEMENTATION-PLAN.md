# notecast — Implementation Plan

**Read `ARCHITECTURE.md` first.** That document covers *what* and *why*; this
one covers *in what order* and *how you know it worked*.

## How to use this document

- Phases are ordered. Don't start a phase until the previous phase's
  **Done when** holds on a clean tree (from Phase 1 onward, that includes
  `just check` being green).
- Each phase is sized for roughly one session. If one runs long, stop at a
  green `just check` and write handoff notes in the Status section at the
  bottom.
- Every phase ends with a commit whose message references the phase number.
- Never edit an applied migration. Add a new one.
- Anything touching `asahihome` follows `~/homelab-docs/docs/conventions.md` on
  that box: hard rules, tmux for long operations, SELinux enforcing, and Eric's
  confirmation before any root-owned change.
- **No `guix home reconfigure` or `guix system reconfigure` without Eric's
  explicit go-ahead.** Build first (`guix home build`), then stop and ask.
- If the plan turns out to be wrong, say so in the handoff notes. Don't
  silently substitute a different design.

---

## Phase 0: Eval spike (roundabits only, no app)

**Goal:** answer the questions the rest of the plan depends on, using her real
notes, before writing any app code. This is the phase most likely to change
the plan.

Build:

1. `~/notecast-eval/` (outside the repo): 15–20 photos of real pages from her
   iPhone, taken the way she would actually take them. Cover different pens,
   paper, lighting, density, and at least one page with a diagram and one dense
   page. For about 6 pages, make **ground-truth transcripts** by correcting the
   best model's output by hand (faster than typing from scratch). Her help here
   is worth a lot: she's the only one who knows what her abbreviations mean.
2. `eval/ocr_bench.py`: for each (model, quantization, long-edge size) run the
   OCR prompt through llama-server and record CER, WER (a ~20-line Levenshtein
   is enough, so no `jiwer` dependency), seconds per page, and a manual count of
   meaning-changing errors.
3. Candidate models: current open vision-language models that llama.cpp
   supports and that fit 16 GB with room for context. As of writing that means
   Qwen2.5-VL-7B and Qwen3-VL-8B at Q8_0, and Gemma 3 12B at Q4/Q6. **Check for
   newer ones first**, since this list will be stale. GGUF plus the matching
   `mmproj`. Always `--device Vulkan0`.
4. Image sizes: 1536, 2048, and native. Optionally grayscale + autocontrast.
5. Narration: run `narrate.v1.md` over the ground-truth transcripts. She reads
   two or three outputs and says what's wrong with the style.
6. TTS: get Kokoro running with Guix packages (Plan A, ARCHITECTURE §6.1),
   timeboxed to one session. That includes confirming kokoro-onnx accepts
   Guix's `espeak-ng` library and data paths. Otherwise fall back to Plan B
   and say so. Render one narration in 3–4 voices at 1.0×/1.1×.
7. Confirm Guix's `ffmpeg` has `libmp3lame` (`ffmpeg -encoders | grep mp3`).
8. **Optional, only with her agreement:** run 3 pages through one cloud vision
   model as a quality ceiling.

**Done when:**

- `~/notecast-eval/RESULTS.md` has the results table, the chosen model, quant
  and image size, and the reasoning.
- Kokoro runs, and the Plan A vs Plan B decision is recorded with its reason.
- She has listened to a full-length MP3 of one of her own notes and picked a
  voice, a speed and a narration style.
- **Go/no-go gate:** with the review step, is the best local model good
  enough? A reasonable bar: under ~5% CER and no meaning-changing errors on most
  pages. If not, stop and decide on cloud OCR with her before Phase 1. The
  provider seam means the rest of the plan doesn't change.

---

## Phase 1: Scaffold and gates

Build:

- Repo at `~/development/homelab/notecast` and Forgejo remote
  `git.ericbits.win/eric/notecast`.
- `manifest.scm` (go, just, sqlite, python, ruff, python-pytest, plus the
  worker deps from ARCHITECTURE §6.1). `justfile` with `check`, `dev` and
  `build-release`.
- `server/`: a Go module, a `cmd/notecast` that opens the DB, runs embedded
  migrations (`0001_init.sql` from ARCHITECTURE §4.4), serves `/healthz` on
  both listeners, and refuses `DEV_AUTH_EMAIL` with non-loopback binds.
- `worker/`: package skeleton, `config.py` loading `worker.toml`, and ruff
  config.
- `.forgejo/workflows/ci.yml` running `just check` on the existing runner. The
  image needs Go, Python, ruff and pytest. Check how workout-pwa-rs's
  `Containerfile.workout-ci` is built and do the same.

**Done when:** a push turns CI green. `just dev` starts the server on loopback.
`curl localhost:8790/healthz` returns `ok`. A test asserts that the
non-loopback plus `DEV_AUTH_EMAIL` combination exits non-zero.

---

## Phase 2: Server core (no UI yet)

Build:

- `internal/notes`: the status machine as pure functions over (status, event).
  It gets a **table-driven test covering every (status, event) pair**, with
  illegal transitions returning errors.
- `internal/files`: layout and atomic writes; sweeping `tmp/` at startup.
- `internal/jobs`: enqueue (and notify), long-poll claim, heartbeat, complete,
  fail with backoff, cancel, and the reaper. The partial unique index enforces
  one active job per note.
- `internal/workerapi`: the endpoints from ARCHITECTURE §4.6, checking the
  bearer token against `WORKER_TOKEN_SHA256`.
- Minimal JSON-free form endpoints to create a note, upload pages and submit,
  so tests can drive the whole lifecycle.
- `internal/store`: the FTS upsert inside every note-writing transaction.
- `cmd/fakeworker` (test-only): claims jobs and returns canned results and
  files.

**Done when:** a Go integration test drives the whole lifecycle: create → 2
pages → submit → fake transcribe → review → edit transcript (sets
`narration_stale`) → narrate → approve → speak → ready. It also asserts:

- a stale `lease_token` completion is rejected and changes nothing;
- a reaped lease is retried with backoff and fails after `max_attempts`;
- cancel → the next heartbeat gets 409;
- a second active job for the same note is impossible;
- FTS finds the note by a word that appears only in the transcript.

---

## Phase 3: Real worker, local end to end

Build:

- `client.py` (claim loop with jittered reconnect and a heartbeat thread),
  `images.py`, `llm.py` (`OpenAIChat`), `llama.py` (spawn, health-wait, idle
  stop, GPU guard), `tts.py` (`KokoroEngine`), `audio.py` (silence, concat,
  loudnorm, MP3, ID3 chapters, chapters JSON).
- Prompts `ocr.v1.md` and `narrate.v1.md`, taken from Phase 0 results.
  Narration uses the JSON-schema `response_format`.
- `models.lock` plus `just fetch-models` with sha256 verification.
- Unit tests with fake `ChatProvider` and `SpeechEngine`: chapter timing math,
  sentence chunking, the GPU guard's kinds selection with mocked sysfs, and
  JSON-schema validation of narration output, including retrying once on
  invalid JSON.

**Done when:** with the server running locally (`just dev`) and the real
worker on roundabits, a real 3-page note goes from photos to a playable MP3
with correct chapters. Also:

- Killing the worker mid-transcribe → retried after the lease expires →
  completes.
- Allocating VRAM (launching a game is enough) → the worker reports
  `gpu_unavailable` and still takes `speak` jobs.
- llama-server is gone 10 minutes after the last GPU job (`nvtop`/`radeontop`,
  or the sysfs VRAM numbers drop).

---

## Phase 4: UI

Build (html/template plus vendored htmx, no JS build):

- Library with live search (`hx-trigger="input changed delay:300ms"`),
  collection filter, status badges, and an inline ▶.
- New note: collection picker, per-page upload with progress, reorder and
  delete, Submit.
- Note page, review mode: images stacked above editable transcripts, `[?…]`
  and `[illegible]` highlighted, the narration textarea, a stale-narration
  banner, Save / Rewrite narration / Approve.
- Note page, ready mode: `<audio>` with speed buttons and Media Session
  metadata, Download, Edit.
- Status partial polling only while active. Worker banner: "Processing machine
  offline since …" / "waiting for the GPU" / "working on page 2 of 3".
- Settings: worker status, auto-approve, voice and speed defaults, illegible
  policy, collections CRUD, and a feed-token placeholder (feeds come in
  Phase 6).
- Origin check middleware. Mobile-first CSS, dark mode via
  `prefers-color-scheme`, tap targets at least 44 px.

**Done when:** Eric runs the full flow against `just dev` on loopback in a
desktop browser at phone width. An HEIC upload works, and a search finds a
note by a transcript word and shows a highlighted snippet. Real-iPhone checks
wait for Phase 5, the first time the app is reachable from a phone without
weakening the `DEV_AUTH_EMAIL` loopback guard.

---

## Phase 5: Deploy to asahihome behind Access (Eric only)

The app never runs on the box without JWT verification. Auth is built first,
in this phase, and deploy and ingress follow in the same phase.

Build:

- `internal/auth`: Access JWT middleware (go-oidc remote key set, aud, iss,
  exp, email allowlist), with tests using a locally generated key set
  (valid, expired, wrong aud, wrong key, email not allowed).
- `just build-release` (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, with the git
  sha embedded) and `just deploy` (scp, repoint `current`, restart).
- `deploy/notecast.service` (ARCHITECTURE §8) placed in the Quadlet repo with
  a symlink. `/mnt/data/volumes/notecast/`.
- Generate the worker token on roundabits. Put its sha256 in the unit and the
  token itself in Vaultwarden and `~/.config/notecast/token`. Check that
  roundabits reaches `100.97.104.119:8791` over the tailnet.
- Cloudflare: tunnel hostname, Access app A (**Eric's email only** for now),
  app B (`/feed` bypass). Write down every value in `deploy/cloudflare.md`.
- Caddy: the `http://notecast.yager.me` block next to `git.yager.me`, then
  `systemctl --user restart caddy`.
- `dump-services.sh` "notecast" block. **Ask Eric before** adding the restic
  `-wal`/`-shm` excludes (root unit).
- Box docs: `services/notecast.md`, `INDEX.md`, `_sidebar.md`, a changelog
  entry, and a `landing/services.json` entry probing `/healthz`.

**Done when:**

- `readelf -l` shows alignment compatible with 16K pages, and the binary runs.
  `systemctl --user status notecast` is active.
- From a non-tailnet network (phone on cellular): the Access OTP works, and a
  full note goes upload → review → ready with the real worker on roundabits.
  There are no AVCs in `ausearch -m avc -ts recent` afterwards.
- `curl https://notecast.yager.me/` with no session → Access login, not the app.
- `curl http://100.97.104.119:8790/` from a tailnet device without a JWT →
  401, and with a forged or unsigned JWT → 401.
- `curl https://notecast.yager.me/feed/anything` → reaches the app (404 for a
  bad token), confirming the bypass is scoped to `/feed`.
- `dump-services.sh` produces a `quick_check`-clean notecast snapshot.
- **Restore drill** (ARCHITECTURE §8.1) passes against a scratch directory.

---

## Phase 6: Feeds, and onboarding her

Build:

- `internal/feed`: `all.xml`, per-collection feeds, cover art, chapters JSON,
  GUID versioning, `Cache-Control: private`. Use golden-file tests for the XML,
  and validate one feed with an online validator using a throwaway note (not
  her content).
- Settings: generate or rotate the feed token, show the feed URLs, and step
  by step instructions for "Follow a Show by URL" in Apple Podcasts (or
  whichever app she prefers).
- Log feed fetches (User-Agent, `CF-Connecting-IP`) to find out who actually
  polls the feed.
- Worker-offline ntfy alert (ARCHITECTURE §4.8).
- Add her email to Access app A and `ALLOWED_EMAILS`.

**Done when, on her phone:**

- She logs in with the emailed code and uploads a real note. Review, approve,
  play, download.
- The feed is subscribed in her podcast app, new episodes appear on their own,
  and playback speed works. Note which chapter format her app shows, if any.
- Background and lock-screen playback works from the Safari player.
- Check whether a home-screen shortcut keeps the Access session, and record the
  result (ARCHITECTURE §7.4).
- Rotating the feed token breaks the old feed URL and the new one works.

---

## Phase 7: Worker as a Guix home service

Build:

- Plan A: `notecast-worker` package in `~/.guix-channel` (plus
  `python-kokoro-onnx` and `python-phonemizer-fork` if they weren't already
  packaged in Phase 0), wrapping llama-server and ffmpeg by store path.
  Plan B: the FHS-container command line, with `worker/RUNBOOK.md`.
- The Shepherd service and `worker.toml` in `~/.dots/home-config.scm`
  (ARCHITECTURE §6.5).
- `guix home build`, **then stop and ask Eric before `guix home reconfigure`.**

**Done when:** after a reboot of roundabits the worker comes up on its own and
picks up a queued note. `herd stop notecast-worker` pauses it cleanly with no
orphaned llama-server. `worker/RUNBOOK.md` covers rebuilding the worker from a
blank machine: channel, home config, token from Vaultwarden, `just
fetch-models`.

---

## Later / optional

- **Wake-on-LAN** from asahihome when a note is queued and the worker is
  offline. This only works if roundabits is on **wired** Ethernet with WoL
  enabled in firmware and on the NIC. Check that before designing anything.
- **Jump to the matching section**: a search hit inside a section links to
  `audio#t=<chapter start>`.
- **Anthropic adapter** for `llm.py` if cloud OCR is ever chosen.
- **GPU TTS / voice cloning** as a second `SpeechEngine`.
- **Terraform** for the `yager.me` tunnel hostnames and Access apps.

---

## Status

_Update at the end of every session: current phase, what's done, what's next,
and anything the next session needs to know._

- 2026-10-14: plan written.
- 2026-10-14 (later): Phases 1, 2 and 4 done; Phase 5 and 6 *code* done
  (Access JWT verification, podcast feeds, worker-offline alert). Deploy steps
  of Phases 5–6 not started (need Eric: Cloudflare dashboard, asahihome).
  Phase 0: Kokoro packaged in Guix (channel d4305d1, not pushed), models
  downloaded, 9 full-res HEIC pages in ~/notecast-eval/pages, draft truth for
  3 pages. **GPU abandoned** (ARCHITECTURE §11a): CPU-only from here.
  Next: CPU OCR eval on full-res pages; Python worker main loop (client.py,
  main.py) against the real worker API; deploy files.
- 2026-10-15: Deployed at notecast.yager.me; worker running by hand on
  roundabits (Shepherd service still to do, Phase 7). First real note (8 pages)
  went through: ~6.5 min/page OCR, 17 min narration on the CPU. Worker fixes:
  1 h timeout for the managed llama-server, per-page OCR cache. Then
  **narration per page** (ARCHITECTURE §3.3, migration 0002): rewrites redo only
  changed pages, her edits are kept, per-page narration cache. She agreed to
  OpenRouter for narration (docs/changelog.md); waiting on an API key, then
  compare GLM 5.3 Flash against the local model on her note.

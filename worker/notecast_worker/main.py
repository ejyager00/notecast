"""The worker loop: claim a job, run it with a heartbeat, report the result.

notecast-worker --config ~/.config/notecast/worker.toml [--once]
"""

from __future__ import annotations

import argparse
import hashlib
import logging
import os
import random
import signal
import subprocess
import tempfile
import threading
import time
from pathlib import Path
from typing import Any

import httpx
from PIL import UnidentifiedImageError

from . import __version__, audio, images, pipeline, tts
from .client import Client, LeaseLost
from .config import Config, load
from .llama import CPU, LlamaModel, LlamaServer, amd_vram, gpu_available
from .llm import OpenAIChat
from .models import model_path

log = logging.getLogger("notecast.worker")

HEARTBEAT_EVERY = 30.0  # server lease is 120 s

# A whole note's narration on the CPU can run past 10 minutes; the managed
# llama-server is on loopback, so a long wait means work, not a dead link.
MANAGED_TIMEOUT = 3600.0

# Page transcripts, kept so a retry after a failed narration doesn't spend
# another hour re-reading the same photos. Keyed by image, model and prompt.
CACHE = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache") / "notecast"
OCR_CACHE = CACHE / "ocr"

# Page narrations, for the same reason: a retry picks up at the page that
# failed. Keyed by everything the narration prompt sees.
NARRATION_CACHE = CACHE / "narration"


class Aborted(Exception):
    """The server took the job back (cancelled or lease lost); stop work on it."""


class Permanent(Exception):
    """A failure that retrying won't fix (e.g. an image that can't be decoded)."""


class Stop(BaseException):
    """SIGTERM/SIGINT: fail the current job as retryable and exit."""


class Heartbeat:
    """Keeps the lease alive from a background thread and carries progress."""

    def __init__(self, client: Client, job: dict[str, Any]) -> None:
        self.client, self.job = client, job
        self.progress = "starting"
        self.lost = threading.Event()
        self._stop = threading.Event()
        self._t = threading.Thread(target=self._run, daemon=True)

    def __enter__(self) -> Heartbeat:
        self._t.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._stop.set()
        self._t.join(timeout=5)

    def _run(self) -> None:
        while not self._stop.wait(HEARTBEAT_EVERY):
            try:
                self.client.heartbeat(self.job, self.progress)
            except LeaseLost:
                self.lost.set()
                return
            except httpx.HTTPError as e:  # transient; the lease survives 120 s
                log.warning("heartbeat failed: %s", e)

    def step(self, progress: str) -> None:
        """Record progress, and abort if the server has taken the job back."""
        if self.lost.is_set():
            raise Aborted("lease lost")
        self.progress = progress
        log.info("job %s: %s", self.job["job_id"], progress)


class Worker:
    def __init__(self, cfg: Config, client: Client | None = None) -> None:
        self.cfg = cfg
        self.client = client or Client(cfg.server_url, cfg.token, cfg.worker_id, __version__)
        self.server = self._llama_server()
        self.providers = {name: self._provider(name, p) for name, p in cfg.providers.items()}
        self._engine: tts.SpeechEngine | None = None
        self.ocr_cache: Path | None = OCR_CACHE
        self.narration_cache: Path | None = NARRATION_CACHE

    # --- setup -------------------------------------------------------------

    def _llama_server(self) -> LlamaServer | None:
        lc = self.cfg.llama
        if not lc:
            return None
        m = LlamaModel(
            name=lc.get("name", lc["model"]),
            model=model_path(lc["model"]),
            mmproj=model_path(lc["mmproj"]) if lc.get("mmproj") else None,
            ctx_size=int(lc.get("ctx", 16384)),
            extra_args=list(lc.get("args", [])),
            env=dict(lc.get("env", {})),
        )
        return LlamaServer(m, port=int(lc.get("port", 8092)), device=lc.get("device", CPU))

    def _provider(self, name: str, p: dict[str, Any]) -> OpenAIChat:
        if p.get("managed"):
            if self.server is None:
                raise ValueError(f"provider {name!r} is managed but [llama] is not configured")
            p = {"timeout": MANAGED_TIMEOUT, **p, "base_url": self.server.base_url}
        return OpenAIChat.from_config(name, p)

    def engine(self) -> tts.SpeechEngine:
        if self._engine is None:
            self._engine = tts.KokoroEngine(
                model_path(self.cfg.kokoro_model), model_path(self.cfg.kokoro_voices)
            )
        return self._engine

    def chat(self, stage: str) -> OpenAIChat:
        name = self.cfg.stages[stage]
        if self.cfg.providers[name].get("managed"):
            self.server.start()  # no-op if already up
            self.server.touch()
        return self.providers[name]

    # --- loop ----------------------------------------------------------------

    def kinds(self) -> list[str]:
        """What we can take right now. On the CPU that's everything; with a GPU,
        OCR and narration wait while something else holds the VRAM."""
        s = self.server
        if s is None or s.device == CPU:
            self.client.status.state = "idle"
            return ["transcribe", "narrate", "speak"]
        ok = gpu_available(s.model.needed_vram_mib(), s.running(), amd_vram())
        self.client.status.state = "idle" if ok else "gpu_unavailable"
        return ["transcribe", "narrate", "speak"] if ok else ["speak"]

    def run_forever(self, once: bool = False) -> None:
        backoff = 1.0
        while True:
            if self.server:
                self.server.stop_if_idle(self.cfg.idle_unload)
            try:
                job = self.client.claim(self.kinds())
                backoff = 1.0
            except httpx.HTTPError as e:
                log.warning("server unreachable (%s); retrying in %.0fs", e, backoff)
                time.sleep(backoff * random.uniform(0.8, 1.2))
                backoff = min(backoff * 2, 60)
                continue
            if job is None:
                continue
            self.run(job)
            if once:
                return

    def run(self, job: dict[str, Any]) -> None:
        log.info(
            "job %s: %s for note %s (attempt %s)",
            job["job_id"], job["kind"], job["note_pid"], job["attempt"],
        )  # fmt: skip
        self.client.status.state = "busy"
        try:
            with Heartbeat(self.client, job) as hb:
                handlers = {
                    "transcribe": self.transcribe,
                    "narrate": self.narrate,
                    "speak": self.speak,
                }
                result, files = handlers[job["kind"]](job, hb)
                hb.step("uploading results")
            self.client.complete(job, result, files)
            log.info("job %s: done", job["job_id"])
        except (Aborted, LeaseLost) as e:
            log.info("job %s: abandoned (%s)", job["job_id"], e)
        except Stop:
            self._report(job, "worker was stopped", retryable=True)
            raise
        except (Permanent, UnidentifiedImageError, subprocess.CalledProcessError) as e:
            self._report(job, f"{type(e).__name__}: {e}", retryable=False)
        except Exception as e:  # LLM hiccups, timeouts, bad JSON: let the server retry
            log.exception("job %s failed", job["job_id"])
            self._report(job, f"{type(e).__name__}: {e}", retryable=True)
        finally:
            self.client.status.state = "idle"

    def _report(self, job: dict[str, Any], msg: str, retryable: bool) -> None:
        try:
            self.client.fail(job, msg, retryable)
        except (LeaseLost, httpx.HTTPError) as e:
            log.warning("job %s: couldn't report failure (%s)", job["job_id"], e)

    # --- jobs ------------------------------------------------------------------

    def transcribe(self, job: dict[str, Any], hb: Heartbeat):
        pages = sorted(job["pages"], key=lambda p: p["position"])
        todo = [p for p in pages if p["page_id"] in set(job["transcribe"])]
        ocr = self.chat("ocr")
        # Some OCR models are trained on one fixed instruction (GLM-OCR wants
        # just "Text Recognition:"); a provider can override the prompts.
        pcfg = self.cfg.providers[self.cfg.stages["ocr"]]
        overrides = {}
        if "ocr_system" in pcfg:
            overrides["system"] = pcfg["ocr_system"]
        if "ocr_user" in pcfg:
            overrides["user"] = pcfg["ocr_user"]
        # Convert every page first and send the JPEG copies right away: OCR
        # takes minutes a page on the CPU, and HEIC originals don't display
        # outside Safari, so otherwise the note shows broken images until the end.
        files: dict[str, bytes] = {}
        ocr_in: dict[int, bytes] = {}
        for i, p in enumerate(todo, 1):
            hb.step(f"preparing page {i} of {len(todo)}")
            raw = self.client.input(job, p["input"])
            try:
                n = images.normalize(raw, ocr_long_edge=self.cfg.ocr_long_edge)
            except (UnidentifiedImageError, subprocess.CalledProcessError, OSError) as e:
                raise Permanent(f"page {i} isn't a readable image: {e}") from e
            files[f"display-{p['page_id']}.jpg"] = n.display
            files[f"thumb-{p['page_id']}.jpg"] = n.thumb
            ocr_in[p["page_id"]] = n.ocr
        try:
            self.client.previews(job, files)
        except httpx.HTTPError as e:  # cosmetic; complete() sends them again
            log.warning("job %s: previews not sent: %s", job["job_id"], e)
        texts: dict[int, str] = {}
        prov: dict[str, Any] = {}
        for i, p in enumerate(todo, 1):
            hb.step(f"reading page {i} of {len(todo)}")
            key = hashlib.sha256(
                repr((ocr.model, pipeline.OCR_PROMPT, sorted(overrides.items()))).encode()
                + ocr_in[p["page_id"]]
            ).hexdigest()
            cached = self.ocr_cache / f"{key}.md" if self.ocr_cache else None
            if cached and cached.exists():
                log.info("job %s: page %d of %d from cache", job["job_id"], i, len(todo))
                texts[p["page_id"]] = cached.read_text()
            else:
                r = pipeline.ocr_page(ocr, ocr_in[p["page_id"]], **overrides)
                texts[p["page_id"]] = r.markdown
                if cached:
                    cached.parent.mkdir(parents=True, exist_ok=True)
                    cached.write_text(r.markdown)
            prov = {**pipeline._prov(ocr, pipeline.OCR_PROMPT), "long_edge": self.cfg.ocr_long_edge}
        for p in pages:
            if p["page_id"] in texts:
                p["transcript"] = texts[p["page_id"]]
        segments, meta, nprov = self._narrate(pages, job, hb)
        result = {
            "pages": [{"page_id": pid, "transcript": t} for pid, t in texts.items()],
            "segments": segments,
            "provenance": {"ocr": prov, "narrate": nprov},
        }
        if meta:
            result["meta"] = meta
        return result, files

    def narrate(self, job: dict[str, Any], hb: Heartbeat):
        pages = sorted(job["pages"], key=lambda p: p["position"])
        segments, meta, prov = self._narrate(pages, job, hb)
        result: dict[str, Any] = {"segments": segments, "provenance": prov}
        if meta:
            result["meta"] = meta
        return result, {}

    def _narrate(self, pages: list[dict[str, Any]], job: dict[str, Any], hb: Heartbeat):
        """Narrate the pages the server marked, in order, each carrying on from
        the narration before it (new or kept); then title, summary and tags if
        asked. Returns (segments, meta or None, provenance)."""
        chat = self.chat("narrate")
        policy = job.get("illegible_policy") or "skip"
        style = self.cfg.narration_style
        name = pipeline.NARRATE_PROMPTS[style]
        todo = [p for p in pages if p.get("narrate")]
        segments: list[dict[str, Any]] = []
        so_far: list[str] = []
        done = 0
        for p in pages:
            if p.get("transcript") is None:
                continue  # not read yet; not part of the script
            if not p.get("narrate"):
                so_far.append(p.get("narration") or "")
                continue
            done += 1
            hb.step(f"narrating page {done} of {len(todo)}")
            before = pipeline.tail(pipeline.join_narrations(so_far))
            page_no = len(so_far) + 1
            key = hashlib.sha256(
                repr((chat.model, name, policy, page_no, before, p["transcript"])).encode()
            ).hexdigest()
            cached = self.narration_cache / f"{key}.txt" if self.narration_cache else None
            if cached and cached.exists():
                log.info("job %s: narration %d of %d from cache", job["job_id"], done, len(todo))
                text = cached.read_text()
            else:
                text = pipeline.narrate_page(
                    chat, p["transcript"], before, page_no, policy, style
                ).narration
                if cached:
                    cached.parent.mkdir(parents=True, exist_ok=True)
                    cached.write_text(text)
            segments.append({"page_id": p["page_id"], "narration": text})
            so_far.append(text)
        prov: dict[str, Any] = {**pipeline._prov(chat, name), "style": style}
        meta = None
        if job.get("meta"):
            hb.step("writing title and summary")
            m = pipeline.meta(
                chat,
                pipeline.join_pages(
                    [p["transcript"] for p in pages if p.get("transcript") is not None]
                ),
            )
            meta = {"title": m.title, "summary": m.summary, "tags": m.tags}
        return segments, meta, prov

    def speak(self, job: dict[str, Any], hb: Heartbeat):
        hb.step("making audio")
        voice, speed = job.get("voice") or "af_heart", float(job.get("speed") or 1.0)
        title = job.get("title") or "notecast"
        r = tts.render(job["narration"], self.engine(), voice, speed, title=title)
        hb.step("encoding")
        with tempfile.TemporaryDirectory(prefix="notecast-speak-") as d:
            mp3 = Path(d, "audio.mp3")
            audio.encode_mp3(r, mp3, title, album=job.get("collection") or None)
            data = mp3.read_bytes()
        result = {
            "audio": {
                "seconds": round(r.seconds, 2),
                "chapters": [{"start": round(c.start, 2), "title": c.title} for c in r.chapters],
                "voice": voice,
                "speed": speed,
            },
            "provenance": {"engine": "kokoro-82m", "voice": voice, "speed": speed},
        }
        return result, {"audio.mp3": data, "chapters.json": audio.chapters_json(r).encode()}

    def close(self) -> None:
        if self.server:
            self.server.stop()


def main() -> None:
    ap = argparse.ArgumentParser(prog="notecast-worker")
    ap.add_argument(
        "--config", type=Path, default=Path("~/.config/notecast/worker.toml").expanduser()
    )
    ap.add_argument("--once", action="store_true", help="process one job, then exit")
    args = ap.parse_args()
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s"
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)

    def stop(signum: int, _frame: object) -> None:
        raise Stop(signal.Signals(signum).name)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    w = Worker(load(args.config))
    log.info("notecast-worker %s as %s -> %s", __version__, w.cfg.worker_id, w.cfg.server_url)
    try:
        w.run_forever(once=args.once)
    except Stop as e:
        log.info("stopping (%s)", e)
    finally:
        w.close()


if __name__ == "__main__":
    main()

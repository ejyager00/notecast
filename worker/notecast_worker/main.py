"""The worker loop: claim a job, run it with a heartbeat, report the result.

notecast-worker --config ~/.config/notecast/worker.toml [--once]
"""

from __future__ import annotations

import argparse
import logging
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
            p = {**p, "base_url": self.server.base_url}
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
        files: dict[str, bytes] = {}
        texts: dict[int, str] = {}
        prov: dict[str, Any] = {}
        for i, p in enumerate(todo, 1):
            hb.step(f"reading page {i} of {len(todo)}")
            raw = self.client.input(job, p["input"])
            try:
                n = images.normalize(raw, ocr_long_edge=self.cfg.ocr_long_edge)
            except (UnidentifiedImageError, subprocess.CalledProcessError, OSError) as e:
                raise Permanent(f"page {i} isn't a readable image: {e}") from e
            files[f"display-{p['page_id']}.jpg"] = n.display
            files[f"thumb-{p['page_id']}.jpg"] = n.thumb
            r = pipeline.ocr_page(ocr, n.ocr, **overrides)
            texts[p["page_id"]] = r.markdown
            prov = {**r.provenance, "long_edge": self.cfg.ocr_long_edge}
        full = pipeline.join_pages(
            [texts.get(p["page_id"]) or p.get("transcript") or "" for p in pages]
        )
        hb.step("writing narration")
        nar = self._narrate(full, job)
        result = {
            "pages": [{"page_id": pid, "transcript": t} for pid, t in texts.items()],
            "narration": nar[0],
            "provenance": {"ocr": prov, "narrate": nar[1]},
        }
        return result, files

    def narrate(self, job: dict[str, Any], hb: Heartbeat):
        hb.step("writing narration")
        nar, prov = self._narrate(job["transcript"], job)
        return {"narration": nar, "provenance": prov}, {}

    def _narrate(self, transcript: str, job: dict[str, Any]):
        policy = job.get("illegible_policy") or "skip"
        n = pipeline.narrate(self.chat("narrate"), transcript, policy, self.cfg.narration_style)
        return (
            {"title": n.title, "summary": n.summary, "tags": n.tags, "narration": n.narration},
            {**n.provenance, "style": self.cfg.narration_style},
        )

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

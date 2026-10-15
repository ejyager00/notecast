import io
from typing import Any

import numpy as np
import pytest
from PIL import Image

from notecast_worker import main as M
from notecast_worker.client import LeaseLost, Status
from notecast_worker.config import Config
from notecast_worker.llm import ChatResult


def jpeg() -> bytes:
    b = io.BytesIO()
    Image.new("RGB", (300, 400), "white").save(b, "JPEG")
    return b.getvalue()


class FakeClient:
    def __init__(self, inputs: dict[str, bytes] | None = None, lose_lease: bool = False) -> None:
        self.inputs = inputs or {}
        self.lose_lease = lose_lease
        self.completed: list[tuple[dict, dict]] = []
        self.previewed: list[dict] = []
        self.events: list[str] = []
        self.failed: list[tuple[str, bool]] = []
        self.status = Status()

    def input(self, job, name):
        return self.inputs[name]

    def heartbeat(self, job, progress):
        if self.lose_lease:
            raise LeaseLost("cancelled")

    def previews(self, job, files):
        self.events.append("previews")
        self.previewed.append(files)

    def complete(self, job, result, files):
        self.completed.append((result, files))

    def fail(self, job, error, retryable):
        self.failed.append((error, retryable))


class FakeChat:
    name, model = "fake", "fake-model"
    spent = 0.0

    def __init__(self, fail: Exception | None = None) -> None:
        self.calls: list[dict[str, Any]] = []
        self.events: list[str] = []
        self.fail = fail

    def complete(self, system, user_text, images=None, json_schema=None, max_tokens=4096):
        self.events.append("chat")
        self.calls.append(
            {"user": user_text, "images": len(images or []), "json": json_schema is not None}
        )
        if self.fail:
            raise self.fail
        if json_schema:
            text = '{"title":"T","summary":"S","tags":["Hematology"],"narration":"## A\\nSaid."}'
        else:
            text = f"page text {len(self.calls)}"
        return ChatResult(text, 0.1, None, None)


class FakeEngine:
    sample_rate = 24000

    def synthesize(self, text, voice, speed):
        return np.zeros(2400, dtype=np.float32) + 0.01


def worker(client, chat=None) -> M.Worker:
    cfg = Config(
        server_url="http://x", token="t", worker_id="w",
        providers={"p": {"kind": "openai", "base_url": "http://unused/v1"}},
        stages={"ocr": "p", "narrate": "p"},
    )  # fmt: skip
    w = M.Worker(cfg, client=client)
    w.providers["p"] = chat or FakeChat()
    w._engine = FakeEngine()
    w.ocr_cache = w.narration_cache = None
    return w


def job(kind: str, **kw) -> dict:
    return {"job_id": 1, "kind": kind, "note_pid": "abc", "attempt": 1, "lease_token": "L", **kw}


def test_transcribe_ocrs_only_new_pages_and_narrates_marked_ones():
    c, chat = FakeClient({"page-2": jpeg()}), FakeChat()
    c.events = chat.events = []  # one shared log: previews must precede any OCR
    w = worker(c, chat)
    w.run(job("transcribe", transcribe=[2], meta=True, pages=[
        {"page_id": 1, "position": 1, "transcript": "old page", "narration": "Old said."},
        {"page_id": 2, "position": 2, "input": "page-2", "narrate": True},
    ]))  # fmt: skip
    assert not c.failed
    result, files = c.completed[0]
    assert result["pages"] == [{"page_id": 2, "transcript": "page text 1"}]
    assert set(files) == {"display-2.jpg", "thumb-2.jpg"}
    assert c.previewed == [files], "previews go up early, and the same JPEGs again at the end"
    assert c.events == ["previews", "chat", "chat", "chat"], "OCR, page 2, then title/summary"
    assert files["display-2.jpg"][:3] == b"\xff\xd8\xff"
    page2 = chat.calls[1]["user"]
    assert "Old said." in page2 and "page text 1" in page2 and "old page" not in page2
    assert result["segments"] == [{"page_id": 2, "narration": "## A\nSaid."}]
    meta_input = chat.calls[2]["user"]
    assert "old page" in meta_input and "page text 1" in meta_input
    assert result["meta"]["tags"] == ["hematology"]
    assert result["provenance"]["ocr"]["model"] == "fake-model"


def test_retry_after_failed_narration_reuses_page_text(tmp_path, monkeypatch):
    pages = [{"page_id": 1, "position": 1, "input": "page-1", "narrate": True}]
    c, chat = FakeClient({"page-1": jpeg()}), FakeChat()
    w = worker(c, chat)
    w.ocr_cache = tmp_path

    def narrate_times_out(*a, **kw):
        raise TimeoutError("narration timed out")

    with monkeypatch.context() as m:
        m.setattr(M.pipeline, "narrate_page", narrate_times_out)
        w.run(job("transcribe", transcribe=[1], pages=pages))
    assert c.failed and c.failed[0][1], "retryable"
    assert len(chat.calls) == 1

    w.run(job("transcribe", transcribe=[1], pages=pages, attempt=2))
    result, _ = c.completed[0]
    assert result["pages"] == [{"page_id": 1, "transcript": "page text 1"}]
    assert [x["json"] for x in chat.calls] == [False, True], "OCR ran once; retry only narrated"


def test_narrate_redoes_marked_pages_and_resumes_after_failure(tmp_path):
    pages = [
        {
            "page_id": 1,
            "position": 1,
            "transcript": "kept",
            "narration": "First para.\n\nKept end.",
        },
        {
            "page_id": 2,
            "position": 2,
            "transcript": "changed",
            "narration": "Old.",
            "narrate": True,
        },
        {"page_id": 3, "position": 3, "transcript": None},  # uploaded, not read yet
        {"page_id": 4, "position": 4, "transcript": "also changed", "narrate": True},
    ]

    class FailsOnce(FakeChat):
        def complete(self, *a, **kw):
            if len(self.calls) == 1 and not getattr(self, "failed", False):
                self.failed = True
                raise TimeoutError("slow")
            return super().complete(*a, **kw)

    c, chat = FakeClient(), FailsOnce()
    w = worker(c, chat)
    w.narration_cache = tmp_path
    w.run(job("narrate", pages=pages))
    assert c.failed and len(chat.calls) == 1, "page 2 done, page 4 failed"
    assert "Kept end." in chat.calls[0]["user"] and "First para." not in chat.calls[0]["user"]

    w.run(job("narrate", pages=pages, attempt=2))
    result, _ = c.completed[0]
    assert [s["page_id"] for s in result["segments"]] == [2, 4]
    assert len(chat.calls) == 2, "page 2 came from the cache; only page 4 ran again"
    assert "Said." in chat.calls[1]["user"], "page 4 carries on from page 2's new narration"
    assert "meta" not in result


def test_managed_provider_waits_long_for_local_model():
    w = worker(FakeClient())
    w.server = type("S", (), {"base_url": "http://127.0.0.1:8092/v1"})()
    assert w._provider("local", {"kind": "openai", "managed": True}).timeout == M.MANAGED_TIMEOUT
    assert w._provider("x", {"kind": "openai", "base_url": "http://c/v1"}).timeout == 600


def test_speak_produces_tagged_mp3_with_chapters():
    c = FakeClient()
    w = worker(c)
    w.run(
        job(
            "speak",
            narration="## One\nHello.\n## Two\nBye.",
            title="T",
            voice="af_heart",
            speed=1.0,
        )
    )
    result, files = c.completed[0]
    assert files["audio.mp3"][:3] == b"ID3"
    assert [ch["title"] for ch in result["audio"]["chapters"]] == ["One", "Two"]
    assert result["audio"]["seconds"] > 0


def test_unreadable_image_is_permanent():
    c = FakeClient({"page-1": b"\xff\xd8\xff not really a jpeg"})
    worker(c).run(
        job("transcribe", transcribe=[1], pages=[{"page_id": 1, "position": 1, "input": "page-1"}])
    )
    assert len(c.failed) == 1 and c.failed[0][1] is False and not c.completed


def test_model_error_is_retryable():
    c = FakeClient()
    worker(c, FakeChat(fail=RuntimeError("llama-server fell over"))).run(
        job("narrate", pages=[{"page_id": 1, "position": 1, "transcript": "x", "narrate": True}])
    )
    assert c.failed == [("RuntimeError: llama-server fell over", True)]


def test_lost_lease_abandons_quietly(monkeypatch):
    monkeypatch.setattr(M, "HEARTBEAT_EVERY", 0.01)
    c = FakeClient({"page-1": jpeg(), "page-2": jpeg()}, lose_lease=True)

    class SlowChat(FakeChat):
        def complete(self, *a, **kw):
            import time

            time.sleep(0.05)  # let a heartbeat discover the lost lease
            return super().complete(*a, **kw)

    pages = [{"page_id": i, "position": i, "input": f"page-{i}"} for i in (1, 2)]
    worker(c, SlowChat()).run(job("transcribe", transcribe=[1, 2], pages=pages))
    assert not c.completed and not c.failed


def test_stop_reports_retryable_and_propagates():
    c = FakeClient()
    chat = FakeChat(fail=M.Stop("SIGTERM"))
    with pytest.raises(M.Stop):
        worker(c, chat).run(
            job(
                "narrate", pages=[{"page_id": 1, "position": 1, "transcript": "x", "narrate": True}]
            )
        )
    assert c.failed == [("worker was stopped", True)]


def test_cpu_offers_every_kind():
    assert worker(FakeClient()).kinds() == ["transcribe", "narrate", "speak"]

"""OCR and narration stages (ARCHITECTURE §3.2 and §3.3).

Prompt files are versioned; the version is recorded with every result so a
note can be re-run against a newer prompt or model and compared.
"""

from __future__ import annotations

from dataclasses import dataclass
from importlib import resources
from typing import Any

from .llm import ChatProvider, parse_json

OCR_PROMPT = "ocr.v1"
NARRATE_PROMPTS = {"prose": "narrate-page.v1", "quiz": "narrate-quiz-page.v1"}
META_PROMPT = "meta.v1"

ILLEGIBLE = {
    "skip": "leave it out and carry on smoothly.",
    "say": 'say "one word unreadable" (or "a few words unreadable").',
}

PAGE_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {"narration": {"type": "string"}},
    "required": ["narration"],
    "additionalProperties": False,
}

META_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "title": {"type": "string"},
        "summary": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["title", "summary", "tags"],
    "additionalProperties": False,
}

# How much of the narration so far a page sees, so it carries on smoothly
# without repeating it. Its last paragraph is enough, and keeps prompts short.
CONTEXT_CHARS = 800


def prompt(name: str) -> str:
    return resources.files("notecast_worker.prompts").joinpath(f"{name}.md").read_text()


@dataclass(frozen=True)
class PageText:
    markdown: str
    seconds: float
    provenance: dict[str, str]


def ocr_page(
    provider: ChatProvider, jpeg: bytes, system: str | None = None, user: str | None = None
) -> PageText:
    """Transcribe one page. `system`/`user` override the prompts for models
    trained on a fixed instruction (GLM-OCR wants just "Text Recognition:")."""
    r = provider.complete(
        system=prompt(OCR_PROMPT) if system is None else system,
        user_text="Transcribe this page." if user is None else user,
        images=[jpeg],
        max_tokens=4096,
    )
    text = r.text.strip()
    if text.startswith("```"):
        text = text.split("\n", 1)[1].rsplit("```", 1)[0].strip()
    return PageText(text, r.seconds, _prov(provider, OCR_PROMPT))


@dataclass(frozen=True)
class PageNarration:
    narration: str
    seconds: float
    provenance: dict[str, str]


@dataclass(frozen=True)
class Meta:
    title: str
    summary: str
    tags: list[str]
    seconds: float
    provenance: dict[str, str]


def join_pages(pages: list[str]) -> str:
    return "\n\n".join(f"<!-- page {i} -->\n{p.strip()}" for i, p in enumerate(pages, 1))


def join_narrations(parts: list[str]) -> str:
    """The note's script, as the server joins it (core.syncNarration)."""
    return "\n\n".join(p.strip() for p in parts if p.strip())


def tail(narration: str) -> str:
    """The end of the narration so far: its last paragraph, at most CONTEXT_CHARS."""
    paras = [p for p in narration.strip().split("\n\n") if p.strip()]
    return paras[-1][-CONTEXT_CHARS:] if paras else ""


def narrate_page(
    provider: ChatProvider,
    transcript: str,
    before: str = "",
    page: int = 1,
    illegible: str = "skip",
    style: str = "prose",
) -> PageNarration:
    """Narrate one page. `before` is the end of the narration so far (see tail)."""
    name = NARRATE_PROMPTS[style]
    system = (
        prompt(name)
        .replace("{illegible_instruction}", ILLEGIBLE[illegible])
        .replace("{page}", str(page))
    )
    lead = (
        f"The narration so far ended with:\n\n{before}"
        if before
        else "This is the first page; the narration starts here."
    )
    user = f"{lead}\n\n---\n\nPage {page}:\n\n{transcript}"
    d, r = _json(provider, system, user, PAGE_SCHEMA, 4096)
    return PageNarration(str(d["narration"]).strip(), r.seconds, _prov(provider, name))


def meta(provider: ChatProvider, transcript: str) -> Meta:
    """Title, summary and tags for the whole note (transcript as join_pages makes it)."""
    d, r = _json(provider, prompt(META_PROMPT), f"Transcript:\n\n{transcript}", META_SCHEMA, 1024)
    tags = [str(t).strip().lower() for t in d["tags"] if str(t).strip()][:6]
    return Meta(
        title=str(d["title"]).strip(),
        summary=str(d["summary"]).strip(),
        tags=tags,
        seconds=r.seconds,
        provenance=_prov(provider, META_PROMPT),
    )


def narrate_note(
    provider: ChatProvider, pages: list[str], illegible: str = "skip", style: str = "prose"
) -> tuple[str, Meta]:
    """Narrate every page in order, then the title, summary and tags (for eval)."""
    parts: list[str] = []
    for i, t in enumerate(pages, 1):
        before = tail(join_narrations(parts))
        parts.append(narrate_page(provider, t, before, i, illegible, style).narration)
    return join_narrations(parts), meta(provider, join_pages(pages))


def _json(provider: ChatProvider, system: str, user: str, schema: dict[str, Any], max_tokens: int):
    last_err: Exception | None = None
    for _ in range(2):  # one retry on malformed JSON
        r = provider.complete(
            system=system, user_text=user, json_schema=schema, max_tokens=max_tokens
        )
        try:
            d = parse_json(r.text)
            if not isinstance(d, dict) or not all(k in d for k in schema["required"]):
                raise ValueError(f"missing keys in {r.text[:200]!r}")
            return d, r
        except (ValueError, TypeError) as e:
            last_err = e
    raise RuntimeError(f"model output was not valid JSON: {last_err}")


def _prov(p: ChatProvider, prompt_version: str) -> dict[str, str]:
    return {"provider": p.name, "model": p.model, "prompt": prompt_version}

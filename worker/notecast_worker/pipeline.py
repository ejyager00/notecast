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
NARRATE_PROMPTS = {"prose": "narrate.v1", "quiz": "narrate-quiz.v1"}

ILLEGIBLE = {
    "skip": "leave it out and carry on smoothly.",
    "say": 'say "one word unreadable" (or "a few words unreadable").',
}

NARRATION_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "title": {"type": "string"},
        "summary": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}},
        "narration": {"type": "string"},
    },
    "required": ["title", "summary", "tags", "narration"],
    "additionalProperties": False,
}


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
class Narration:
    title: str
    summary: str
    tags: list[str]
    narration: str
    seconds: float
    provenance: dict[str, str]


def join_pages(pages: list[str]) -> str:
    return "\n\n".join(f"<!-- page {i} -->\n{p.strip()}" for i, p in enumerate(pages, 1))


def narrate(
    provider: ChatProvider, transcript: str, illegible: str = "skip", style: str = "prose"
) -> Narration:
    name = NARRATE_PROMPTS[style]
    system = prompt(name).replace("{illegible_instruction}", ILLEGIBLE[illegible])
    last_err: Exception | None = None
    for _ in range(2):  # one retry on malformed JSON
        r = provider.complete(
            system=system,
            user_text=f"Transcript:\n\n{transcript}",
            json_schema=NARRATION_SCHEMA,
            max_tokens=8192,
        )
        try:
            d = parse_json(r.text)
            tags = [str(t).strip().lower() for t in d["tags"] if str(t).strip()][:6]
            return Narration(
                title=str(d["title"]).strip(),
                summary=str(d["summary"]).strip(),
                tags=tags,
                narration=str(d["narration"]).strip(),
                seconds=r.seconds,
                provenance=_prov(provider, name),
            )
        except (ValueError, KeyError, TypeError) as e:
            last_err = e
    raise RuntimeError(f"narration output was not valid JSON: {last_err}")


def _prov(p: ChatProvider, prompt_version: str) -> dict[str, str]:
    return {"provider": p.name, "model": p.model, "prompt": prompt_version}

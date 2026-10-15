"""Speech synthesis (ARCHITECTURE §3.5).

Narration text is plain prose with `## ` lines splitting sections. Each
section becomes an audio chapter: its heading is read, then its paragraphs,
with pauses between. A line that is just `[pause]` (or `[pause 3.5]`) is a
deliberate silence: quiz-style narration reads a cue, pauses, then gives the
answer.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

import numpy as np

SAMPLE_RATE = 24000
PAUSE_AFTER_HEADING = 0.4
PAUSE_PARAGRAPH = 0.5
PAUSE_SECTION = 1.0
PAUSE_QUIZ = 3.0  # default for a bare [pause]
MAX_PAUSE = 15.0


class SpeechEngine(Protocol):
    sample_rate: int

    def synthesize(self, text: str, voice: str, speed: float) -> np.ndarray: ...


class KokoroEngine:
    sample_rate = SAMPLE_RATE

    def __init__(self, model: Path, voices: Path) -> None:
        from kokoro_onnx import Kokoro  # heavy import; keep tests fast

        self._k = Kokoro(str(model), str(voices))

    def voices(self) -> list[str]:
        return list(self._k.get_voices())

    def synthesize(self, text: str, voice: str, speed: float) -> np.ndarray:
        samples, sr = self._k.create(text, voice=voice, speed=speed, lang="en-us")
        assert sr == self.sample_rate, sr
        return np.asarray(samples, dtype=np.float32)


@dataclass(frozen=True)
class Pause:
    seconds: float


@dataclass(frozen=True)
class Section:
    heading: str | None
    paragraphs: list[str | Pause]


_PAUSE = re.compile(r"^\[pause(?:\s+(\d+(?:\.\d+)?))?\]$", re.I)


def parse_narration(text: str) -> list[Section]:
    """Split narration on `## ` lines; blank lines separate paragraphs."""
    sections: list[Section] = []
    heading: str | None = None
    paras: list[str | Pause] = []
    buf: list[str] = []

    def flush_para() -> None:
        if buf:
            paras.append(" ".join(buf))
            buf.clear()

    def flush_section() -> None:
        flush_para()
        if heading is not None or paras:
            sections.append(Section(heading, list(paras)))
        paras.clear()

    for raw in text.splitlines():
        line = raw.strip()
        if m := _PAUSE.match(line):
            flush_para()
            secs = float(m.group(1)) if m.group(1) else PAUSE_QUIZ
            paras.append(Pause(min(secs, MAX_PAUSE)))
        elif line == "##" or line.startswith("## "):
            flush_section()
            heading = line[2:].strip() or None
        elif line.startswith("#"):
            # Any other heading level is treated as prose, minus the hashes.
            if text_ := line.lstrip("#").strip():
                buf.append(text_)
        elif not line:
            flush_para()
        else:
            buf.append(line)
    flush_section()
    return [s for s in sections if s.heading or s.paragraphs]


_SENTENCE = re.compile(r"(?<=[.!?])\s+(?=[\"'(\[]?[A-Z0-9])")


def chunks(paragraph: str, max_chars: int = 350) -> list[str]:
    """Sentence-ish chunks short enough to stay under Kokoro's phoneme limit."""
    out: list[str] = []
    cur = ""
    for sent in _SENTENCE.split(paragraph.strip()):
        if cur and len(cur) + 1 + len(sent) > max_chars:
            out.append(cur)
            cur = sent
        else:
            cur = f"{cur} {sent}".strip()
    if cur:
        out.append(cur)
    return out


@dataclass(frozen=True)
class Chapter:
    start: float  # seconds
    title: str


@dataclass(frozen=True)
class Rendered:
    pcm: np.ndarray
    sample_rate: int
    chapters: list[Chapter]

    @property
    def seconds(self) -> float:
        return len(self.pcm) / self.sample_rate


def render(
    narration: str, engine: SpeechEngine, voice: str, speed: float, title: str | None = None
) -> Rendered:
    sr = engine.sample_rate
    parts: list[np.ndarray] = []
    chapters: list[Chapter] = []
    pos = 0

    def silence(seconds: float) -> None:
        nonlocal pos
        n = int(seconds * sr)
        parts.append(np.zeros(n, dtype=np.float32))
        pos += n

    def speak(text: str) -> None:
        nonlocal pos
        for c in chunks(text):
            pcm = engine.synthesize(c, voice, speed)
            parts.append(pcm)
            pos += len(pcm)

    sections = parse_narration(narration)
    if not sections:
        raise ValueError("narration is empty")
    for i, sec in enumerate(sections):
        if i:
            silence(PAUSE_SECTION)
        chapters.append(
            Chapter(pos / sr, sec.heading or (title if i == 0 else None) or f"Part {i + 1}")
        )
        if sec.heading:
            speak(sec.heading)
            silence(PAUSE_AFTER_HEADING)
        prev: str | Pause | None = None
        for para in sec.paragraphs:
            if isinstance(para, Pause):
                silence(para.seconds)
            else:
                if isinstance(prev, str):
                    silence(PAUSE_PARAGRAPH)
                speak(para)
            prev = para
    silence(0.3)
    return Rendered(np.concatenate(parts), sr, chapters)

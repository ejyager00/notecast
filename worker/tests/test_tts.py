import numpy as np
import pytest

from notecast_worker import tts


class FakeEngine:
    """One second of audio per 10 characters, so timing math is checkable."""

    sample_rate = 1000

    def __init__(self) -> None:
        self.calls: list[str] = []

    def synthesize(self, text: str, voice: str, speed: float) -> np.ndarray:
        self.calls.append(text)
        return np.ones(len(text) * 100, dtype=np.float32)


def test_parse_sections_and_paragraphs():
    s = tts.parse_narration(
        "Intro line.\n\n## Glycolysis\nFirst para\ncontinues.\n\nSecond.\n## Krebs\nOnly."
    )
    assert [x.heading for x in s] == [None, "Glycolysis", "Krebs"]
    assert s[1].paragraphs == ["First para continues.", "Second."]
    assert s[0].paragraphs == ["Intro line."]


def test_parse_ignores_empty_and_strips_other_heading_levels():
    s = tts.parse_narration("\n\n# Big title\n\n## \nText.\n")
    assert s[0].paragraphs == ["Big title"]
    assert s[1].heading is None and s[1].paragraphs == ["Text."]


def test_chunks_split_on_sentences_under_limit():
    para = " ".join(f"Sentence number {i} is here." for i in range(40))
    out = tts.chunks(para, max_chars=120)
    assert all(len(c) <= 120 for c in out)
    assert " ".join(out) == para


def test_render_chapter_times():
    eng = FakeEngine()
    r = tts.render("## Aaaaa\nbbbbbbbbbb\n## Cc\ndd", eng, "v", 1.0, title="T")
    assert [c.title for c in r.chapters] == ["Aaaaa", "Cc"]
    # chapter 2 starts after: "Aaaaa" (0.5s) + heading pause + "bbbbbbbbbb" (1s) + section pause
    expected = 0.5 + tts.PAUSE_AFTER_HEADING + 1.0 + tts.PAUSE_SECTION
    assert r.chapters[1].start == pytest.approx(expected, abs=1e-3)
    assert eng.calls == ["Aaaaa", "bbbbbbbbbb", "Cc", "dd"]


def test_render_untitled_first_section_uses_note_title():
    r = tts.render("Just prose.", FakeEngine(), "v", 1.0, title="My note")
    assert r.chapters[0].title == "My note"


def test_render_empty_raises():
    with pytest.raises(ValueError):
        tts.render("  \n\n", FakeEngine(), "v", 1.0)


def test_pause_markers_parse_and_render():
    s = tts.parse_narration(
        "## Q\nWhat causes it?\n[pause]\nThe answer.\n[PAUSE 1.5]\nNext.\n[pause 99]"
    )
    assert s[0].paragraphs == [
        "What causes it?",
        tts.Pause(tts.PAUSE_QUIZ),
        "The answer.",
        tts.Pause(1.5),
        "Next.",
        tts.Pause(tts.MAX_PAUSE),
    ]
    eng = FakeEngine()
    r = tts.render("Cue.\n[pause 2]\nAnswer!!!", eng, "v", 1.0)
    # 0.4s + 2s pause (no extra paragraph gap) + 0.9s + 0.3s tail
    assert r.seconds == pytest.approx(0.4 + 2 + 0.9 + 0.3, abs=1e-3)
    assert eng.calls == ["Cue.", "Answer!!!"]

import io
import sys
from pathlib import Path

from PIL import Image

from notecast_worker import images
from notecast_worker.llm import parse_json
from notecast_worker.pipeline import META_PROMPT, NARRATE_PROMPTS, join_pages, prompt, tail

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "eval"))
from metrics import cer, normalize, wer


def test_parse_json_tolerates_fence():
    assert parse_json('```json\n{"a": 1}\n```') == {"a": 1}
    assert parse_json(' {"a": 2} ') == {"a": 2}


def test_prompts_load():
    assert "verbatim" in prompt("ocr.v1")
    for name in NARRATE_PROMPTS.values():
        assert "{illegible_instruction}" in prompt(name)
    assert "[pause]" in prompt(NARRATE_PROMPTS["quiz"])
    assert "{page}" in prompt(NARRATE_PROMPTS["quiz"])
    assert "summary" in prompt(META_PROMPT)


def test_tail_is_the_last_paragraph():
    assert tail("") == ""
    assert tail("## A\nOne.\n\nTwo.\n\n") == "Two."
    assert len(tail("x" * 5000)) == 800


def test_join_pages_marks_page_numbers():
    assert join_pages(["a", "b"]) == "<!-- page 1 -->\na\n\n<!-- page 2 -->\nb"


def test_normalize_exif_rotation_and_sizes():
    img = Image.new("RGB", (400, 100), "white")
    exif = img.getexif()
    exif[0x0112] = 6  # rotate 90 CW on display
    buf = io.BytesIO()
    img.save(buf, "JPEG", exif=exif)
    n = images.normalize(buf.getvalue(), ocr_long_edge=200)
    assert Image.open(io.BytesIO(n.display)).size == (100, 400)
    assert max(Image.open(io.BytesIO(n.ocr)).size) == 200


def test_heif_sniff():
    assert images.is_heif(b"\0\0\0\x18ftypheic\0\0\0\0")
    assert not images.is_heif(b"\xff\xd8\xff\xe0" + b"\0" * 12)


def test_metrics():
    ref = "# Title\n- one **two** three"
    assert normalize(ref) == "Title one two three"
    assert cer(ref, ref) == 0
    assert wer("Title one too three", ref) == 0.25
    assert normalize("[?word] here") == "word here"
    assert normalize("a→b [diagram: egg] c") == "a -> b c"


def test_disagreement_flags_and_highlight():
    from disagree import flagged_positions, highlight

    a = ["Giant", "fluke", "->", "Fasciola", "hepatica"]
    b = ["Giant", "fluke", "->", "Fasciolopsis", "buski"]
    assert flagged_positions(a, b) == {3, 4}
    assert highlight(a, b) == "Giant fluke -> {Fasciola hepatica|Fasciolopsis buski}"

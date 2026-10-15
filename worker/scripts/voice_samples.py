"""Render the short voice samples played by the "Sample" buttons in the app.

One MP3 per voice offered in the server's picker, all reading the same
sentence at 1.0x (the browser plays them at the chosen reading speed). The
voice list is read from server/internal/web/pages.go so there is one source
of truth; a Go test checks every listed voice has a sample. Re-run after
changing that list:

    just voice-samples [--only af_heart,bm_george]
"""

from __future__ import annotations

import argparse
import re
from pathlib import Path

from notecast_worker import audio, tts
from notecast_worker.models import model_path

ROOT = Path(__file__).resolve().parents[2]
PAGES_GO = ROOT / "server/internal/web/pages.go"
OUT = ROOT / "server/internal/web/static/voices"

# Generic and in the style of her notes; not taken from them.
TEXT = (
    "This is how your notes will sound. "
    "Neutrophils are the most common white blood cell in adult blood, "
    "and they are the first to arrive at a site of infection."
)


def listed_voices() -> list[str]:
    block = PAGES_GO.read_text().split("var voices = []voice{", 1)[1].split("\n}", 1)[0]
    return re.findall(r'\{"([a-z]{2}_[a-z]+)",', block)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", help="comma-separated voice ids (default: all listed)")
    args = ap.parse_args()
    ids = args.only.split(",") if args.only else listed_voices()
    if not ids:
        raise SystemExit(f"no voices found in {PAGES_GO}")
    OUT.mkdir(parents=True, exist_ok=True)
    engine = tts.KokoroEngine(model_path("kokoro-v1.0.onnx"), model_path("kokoro-voices-v1.0.bin"))
    for v in ids:
        r = tts.render(TEXT, engine, v, 1.0)
        dest = OUT / f"{v}.mp3"
        audio.encode_mp3(r, dest, f"notecast voice sample ({v})")
        print(f"{dest.relative_to(ROOT)}  {r.seconds:.1f}s  {dest.stat().st_size // 1024} KiB")


if __name__ == "__main__":
    main()

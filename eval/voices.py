"""Render one narration in several voices/speeds for her to compare.

python eval/voices.py NARRATION.json|TEXT_FILE --out DIR
    [--voices af_heart,af_bella,bf_emma,am_michael] [--speeds 1.0,1.1]
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import common  # noqa: F401  (sets sys.path)
from notecast_worker import audio, tts
from notecast_worker.models import model_path


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("source", type=Path)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--voices", default="af_heart,af_bella,af_nicole,bf_emma,am_michael")
    ap.add_argument("--speeds", default="1.0")
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)

    raw = args.source.read_text()
    if args.source.suffix == ".json":
        d = json.loads(raw)
        text, title = d["narration"], d.get("title", "Sample")
    else:
        text, title = raw, "Sample"
    engine = tts.KokoroEngine(model_path("kokoro-v1.0.onnx"), model_path("kokoro-voices-v1.0.bin"))
    for v in args.voices.split(","):
        for s in (float(x) for x in args.speeds.split(",")):
            r = tts.render(text, engine, v, s, title=title)
            dest = args.out / f"{v}-{s:g}.mp3"
            audio.encode_mp3(r, dest, f"{title} ({v}, {s:g}x)")
            print(f"{dest}  {r.seconds:.0f}s")


if __name__ == "__main__":
    main()

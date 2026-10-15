"""End-to-end without the server: page photos -> transcript -> narration -> MP3.

    python eval/render_note.py PAGE... --out DIR [--model qwen3-vl-8b]
        [--voice af_heart] [--speed 1.0] [--size 2048] [--illegible skip]

Writes transcript.md, narration.json, <voice>.mp3 and chapters.json to DIR.
This is the same code path the worker's transcribe/speak jobs will use.
"""

from __future__ import annotations

import argparse
import json
import logging
import time
from pathlib import Path

from common import guard_gpu, llama_model
from notecast_worker import audio, images, pipeline, tts
from notecast_worker.llama import LlamaServer
from notecast_worker.llm import OpenAIChat
from notecast_worker.models import model_path


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("pages", nargs="*", type=Path)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--model", default="qwen3-vl-8b")
    ap.add_argument("--voice", default="af_heart")
    ap.add_argument("--speed", type=float, default=1.0)
    ap.add_argument("--size", type=int, default=2048)
    ap.add_argument("--illegible", choices=sorted(pipeline.ILLEGIBLE), default="skip")
    ap.add_argument("--style", choices=sorted(pipeline.NARRATE_PROMPTS), default="prose")
    ap.add_argument("--transcript", type=Path, help="skip OCR; narrate this transcript")
    ap.add_argument("--env", action="append", default=[], help="KEY=VALUE for llama-server")
    ap.add_argument("--arg", action="append", default=[], help="extra llama-server argument")
    ap.add_argument(
        "--device", default="none", help="llama.cpp device (CPU unless NOTECAST_ALLOW_GPU=1)"
    )
    ap.add_argument("--force", action="store_true", help="skip the free-VRAM check")
    args = ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    args.out.mkdir(parents=True, exist_ok=True)

    if not args.pages and not args.transcript:
        ap.error("give page images or --transcript")
    model, body = llama_model(args.model, args.env, args.arg)
    guard_gpu(model, args.device, args.force)
    with LlamaServer(model, device=args.device, log_path=args.out / "llama-server.log") as srv:
        chat = OpenAIChat(args.model, srv.base_url, args.model, extra_body=body)
        if args.transcript:
            transcript = args.transcript.read_text()
        else:
            texts = []
            for p in args.pages:
                n = images.normalize(p.read_bytes(), ocr_long_edge=args.size)
                r = pipeline.ocr_page(chat, n.ocr)
                logging.info("ocr %s: %.1fs", p.name, r.seconds)
                texts.append(r.markdown)
            transcript = pipeline.join_pages(texts)
            (args.out / "transcript.md").write_text(transcript + "\n")
        nar = pipeline.narrate(chat, transcript, args.illegible, args.style)
        logging.info("narrate: %.1fs", nar.seconds)
    (args.out / "narration.json").write_text(json.dumps(nar.__dict__, indent=2))

    engine = tts.KokoroEngine(model_path("kokoro-v1.0.onnx"), model_path("kokoro-voices-v1.0.bin"))
    t0 = time.monotonic()
    rendered = tts.render(nar.narration, engine, args.voice, args.speed, title=nar.title)
    logging.info("tts: %.1fs audio in %.1fs", rendered.seconds, time.monotonic() - t0)
    mp3 = args.out / f"{args.style}-{args.voice}-{args.speed:g}.mp3"
    audio.encode_mp3(rendered, mp3, nar.title)
    (args.out / "chapters.json").write_text(audio.chapters_json(rendered))
    print(f"\n{nar.title}\n{nar.summary}\ntags: {', '.join(nar.tags)}\n-> {mp3}")


if __name__ == "__main__":
    main()

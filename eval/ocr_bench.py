"""Phase 0 OCR benchmark.

For each candidate model and image size, transcribe every page in
$NOTECAST_EVAL_DIR/pages and score it against <truth>/<stem>.md when present.
Outputs go to runs/<model>/[<run>-]<size>/<stem>.md; the table to
runs/summary[-<run>].md.

    python eval/ocr_bench.py [--models a,b] [--sizes 1536,2048,0] [--pages DIR]
        [--truth DIR] [--run NAME] [--env K=V ...] [--arg=-c --arg=8192 ...]
        [--repeat N] [--device none]
"""

from __future__ import annotations

import argparse
import json
import logging
import statistics
import time
from pathlib import Path

from common import EVAL_DIR, candidates, guard_gpu, llama_model, model_names, page_images
from metrics import cer, wer
from notecast_worker import images
from notecast_worker.llama import LlamaServer, amd_vram
from notecast_worker.llm import OpenAIChat
from notecast_worker.pipeline import ocr_page


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default=",".join(model_names()))
    ap.add_argument("--sizes", default="1536,2048,0", help="long edge; 0 = native")
    ap.add_argument("--pages", default=str(EVAL_DIR / "pages"))
    ap.add_argument(
        "--device", default="none", help="llama.cpp device (CPU unless NOTECAST_ALLOW_GPU=1)"
    )
    ap.add_argument("--force", action="store_true", help="skip the free-VRAM check")
    ap.add_argument("--truth", default=str(EVAL_DIR / "truth"))
    ap.add_argument("--run", default="", help="prefix for output dirs and summary")
    ap.add_argument("--env", action="append", default=[], help="KEY=VALUE for llama-server")
    ap.add_argument("--arg", action="append", default=[], help="extra llama-server argument")
    ap.add_argument("--repeat", type=int, default=1, help="passes over the pages")
    ap.add_argument("--stop-on-error", action="store_true", help="abort at the first failure")
    args = ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")

    pages_dir = Path(args.pages)
    pages = page_images(pages_dir)
    if not pages:
        raise SystemExit(f"no images in {pages_dir}")
    truth_dir = Path(args.truth)
    runs = EVAL_DIR / "runs"
    sizes = [int(s) for s in args.sizes.split(",")]
    decoded = {p: images.decode(p.read_bytes()) for p in pages}

    rows = []
    for name in args.models.split(","):
        model, body = llama_model(name, args.env, args.arg)
        guard_gpu(model, args.device, args.force)
        prefix = f"{args.run}-" if args.run else ""
        log_path = runs / name / f"{prefix}llama-server.log"
        log_path.parent.mkdir(parents=True, exist_ok=True)
        t0 = time.monotonic()
        with LlamaServer(model, device=args.device, log_path=log_path) as srv:
            load_s = time.monotonic() - t0
            vram = amd_vram()
            chat = OpenAIChat(name, srv.base_url, name, extra_body=body)
            cand = candidates()[name]
            prompts = {"system": cand.get("ocr_system"), "user": cand.get("ocr_user")}
            for size in sizes:
                out = runs / name / (prefix + (str(size) if size else "native"))
                out.mkdir(parents=True, exist_ok=True)
                scores = []
                errors = 0
                restarts = 0
                for rep in range(args.repeat):
                    for p in pages:
                        jpeg = images.to_jpeg(images.fit(decoded[p], size or None), quality=92)
                        if srv.has_died():  # GPU reset mid-run: reload (with retries)
                            logging.warning("llama-server died; restarting")
                            restarts += 1
                            srv.start()
                        try:
                            r = ocr_page(chat, jpeg, **prompts)
                        except Exception as e:  # record and continue
                            errors += 1
                            logging.error("%s %s %s: %s", name, size, p.name, e)
                            (out / f"{p.stem}.error").write_text(str(e))
                            if args.stop_on_error:
                                break
                            continue
                        (out / f"{p.stem}.md").write_text(r.markdown + "\n")
                        rec = {"page": p.stem, "pass": rep, "seconds": round(r.seconds, 2)}
                        t = truth_dir / f"{p.stem}.md"
                        if t.exists():
                            ref = t.read_text()
                            rec |= {"cer": cer(r.markdown, ref), "wer": wer(r.markdown, ref)}
                        scores.append(rec)
                        logging.info("%s %s %s %s", name, size or "native", p.name, rec)
                    if errors and args.stop_on_error:
                        break
                (out / "scores.json").write_text(json.dumps(scores, indent=2))
                scored = [s for s in scores if "cer" in s]
                rows.append(
                    {
                        "model": name,
                        "size": size or "native",
                        "pages": len(scores),
                        "errors": errors,
                        "restarts": restarts,
                        "loads": f"{srv.load_attempts - srv.load_failures}/{srv.load_attempts}",
                        "cer": statistics.mean(s["cer"] for s in scored) if scored else None,
                        "wer": statistics.mean(s["wer"] for s in scored) if scored else None,
                        "sec_per_page": statistics.mean(s["seconds"] for s in scores)
                        if scores
                        else None,
                        "load_s": round(load_s, 1),
                        "vram_used_mib": vram.used_mib if vram else None,
                    }
                )

    lines = [
        "| model | size | pages | errors | restarts | loads ok | CER | WER | s/page | load s "
        "| VRAM used MiB |",
        "|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for r in rows:
        f = lambda v, pct=False: "—" if v is None else (f"{v:.1%}" if pct else f"{v:.1f}")  # noqa: E731
        lines.append(
            f"| {r['model']} | {r['size']} | {r['pages']} | {r['errors']} | {r['restarts']} | "
            f"{r['loads']} | {f(r['cer'], True)} | "
            f"{f(r['wer'], True)} | {f(r['sec_per_page'])} | {r['load_s']} | "
            f"{r['vram_used_mib'] or '—'} |"
        )
    summary = runs / (f"summary-{args.run}.md" if args.run else "summary.md")
    summary.write_text("\n".join(lines) + "\n")
    print("\n".join(lines))


if __name__ == "__main__":
    main()

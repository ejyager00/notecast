"""Repeated cold loads of one model, to measure GPU load reliability.

Each round: wait --settle seconds after the previous unload, start
llama-server with retries disabled, send one short request, unload.

    python eval/load_test.py [--model qwen3-vl-8b] [--rounds 4] [--settle 30]
        [--env K=V ...] [--arg=--no-mmap ...] [--name LABEL]

Appends one line per run to $NOTECAST_EVAL_DIR/load-tests.log.
"""

from __future__ import annotations

import argparse
import time
from datetime import datetime

from common import EVAL_DIR, llama_model
from notecast_worker.llama import LlamaServer, LoadFailed, amd_vram
from notecast_worker.llm import OpenAIChat


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="qwen3-vl-8b")
    ap.add_argument("--rounds", type=int, default=4)
    ap.add_argument("--settle", type=float, default=30)
    ap.add_argument("--env", action="append", default=[])
    ap.add_argument("--arg", action="append", default=[])
    ap.add_argument("--name", default="")
    args = ap.parse_args()

    model, body = llama_model(args.model, args.env, args.arg)
    log_path = EVAL_DIR / "runs" / args.model / f"load-{args.name or 'test'}.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    results = []
    for i in range(args.rounds):
        srv = LlamaServer(model, log_path=log_path, retry_delays=(), settle_after_stop=args.settle)
        t0 = time.monotonic()
        try:
            srv.start()
            OpenAIChat("t", srv.base_url, "t", extra_body=body).complete(
                "You are terse.", "Say OK.", max_tokens=8
            )
            res = f"ok({time.monotonic() - t0:.1f}s)"
        except LoadFailed as e:
            res = "LOST" if e.device_lost else "EXIT"
        except Exception as e:  # request failure after load = lost mid-request
            res = f"REQFAIL({type(e).__name__})"
        finally:
            srv.stop()
        v = amd_vram()
        print(f"round {i + 1}: {res} vram_after={v.used_mib if v else '?'}MiB", flush=True)
        results.append(res)
    ok = sum(r.startswith("ok") for r in results)
    line = (
        f"{datetime.now():%F %T} {args.name or '-'} model={args.model} settle={args.settle:g}s "
        f"env={' '.join(args.env) or '-'} args={' '.join(args.arg) or '-'} "
        f"ok={ok}/{len(results)} [{' '.join(results)}]"
    )
    with (EVAL_DIR / "load-tests.log").open("a") as f:
        f.write(line + "\n")
    print(line)


if __name__ == "__main__":
    main()

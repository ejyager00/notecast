"""Download and verify the model files pinned in worker/models.lock.

Stdlib only, so it runs before any worker dependency exists. Resumable:
a partial download is kept as <name>.part and continued with a Range request.
A file that already exists is re-hashed and only trusted if it matches.

    python3 worker/scripts/fetch_models.py [--group NAME ...] [--verify-only]
"""

import argparse
import hashlib
import os
import sys
import tomllib
import urllib.request
from pathlib import Path

LOCK = Path(__file__).resolve().parent.parent / "models.lock"
CHUNK = 1 << 20


def models_dir() -> Path:
    default = Path.home() / ".local/share/notecast/models"
    return Path(os.environ.get("NOTECAST_MODELS_DIR", default))


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        while block := f.read(CHUNK):
            h.update(block)
    return h.hexdigest()


def download(url: str, part: Path, size: int) -> None:
    have = part.stat().st_size if part.exists() else 0
    if have > size:
        part.unlink()
        have = 0
    if have == size:
        return
    req = urllib.request.Request(url, headers={"Range": f"bytes={have}-"} if have else {})
    with urllib.request.urlopen(req, timeout=60) as resp:
        if have and resp.status != 206:
            have = 0  # server ignored Range; start over
        mode = "ab" if have else "wb"
        done = have
        last = -1
        with part.open(mode) as out:
            while block := resp.read(CHUNK):
                out.write(block)
                done += len(block)
                pct = done * 100 // size
                if pct != last and pct % 5 == 0:
                    print(
                        f"  {part.name}: {pct}% ({done / 1e9:.2f}/{size / 1e9:.2f} GB)", flush=True
                    )
                    last = pct


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--group", action="append", help="only these groups")
    ap.add_argument("--verify-only", action="store_true")
    args = ap.parse_args()

    entries = tomllib.loads(LOCK.read_text())["file"]
    if args.group:
        entries = [e for e in entries if e["group"] in args.group]
    dest = models_dir()
    dest.mkdir(parents=True, exist_ok=True)

    failed = []
    for e in entries:
        final = dest / e["name"]
        if final.exists():
            if final.stat().st_size == e["size"] and sha256_of(final) == e["sha256"]:
                print(f"ok      {e['name']}")
                continue
            print(f"BAD     {e['name']} (hash or size mismatch)")
            if args.verify_only:
                failed.append(e["name"])
                continue
            final.unlink()
        elif args.verify_only:
            print(f"missing {e['name']}")
            failed.append(e["name"])
            continue

        part = dest / (e["name"] + ".part")
        print(f"fetch   {e['name']}", flush=True)
        try:
            download(e["url"], part, e["size"])
        except Exception as exc:  # keep going; report at the end
            print(f"ERROR   {e['name']}: {exc}")
            failed.append(e["name"])
            continue
        got = sha256_of(part)
        if part.stat().st_size != e["size"] or got != e["sha256"]:
            print(f"BAD     {e['name']}: sha256 {got} != {e['sha256']}; deleted")
            part.unlink()
            failed.append(e["name"])
            continue
        part.rename(final)
        print(f"ok      {e['name']}")

    if failed:
        print(f"\n{len(failed)} file(s) failed: {', '.join(failed)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

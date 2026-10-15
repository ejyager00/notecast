"""Locating pinned model files (see worker/models.lock)."""

from __future__ import annotations

import os
import tomllib
from pathlib import Path

LOCK = Path(__file__).resolve().parent.parent / "models.lock"


def models_dir() -> Path:
    default = Path.home() / ".local/share/notecast/models"
    return Path(os.environ.get("NOTECAST_MODELS_DIR", default))


def model_path(name: str) -> Path:
    """Path of a locked model file, after a cheap size check.

    The full sha256 check is `just verify-models`; hashing ~10 GB on every
    worker start would cost tens of seconds for no new information.
    """
    entries = {e["name"]: e for e in tomllib.loads(LOCK.read_text())["file"]}
    if name not in entries:
        raise KeyError(f"{name} is not in models.lock")
    p = models_dir() / name
    if not p.exists():
        raise FileNotFoundError(f"{p} missing; run `just fetch-models`")
    if p.stat().st_size != entries[name]["size"]:
        raise ValueError(f"{p} has the wrong size; run `just fetch-models`")
    return p

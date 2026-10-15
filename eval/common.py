"""Shared helpers for the eval scripts (run via `just eval-*`)."""

from __future__ import annotations

import os
import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "worker"))

from notecast_worker.llama import LlamaModel  # noqa: E402
from notecast_worker.models import model_path  # noqa: E402

EVAL_DIR = Path(os.environ.get("NOTECAST_EVAL_DIR", Path.home() / "notecast-eval"))
IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif"}


def candidates() -> dict[str, dict]:
    return tomllib.loads((ROOT / "eval/candidates.toml").read_text())


def model_names() -> list[str]:
    return [k for k in candidates() if not k.startswith("_")]


def llama_model(
    name: str, extra_env: list[str] | None = None, extra_args: list[str] | None = None
) -> tuple[LlamaModel, dict]:
    """A candidate from candidates.toml, plus CLI overrides.

    `[_all]` in candidates.toml applies `env`/`args` to every candidate (GPU
    workarounds belong to the device, not the model). `extra_env` entries are
    KEY=VALUE strings from the command line.
    """
    cands = candidates()
    shared, c = cands.get("_all", {}), cands[name]
    env = {**shared.get("env", {}), **c.get("env", {})}
    for kv in extra_env or []:
        k, _, v = kv.partition("=")
        env[k] = v
    m = LlamaModel(
        name=name,
        model=model_path(c["model"]),
        mmproj=model_path(c["mmproj"]) if c.get("mmproj") else None,
        ctx_size=c.get("ctx", 16384),
        extra_args=[*shared.get("args", []), *c.get("args", []), *(extra_args or [])],
        env={k: str(v) for k, v in env.items()},
    )
    return m, dict(c.get("body", {}))


def page_images(d: Path) -> list[Path]:
    return sorted(p for p in d.iterdir() if p.suffix.lower() in IMAGE_EXTS)


def guard_gpu(model: LlamaModel, device: str, force: bool = False) -> None:
    """Refuse to load onto the GPU while something else (a game) holds it."""
    from notecast_worker.llama import amd_vram, gpu_available

    if device == "none" or force:
        return
    v = amd_vram()
    need = model.needed_vram_mib()
    if not gpu_available(need, server_running=False, vram=v):
        raise SystemExit(
            f"GPU busy: {v.free_mib} MiB free, {model.name} needs ~{need} MiB. "
            "Close games/GPU apps, use --device none, or --force."
        )

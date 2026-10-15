"""worker.toml (ARCHITECTURE §3.4, §6). No secrets live in this file: the
worker token and any cloud API keys are read from the files it names."""

from __future__ import annotations

import socket
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass
class Config:
    server_url: str
    token: str
    worker_id: str
    providers: dict[str, dict[str, Any]]
    stages: dict[str, str]  # ocr/narrate -> provider name
    llama: dict[str, Any] = field(default_factory=dict)  # the managed llama-server
    ocr_long_edge: int = 1536
    narration_style: str = "prose"
    kokoro_model: str = "kokoro-v1.0.onnx"
    kokoro_voices: str = "kokoro-voices-v1.0.bin"
    idle_unload: float = 1800.0


def load(path: Path) -> Config:
    d = tomllib.loads(path.read_text())
    srv = d["server"]
    token = Path(srv["token_file"]).expanduser().read_text().strip()
    if not token:
        raise ValueError(f"{srv['token_file']} is empty")
    stages = d.get("stages", {"ocr": "local", "narrate": "local"})
    providers = d.get("providers", {})
    for stage, name in stages.items():
        if name not in providers:
            raise ValueError(f"stage {stage!r} uses unknown provider {name!r}")
    return Config(
        server_url=srv["url"].rstrip("/"),
        token=token,
        worker_id=srv.get("worker_id") or socket.gethostname(),
        providers=providers,
        stages=stages,
        llama=d.get("llama", {}),
        ocr_long_edge=int(d.get("images", {}).get("ocr_long_edge", 1536)),
        narration_style=d.get("narration", {}).get("style", "prose"),
        kokoro_model=d.get("tts", {}).get("model", "kokoro-v1.0.onnx"),
        kokoro_voices=d.get("tts", {}).get("voices", "kokoro-voices-v1.0.bin"),
        idle_unload=float(d.get("llama", {}).get("idle_unload_seconds", 1800)),
    )

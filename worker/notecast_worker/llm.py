"""Chat providers (ARCHITECTURE §3.4).

One protocol, one implementation for now: OpenAI-compatible Chat
Completions, which covers llama-server, OpenAI, Gemini's compat endpoint,
OpenRouter and vLLM. A native Anthropic adapter would be a second class
implementing `ChatProvider`.
"""

from __future__ import annotations

import base64
import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

import httpx


@dataclass(frozen=True)
class ChatResult:
    text: str
    seconds: float
    prompt_tokens: int | None
    completion_tokens: int | None


class ChatProvider(Protocol):
    name: str
    model: str

    def complete(
        self,
        system: str,
        user_text: str,
        images: list[bytes] | None = None,
        json_schema: dict[str, Any] | None = None,
        max_tokens: int = 4096,
    ) -> ChatResult: ...


@dataclass
class OpenAIChat:
    name: str
    base_url: str
    model: str
    api_key: str | None = None
    timeout: float = 600.0
    # Merged into every request body, e.g. {"chat_template_kwargs": {...}}.
    extra_body: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_config(cls, name: str, cfg: dict[str, Any]) -> OpenAIChat:
        key = None
        if key_file := cfg.get("api_key_file"):
            key = Path(key_file).expanduser().read_text().strip()
        return cls(
            name=name,
            base_url=cfg["base_url"].rstrip("/"),
            model=cfg.get("model", "local"),
            api_key=key,
            timeout=float(cfg.get("timeout", 600)),
            extra_body=dict(cfg.get("extra_body", {})),
        )

    def complete(
        self,
        system: str,
        user_text: str,
        images: list[bytes] | None = None,
        json_schema: dict[str, Any] | None = None,
        max_tokens: int = 4096,
    ) -> ChatResult:
        content: list[dict[str, Any]] = []
        for img in images or []:
            uri = "data:image/jpeg;base64," + base64.b64encode(img).decode()
            content.append({"type": "image_url", "image_url": {"url": uri}})
        content.append({"type": "text", "text": user_text})

        body: dict[str, Any] = {
            "model": self.model,
            "messages": [
                *([{"role": "system", "content": system}] if system else []),
                {"role": "user", "content": content},
            ],
            "temperature": 0,
            "max_tokens": max_tokens,
        }
        if json_schema is not None:
            body["response_format"] = {
                "type": "json_schema",
                "json_schema": {"name": "result", "strict": True, "schema": json_schema},
            }
        body.update(self.extra_body)

        headers = {"Authorization": f"Bearer {self.api_key}"} if self.api_key else {}
        t0 = time.monotonic()
        resp = httpx.post(
            f"{self.base_url}/chat/completions", json=body, headers=headers, timeout=self.timeout
        )
        resp.raise_for_status()
        data = resp.json()
        dt = time.monotonic() - t0
        choice = data["choices"][0]
        if choice.get("finish_reason") == "length":
            raise RuntimeError(f"{self.name}: output truncated at max_tokens={max_tokens}")
        usage = data.get("usage") or {}
        return ChatResult(
            text=choice["message"]["content"] or "",
            seconds=dt,
            prompt_tokens=usage.get("prompt_tokens"),
            completion_tokens=usage.get("completion_tokens"),
        )


def parse_json(text: str) -> Any:
    """Parse model JSON, tolerating a ```json fence some providers add."""
    t = text.strip()
    if t.startswith("```"):
        t = t.split("\n", 1)[1] if "\n" in t else ""
        t = t.rsplit("```", 1)[0]
    return json.loads(t)

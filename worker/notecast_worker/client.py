"""Client for the server's /worker/v1 API (server/internal/workerapi).

The Go fake worker (server/internal/fakeworker) is the reference for this
protocol; keep the two in step.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

import httpx


class LeaseLost(Exception):
    """The server no longer considers this job ours (cancelled, expired, done)."""


@dataclass
class Status:
    state: str = "idle"  # idle | busy | gpu_unavailable
    detail: str = ""


@dataclass
class Client:
    base_url: str
    token: str
    worker_id: str
    version: str
    status: Status = field(default_factory=Status)
    http: httpx.Client | None = None

    def __post_init__(self) -> None:
        if self.http is None:
            # Claims long-poll for up to 25 s server-side.
            self.http = httpx.Client(timeout=httpx.Timeout(60.0, connect=10.0))

    def _headers(self, lease: str | None = None) -> dict[str, str]:
        h = {
            "Authorization": f"Bearer {self.token}",
            "X-Worker-Id": self.worker_id,
            "X-Worker-Version": self.version,
            "X-Worker-State": self.status.state,
            "X-Worker-Detail": self.status.detail[:200],
        }
        if lease:
            h["X-Lease-Token"] = lease
        return h

    def _check(self, r: httpx.Response) -> httpx.Response:
        if r.status_code == 409:
            raise LeaseLost(r.text.strip())
        r.raise_for_status()
        return r

    def claim(self, kinds: list[str]) -> dict[str, Any] | None:
        r = self._check(
            self.http.post(
                f"{self.base_url}/worker/v1/claim", json={"kinds": kinds}, headers=self._headers()
            )
        )
        return None if r.status_code == 204 else r.json()

    def input(self, job: dict[str, Any], name: str) -> bytes:
        url = f"{self.base_url}/worker/v1/jobs/{job['job_id']}/inputs/{name}"
        return self._check(self.http.get(url, headers=self._headers(job["lease_token"]))).content

    def heartbeat(self, job: dict[str, Any], progress: str) -> None:
        url = f"{self.base_url}/worker/v1/jobs/{job['job_id']}/heartbeat"
        self._check(
            self.http.post(
                url, json={"progress": progress}, headers=self._headers(job["lease_token"])
            )
        )

    def fail(self, job: dict[str, Any], error: str, retryable: bool) -> None:
        url = f"{self.base_url}/worker/v1/jobs/{job['job_id']}/fail"
        body = {"error": error[:2000], "retryable": retryable}
        self._check(self.http.post(url, json=body, headers=self._headers(job["lease_token"])))

    def complete(
        self, job: dict[str, Any], result: dict[str, Any], files: dict[str, bytes]
    ) -> None:
        url = f"{self.base_url}/worker/v1/jobs/{job['job_id']}/complete"
        parts: list[tuple[str, tuple[str | None, bytes, str]]] = [
            ("result", (None, json.dumps(result).encode(), "application/json"))
        ]
        for name, data in files.items():
            parts.append((name, (name, data, "application/octet-stream")))
        self._check(
            self.http.post(url, files=parts, headers=self._headers(job["lease_token"]), timeout=300)
        )

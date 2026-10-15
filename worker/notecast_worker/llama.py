"""Managed llama-server lifecycle and the GPU guard (ARCHITECTURE §6.3).

The worker owns llama-server: it starts it when a GPU job arrives and stops
it after an idle period, so the model doesn't sit in VRAM while Eric games.
"""

from __future__ import annotations

import logging
import os
import shutil
import signal
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path

import httpx

log = logging.getLogger(__name__)

AMD_VENDOR = "0x1002"

# Unload after this long without a job, so a game gets the VRAM back but
# loads (the risky moment on this GPU, see below) stay rare.
DEFAULT_IDLE_UNLOAD = 30 * 60

# On roundabits (RX 6900 XT, kernel 7.2.8, RADV) model load / context creation
# intermittently dies with VK_ERROR_DEVICE_LOST while the kernel resets the
# sdma1 ring, most often when reloading shortly after a teardown. Once
# loaded, inference is stable. So: let the GPU settle after a stop, and retry
# a failed load after a pause. See ~/notecast-eval/RESULTS.md.
DEFAULT_RETRY_DELAYS = (20.0, 60.0, 60.0)
DEFAULT_SETTLE_AFTER_STOP = 30.0
DEVICE_LOST_MARKERS = ("ErrorDeviceLost", "context is lost", "device lost")

_last_stop = 0.0  # monotonic time of the last llama-server teardown, any instance


class LoadFailed(RuntimeError):
    def __init__(self, msg: str, *, device_lost: bool, exited: bool) -> None:
        super().__init__(msg)
        self.device_lost = device_lost
        self.exited = exited


def retry_delay(err: Exception, attempt: int, delays: tuple[float, ...]) -> float | None:
    """Seconds to wait before retrying load attempt `attempt` (0-based) after
    `err`, or None to give up. Only GPU device loss and the process dying
    during load are retried; a hang (timeout) or anything else is not."""
    if not isinstance(err, LoadFailed) or not (err.device_lost or err.exited):
        return None
    if attempt >= len(delays):
        return None
    return delays[attempt]


@dataclass
class LlamaModel:
    """One llama-server configuration: weights, projector and flags."""

    name: str
    model: Path
    mmproj: Path | None = None
    ctx_size: int = 16384
    # Passed through verbatim, e.g. ["--image-max-tokens", "4096"].
    extra_args: list[str] = field(default_factory=list)
    # Extra environment for llama-server, e.g. GGML_VK_* driver workarounds.
    env: dict[str, str] = field(default_factory=dict)
    # Rough VRAM needed, for the GPU guard. Defaults to file sizes + 2.5 GiB.
    vram_mib: int | None = None

    def needed_vram_mib(self) -> int:
        if self.vram_mib is not None:
            return self.vram_mib
        size = self.model.stat().st_size + (self.mmproj.stat().st_size if self.mmproj else 0)
        return size // (1 << 20) + 2560


# The GPU is off by default. On roundabits (RX 6900 XT, kernel 7.2.x) every
# llama.cpp Vulkan load path ended in amdgpu device loss (sdma1 ring
# timeouts), and repeated attempts took the desktop session down with them
# (2026-10-14, see docs/ARCHITECTURE.md §11a). Turning it back on is an
# explicit decision: NOTECAST_ALLOW_GPU=1 plus a device name.
CPU = "none"


def gpu_allowed() -> bool:
    return os.environ.get("NOTECAST_ALLOW_GPU") == "1"


def child_env(device: str, extra: dict[str, str]) -> dict[str, str] | None:
    """Environment for llama-server. On CPU, hide every Vulkan driver so the
    process can't open a GPU context at all, not merely choose not to."""
    if device == CPU:
        return {**os.environ, **extra, "VK_LOADER_DRIVERS_DISABLE": "*"}
    return {**os.environ, **extra} if extra else None


class LlamaServer:
    def __init__(
        self,
        model: LlamaModel,
        port: int = 8092,
        device: str = CPU,
        binary: str | None = None,
        log_path: Path | None = None,
        retry_delays: tuple[float, ...] = DEFAULT_RETRY_DELAYS,
        settle_after_stop: float = DEFAULT_SETTLE_AFTER_STOP,
        sleep=time.sleep,
    ) -> None:
        if device != CPU and not gpu_allowed():
            raise RuntimeError(
                f"GPU device {device!r} requested but the GPU is disabled "
                "(it crashes roundabits; see docs/ARCHITECTURE.md §11a). "
                "Set NOTECAST_ALLOW_GPU=1 only after that is fixed."
            )
        self.model = model
        self.port = port
        self.device = device
        self.binary = binary or shutil.which("llama-server") or "llama-server"
        self.log_path = log_path
        self.retry_delays = retry_delays
        self.settle_after_stop = settle_after_stop
        self._sleep = sleep
        self.proc: subprocess.Popen[bytes] | None = None
        self.last_used = 0.0
        self.load_attempts = 0  # cumulative, for stats
        self.load_failures = 0

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}/v1"

    def running(self) -> bool:
        return self.proc is not None and self.proc.poll() is None

    def start(self, timeout: float = 300.0) -> None:
        """Start llama-server and wait until healthy, retrying device-lost loads."""
        if self.running():
            return
        attempt = 0
        while True:
            self._settle()
            self.load_attempts += 1
            try:
                self._start_once(timeout)
                return
            except LoadFailed as e:
                self.load_failures += 1
                delay = retry_delay(e, attempt, self.retry_delays)
                if delay is None:
                    raise
                log.warning("llama-server load failed (%s); retrying in %.0fs", e, delay)
                self._sleep(delay)
                attempt += 1

    def _settle(self) -> None:
        if self.device == "none":
            return
        wait = self.settle_after_stop - (time.monotonic() - _last_stop)
        if _last_stop and wait > 0:
            log.info("waiting %.0fs for the GPU to settle after the last unload", wait)
            self._sleep(wait)

    def _cmd(self) -> list[str]:
        m = self.model
        cmd = [
            self.binary,
            "--host", "127.0.0.1",
            "--port", str(self.port),
            "--device", self.device,  # never split onto the Intel iGPU (Vulkan1)
            "-ngl", "0" if self.device == "none" else "999",
            "-c", str(m.ctx_size),
            "--parallel", "1",
            "--reasoning", "off",
            "-t", str(max(1, (os.cpu_count() or 2) // 2)),
            "-m", str(m.model),
        ]  # fmt: skip
        if m.mmproj:
            cmd += ["--mmproj", str(m.mmproj)]
        return cmd + m.extra_args

    def _start_once(self, timeout: float) -> None:
        m = self.model
        cmd = self._cmd()
        log.info("starting llama-server: %s", " ".join(cmd))
        log_start = self.log_path.stat().st_size if self.log_path and self.log_path.exists() else 0
        out = self.log_path.open("ab") if self.log_path else subprocess.DEVNULL
        env = child_env(self.device, m.env)
        # Own process group, so stop() takes down anything it forks too.
        self.proc = subprocess.Popen(
            cmd, stdout=out, stderr=subprocess.STDOUT, start_new_session=True, env=env
        )
        self._log_child_env()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                code = self.proc.returncode
                self.proc = None
                _mark_stopped()
                lost = self._log_has_device_lost(log_start)
                raise LoadFailed(
                    f"llama-server exited with {code} while loading"
                    + (" (GPU device lost)" if lost else ""),
                    device_lost=lost,
                    exited=True,
                )
            try:
                if httpx.get(f"http://127.0.0.1:{self.port}/health", timeout=2).status_code == 200:
                    self.last_used = time.monotonic()
                    log.info("llama-server ready (%s)", m.name)
                    return
            except httpx.TransportError:
                pass
            time.sleep(0.5)
        self.stop()
        raise TimeoutError(f"llama-server not healthy after {timeout:.0f}s")

    def _log_child_env(self) -> None:
        """Record the GGML_*/RADV_* environment the child actually got."""
        if self.proc is None:
            return
        try:
            raw = Path(f"/proc/{self.proc.pid}/environ").read_bytes().split(b"\0")
        except OSError:
            return
        keys = (b"GGML_", b"RADV_", b"AMD_", b"MESA_", b"VK_")
        seen = " ".join(e.decode(errors="replace") for e in raw if e.startswith(keys))
        msg = f"notecast: llama-server env: {seen or '(none)'}"
        log.info(msg)
        if self.log_path:
            with self.log_path.open("a") as f:
                f.write(msg + "\n")

    def _log_has_device_lost(self, offset: int) -> bool:
        if not self.log_path or not self.log_path.exists():
            return False
        with self.log_path.open("rb") as f:
            f.seek(offset)
            tail = f.read().decode(errors="replace")
        return any(mark in tail for mark in DEVICE_LOST_MARKERS)

    def has_died(self) -> bool:
        """True if a started server is no longer running (e.g. GPU reset)."""
        return self.proc is not None and self.proc.poll() is not None

    def stop(self) -> None:
        if self.proc is None:
            return
        if self.proc.poll() is None:
            log.info("stopping llama-server (%s)", self.model.name)
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
                self.proc.wait(timeout=20)
            except subprocess.TimeoutExpired:
                os.killpg(self.proc.pid, signal.SIGKILL)
                self.proc.wait()
            except ProcessLookupError:
                pass
        self.proc = None
        _mark_stopped()

    def touch(self) -> None:
        self.last_used = time.monotonic()

    def stop_if_idle(self, idle_seconds: float = DEFAULT_IDLE_UNLOAD) -> bool:
        if self.running() and time.monotonic() - self.last_used > idle_seconds:
            self.stop()
            return True
        return False

    def __enter__(self) -> LlamaServer:
        self.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self.stop()


def _mark_stopped() -> None:
    global _last_stop
    _last_stop = time.monotonic()


# --- GPU guard -------------------------------------------------------------


@dataclass(frozen=True)
class Vram:
    total_mib: int
    used_mib: int

    @property
    def free_mib(self) -> int:
        return self.total_mib - self.used_mib


def amd_vram(drm_root: Path = Path("/sys/class/drm")) -> Vram | None:
    """VRAM of the largest AMD card, from amdgpu's sysfs counters."""
    best: Vram | None = None
    for dev in sorted(drm_root.glob("card[0-9]*/device")):
        try:
            if (dev / "vendor").read_text().strip() != AMD_VENDOR:
                continue
            total = int((dev / "mem_info_vram_total").read_text())
            used = int((dev / "mem_info_vram_used").read_text())
        except (OSError, ValueError):
            continue
        v = Vram(total >> 20, used >> 20)
        if best is None or v.total_mib > best.total_mib:
            best = v
    return best


def gpu_available(needed_mib: int, server_running: bool, vram: Vram | None) -> bool:
    """Whether to advertise GPU job kinds right now.

    A running llama-server already holds its VRAM, so it counts as available.
    No readable AMD card means we can't tell; fail open and let llama-server
    report the error.
    """
    if server_running or vram is None:
        return True
    return vram.free_mib >= needed_mib

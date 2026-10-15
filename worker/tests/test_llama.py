from pathlib import Path

from notecast_worker.llama import Vram, amd_vram, gpu_available


def card(root: Path, n: int, vendor: str, total: int | None, used: int | None) -> None:
    d = root / f"card{n}" / "device"
    d.mkdir(parents=True)
    (d / "vendor").write_text(vendor + "\n")
    if total is not None:
        (d / "mem_info_vram_total").write_text(str(total))
        (d / "mem_info_vram_used").write_text(str(used))


def test_amd_vram_picks_amd_card_and_ignores_intel(tmp_path):
    card(tmp_path, 0, "0x8086", None, None)
    card(tmp_path, 1, "0x1002", 16 << 30, 2 << 30)
    assert amd_vram(tmp_path) == Vram(16384, 2048)


def test_amd_vram_none_without_amd(tmp_path):
    card(tmp_path, 0, "0x8086", None, None)
    assert amd_vram(tmp_path) is None


def test_gpu_guard():
    assert gpu_available(10000, False, Vram(16384, 2000))
    assert not gpu_available(10000, False, Vram(16384, 9000))
    assert gpu_available(10000, True, Vram(16384, 15000))  # our own server holds it
    assert gpu_available(10000, False, None)  # unknown: fail open


def test_server_passes_model_env(tmp_path, monkeypatch):
    import subprocess

    import pytest

    from notecast_worker.llama import LlamaModel, LlamaServer

    seen = {}

    class Stop(Exception):
        pass

    def fake_popen(cmd, **kw):
        seen.update(kw, cmd=cmd)
        raise Stop

    monkeypatch.setattr(subprocess, "Popen", fake_popen)
    monkeypatch.setenv("KEEP_ME", "1")
    gguf = tmp_path / "m.gguf"
    gguf.write_bytes(b"x")
    m = LlamaModel("m", gguf, env={"GGML_VK_FOO": "1"}, extra_args=["--x"])
    with pytest.raises(Stop):
        LlamaServer(m, binary="llama-server", settle_after_stop=0).start()
    assert seen["env"]["GGML_VK_FOO"] == "1"
    assert seen["env"]["KEEP_ME"] == "1"  # merged with, not replacing, the environment
    assert seen["cmd"][-1] == "--x"
    assert seen["start_new_session"] is True


def _server(tmp_path, **kw):
    from notecast_worker.llama import LlamaModel, LlamaServer

    gguf = tmp_path / "m.gguf"
    gguf.write_bytes(b"x")
    slept: list[float] = []
    srv = LlamaServer(LlamaModel("m", gguf), binary="x", sleep=slept.append, **kw)
    return srv, slept


def _flaky(failures):
    """A _start_once that raises the given errors in order, then succeeds."""
    errs = list(failures)

    def start_once(timeout):
        if errs:
            raise errs.pop(0)

    return start_once


def test_retry_delay_policy():
    from notecast_worker.llama import LoadFailed, retry_delay

    lost = LoadFailed("x", device_lost=True, exited=True)
    exited = LoadFailed("x", device_lost=False, exited=True)
    assert retry_delay(lost, 0, (20, 60)) == 20
    assert retry_delay(exited, 1, (20, 60)) == 60
    assert retry_delay(lost, 2, (20, 60)) is None  # out of retries
    assert retry_delay(TimeoutError("hang"), 0, (20, 60)) is None
    assert retry_delay(LoadFailed("x", device_lost=False, exited=False), 0, (20,)) is None


def test_start_retries_device_lost_then_succeeds(tmp_path):
    from notecast_worker.llama import LoadFailed

    srv, slept = _server(tmp_path, retry_delays=(20, 60, 60), settle_after_stop=0)
    lost = LoadFailed("lost", device_lost=True, exited=True)
    srv._start_once = _flaky([lost, lost])
    srv.start()
    assert slept == [20, 60]
    assert (srv.load_attempts, srv.load_failures) == (3, 2)


def test_start_gives_up_after_retries(tmp_path):
    import pytest

    from notecast_worker.llama import LoadFailed

    srv, slept = _server(tmp_path, retry_delays=(20, 60), settle_after_stop=0)
    lost = LoadFailed("lost", device_lost=True, exited=True)
    srv._start_once = _flaky([lost, lost, lost, lost])
    with pytest.raises(LoadFailed):
        srv.start()
    assert slept == [20, 60]
    assert srv.load_attempts == 3


def test_start_does_not_retry_timeouts(tmp_path):
    import pytest

    srv, slept = _server(tmp_path, settle_after_stop=0)
    srv._start_once = _flaky([TimeoutError("hung")])
    with pytest.raises(TimeoutError):
        srv.start()
    assert slept == []


def test_settle_waits_after_recent_stop(tmp_path, monkeypatch):
    import time

    import notecast_worker.llama as L

    monkeypatch.setattr(L, "_last_stop", time.monotonic())
    monkeypatch.setenv("NOTECAST_ALLOW_GPU", "1")  # settling only applies to the GPU path
    srv, slept = _server(tmp_path, settle_after_stop=30, device="Vulkan0")
    srv._start_once = _flaky([])
    srv.start()
    assert len(slept) == 1 and 29 < slept[0] <= 30
    cpu, slept_cpu = _server(tmp_path, settle_after_stop=30, device="none")
    cpu._start_once = _flaky([])
    cpu.start()
    assert slept_cpu == []  # no GPU, nothing to settle


def test_device_lost_detected_from_new_log_lines_only(tmp_path):
    srv, _ = _server(tmp_path, log_path=tmp_path / "srv.log")
    srv.log_path.write_text("old run: vk::Queue::submit: ErrorDeviceLost\n")
    offset = srv.log_path.stat().st_size
    assert not srv._log_has_device_lost(offset)
    with srv.log_path.open("a") as f:
        f.write("radv/amdgpu: The CS has been cancelled because the context is lost.\n")
    assert srv._log_has_device_lost(offset)


def test_gpu_refused_without_opt_in(monkeypatch, tmp_path):
    import pytest

    from notecast_worker.llama import LlamaModel, LlamaServer

    monkeypatch.delenv("NOTECAST_ALLOW_GPU", raising=False)
    m = LlamaModel(name="x", model=tmp_path / "m.gguf")
    with pytest.raises(RuntimeError, match="GPU is disabled"):
        LlamaServer(m, device="Vulkan0")
    assert LlamaServer(m).device == "none"  # CPU is the default


def test_cpu_hides_vulkan_drivers(monkeypatch):
    from notecast_worker.llama import child_env

    env = child_env("none", {"FOO": "1"})
    assert env["VK_LOADER_DRIVERS_DISABLE"] == "*" and env["FOO"] == "1"
    monkeypatch.setenv("NOTECAST_ALLOW_GPU", "1")
    assert child_env("Vulkan0", {}) is None

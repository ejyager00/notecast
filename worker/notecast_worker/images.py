"""Page image normalization (ARCHITECTURE §3.1).

Untrusted image bytes are decoded only here, on the worker, never on the
server. HEIC goes through libheif's `heif-dec` CLI rather than a Python
binding, which Guix doesn't package.
"""

from __future__ import annotations

import io
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

from PIL import Image, ImageOps

# Refuse decompression bombs well above any phone camera (48 MP ~ 8064x6048).
Image.MAX_IMAGE_PIXELS = 80_000_000

HEIF_BRANDS = (b"heic", b"heix", b"hevc", b"hevx", b"mif1", b"msf1")


def is_heif(data: bytes) -> bool:
    return len(data) >= 12 and data[4:8] == b"ftyp" and data[8:12] in HEIF_BRANDS


def decode(data: bytes) -> Image.Image:
    """Decode JPEG/PNG/WebP/HEIC bytes to an upright RGB image."""
    if is_heif(data):
        img = _decode_heif(data)
    else:
        img = Image.open(io.BytesIO(data))
        img.load()
    img = ImageOps.exif_transpose(img)
    return img.convert("RGB")


def _decode_heif(data: bytes) -> Image.Image:
    heif_dec = shutil.which("heif-dec")
    if heif_dec is None:
        raise RuntimeError("heif-dec not found (Guix package libheif)")
    with tempfile.TemporaryDirectory(prefix="notecast-heif-") as d:
        src, dst = Path(d, "in.heic"), Path(d, "out.png")
        src.write_bytes(data)
        # heif-dec applies the HEIF rotation itself; PNG keeps it lossless.
        subprocess.run([heif_dec, str(src), str(dst)], check=True, capture_output=True, timeout=120)
        img = Image.open(dst)
        img.load()
        return img


def fit(img: Image.Image, long_edge: int | None) -> Image.Image:
    """Downscale so the long edge is at most `long_edge` (None = unchanged)."""
    if long_edge is None or max(img.size) <= long_edge:
        return img
    out = img.copy()
    out.thumbnail((long_edge, long_edge), Image.Resampling.LANCZOS)
    return out


def to_jpeg(img: Image.Image, quality: int = 85) -> bytes:
    buf = io.BytesIO()
    img.save(buf, "JPEG", quality=quality, optimize=True)
    return buf.getvalue()


@dataclass(frozen=True)
class Normalized:
    ocr: bytes  # JPEG sent to the vision model
    display: bytes  # JPEG served by the app
    thumb: bytes  # JPEG for lists


def normalize(data: bytes, ocr_long_edge: int | None = 2048) -> Normalized:
    img = decode(data)
    return Normalized(
        ocr=to_jpeg(fit(img, ocr_long_edge), quality=92),
        display=to_jpeg(fit(img, 2048)),
        thumb=to_jpeg(fit(img, 400), quality=80),
    )

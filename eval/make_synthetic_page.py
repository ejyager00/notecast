"""Render a fake handwritten page from text, for plumbing tests only.

A handwriting *font* is far easier to read than real handwriting, so these
pages say nothing about OCR accuracy. Use them to exercise the pipeline and
as committed test fixtures, never her notes.

    python eval/make_synthetic_page.py TEXT_FILE FONT.ttf OUT.jpg
"""

from __future__ import annotations

import random
import sys
from pathlib import Path

from PIL import Image, ImageDraw, ImageFilter, ImageFont


def main() -> None:
    text_file, font_file, out = map(Path, sys.argv[1:4])
    rnd = random.Random(42)
    w, h = 1700, 2200  # roughly letter paper at 200 dpi
    img = Image.new("RGB", (w, h), (246, 243, 232))
    d = ImageDraw.Draw(img)
    for y in range(180, h - 80, 66):  # ruled lines
        d.line([(0, y), (w, y)], fill=(170, 195, 225), width=2)
    d.line([(160, 0), (160, h)], fill=(225, 150, 150), width=2)
    y = 180 - 52
    for line in text_file.read_text().splitlines():
        size = 62 if line.startswith("#") else 50
        font = ImageFont.truetype(str(font_file), size)
        x = 190 + (60 if line.startswith("  ") else 0)
        d.text(
            (x + rnd.randint(-4, 4), y + rnd.randint(-3, 3)),
            line.lstrip("# ").rstrip(),
            font=font,
            fill=(30, 40, 90),
        )
        y += 66
    img = img.rotate(
        rnd.uniform(-1.5, 1.5),
        resample=Image.Resampling.BICUBIC,
        fillcolor=(90, 80, 70),
    ).filter(ImageFilter.GaussianBlur(0.6))
    img.save(out, "JPEG", quality=88)


if __name__ == "__main__":
    main()

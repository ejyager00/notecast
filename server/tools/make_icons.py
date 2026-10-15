"""Regenerate the app icons in internal/web/static (run from server/).

    guix shell python python-pillow -- python3 tools/make_icons.py
"""

from PIL import Image, ImageDraw

ACCENT = (47, 93, 138)
PAPER = (251, 250, 247)


def icon(size: int) -> Image.Image:
    s = 4  # supersample, then downscale for smooth edges
    n = size * s
    img = Image.new("RGB", (n, n), ACCENT)
    d = ImageDraw.Draw(img)
    # A page...
    pw, ph = int(n * 0.42), int(n * 0.56)
    px, py = int(n * 0.16), (n - ph) // 2
    d.rounded_rectangle([px, py, px + pw, py + ph], radius=int(n * 0.04), fill=PAPER)
    for i in range(4):  # ...with lines of writing
        y = py + int(ph * (0.22 + i * 0.17))
        d.line([px + int(pw * 0.16), y, px + int(pw * (0.84 if i < 3 else 0.55)), y],
               fill=ACCENT, width=max(2, int(n * 0.022)))
    # ...and sound coming off it.
    cx, cy = px + pw + int(n * 0.02), n // 2
    for i, r in enumerate((0.12, 0.21, 0.30)):
        rr = int(n * r)
        d.arc([cx - rr, cy - rr, cx + rr, cy + rr], start=-50, end=50,
              fill=PAPER, width=max(3, int(n * (0.045 - i * 0.006))))
    return img.resize((size, size), Image.Resampling.LANCZOS)


for size in (180, 512):
    icon(size).save(f"internal/web/static/icon-{size}.png", optimize=True)

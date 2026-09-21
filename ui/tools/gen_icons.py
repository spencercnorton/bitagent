"""Generate the PWA/home-screen/browser icon set from the brand mark.

The brand mark is now a raster illustration (the teal ringed planet), not
the old geometric SVG glyph, so this resizes one master instead of redrawing
geometry with Pillow primitives. The master IS `static/img/icon-512.png`
(512x512 RGBA, rounded-rect art on transparent corners) — regenerating is
therefore idempotent.

Run from the repo root after replacing the master:

    python3 tools/gen_icons.py

Outputs (committed, served from /static/img/):
  icon-192.png                          transparent, rounded-rect (manifest "any")
  icon-maskable-192.png, icon-maskable-512.png
                                        full-bleed (manifest "maskable")
  apple-touch-icon.png                  180x180 full-bleed (iOS rounds corners)
  favicon.ico                           16/32/48 browser tab icon
"""
from pathlib import Path

from PIL import Image, ImageFilter

MASTER = "icon-512.png"


def bleed(master: Image.Image) -> Image.Image:
    """Full-bleed variant for maskable/iOS.

    The mark's own rounded corners are transparent, and a flat fill behind
    them seams against the art's vignette — so the corners are filled with a
    blurred, zoomed copy of the mark itself and the mark is then zoomed just
    past its corner radius so it reads edge-to-edge instead of framed.
    """
    w = master.width

    def zoom(factor: float) -> Image.Image:
        z = int(w * factor)
        big = master.resize((z, z), Image.LANCZOS)
        return big.crop(((z - w) // 2,) * 2 + ((z + w) // 2,) * 2)

    out = Image.new("RGBA", master.size, master.getpixel((w // 2, int(w * 0.06)))[:3] + (255,))
    out.alpha_composite(zoom(1.5).filter(ImageFilter.GaussianBlur(w * 0.06)))
    out.alpha_composite(zoom(1.22))
    return out


def main() -> None:
    out = Path(__file__).resolve().parent.parent / "static" / "img"
    master = Image.open(out / MASTER).convert("RGBA")
    full = bleed(master)
    jobs = {
        "icon-192.png": master.resize((192, 192), Image.LANCZOS),
        "icon-maskable-192.png": full.resize((192, 192), Image.LANCZOS),
        "icon-maskable-512.png": full,
        "apple-touch-icon.png": full.resize((180, 180), Image.LANCZOS),
    }
    for name, img in jobs.items():
        img.save(out / name, optimize=True)
        print(f"wrote {out / name}")

    master.resize((256, 256), Image.LANCZOS).save(
        out / "favicon.ico", sizes=[(16, 16), (32, 32), (48, 48)]
    )
    print(f"wrote {out / 'favicon.ico'}")


if __name__ == "__main__":
    main()

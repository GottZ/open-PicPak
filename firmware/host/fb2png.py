#!/usr/bin/env python3
"""Decode a 30000-byte PicPak BWRY framebuffer (2bpp MSB-first, vertically
pre-flipped for the panel) into a PNG showing what the VIEWER sees.
Usage: fb2png.py <raw.bin> <out.png>"""
import sys
from PIL import Image

W, H = 400, 300
PAL = {0: (0, 0, 0), 1: (255, 255, 255), 2: (247, 200, 0), 3: (190, 15, 15)}  # K W Y R

data = open(sys.argv[1], "rb").read()
assert len(data) == W * H // 4, f"expected {W*H//4} bytes, got {len(data)}"
img = Image.new("RGB", (W, H))
px = img.load()
for y in range(H):              # y = viewer row (0 = top)
    sy = H - 1 - y              # buffer is packed vertically flipped -> undo it
    for x in range(W):
        idx = sy * W + x
        byte = data[idx // 4]
        shift = (3 - (idx % 4)) * 2   # MSB-first: leftmost pixel in high bits
        px[x, y] = PAL[(byte >> shift) & 3]
img.save(sys.argv[2])
# 4x nearest upscale for easy eyeballing
img.resize((W * 2, H * 2), Image.NEAREST).save(sys.argv[2].replace(".png", ".x2.png"))
print(f"wrote {sys.argv[2]} (+ .x2)")

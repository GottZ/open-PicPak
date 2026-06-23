#!/usr/bin/env python3
"""Generate PicPak e-ink onboarding screens (400x300 BWRY).

Two-font, size-matched pixel hierarchy (each rendered at its NATIVE px size, no AA):
  - Pixel Operator Bold @16  -> headlines / header labels   (UI font)
  - Pixel Operator 8   @8     -> body / hints / footer        (UI font, small)
  - Perfect DOS VGA 437 @16   -> URL + CLI command            (monospace "code")
No QR (Web Serial needs a desktop browser).

Output: <name>.png (panel-accurate), <name>.x2.png, and screens.h
(30000 B, 4px/byte MSB-first, 0=K 1=W 2=Y 3=R, vertically flipped — panel Y mirrored).
"""
import os
from PIL import Image, ImageDraw, ImageFont
from collections import Counter

W, H = 400, 300
F = os.path.join(os.path.dirname(__file__), "fonts")
K, WHITE, Y, R = 0, 1, 2, 3
PAL_RGB = {K:(0,0,0), WHITE:(255,255,255), Y:(247,200,0), R:(190,15,15)}

def f(path, size): return ImageFont.truetype(os.path.join(F, path), size)
H16 = f("po/PixelOperator-Bold.ttf", 16)   # headline / labels
B8  = f("po/PixelOperator8.ttf", 8)        # body / hints / footer (native 8px)
MONO= f("pd/Perfect DOS VGA 437.ttf", 16)  # URL / CLI (monospace, native 16px)

try: VERSION = "v" + open(os.path.join(os.path.dirname(__file__), "..", "version.txt")).read().strip()
except Exception: VERSION = "v0.0.0"
URL = "picpak.local"   # placeholder host shown on the setup screen; set to your own setup endpoint
GH  = "github.com/GottZ/open-PicPak"

def canvas():
    img=Image.new("RGB",(W,H),PAL_RGB[WHITE]); d=ImageDraw.Draw(img); d.fontmode="1"; return img,d
def t(d,xy,s,fnt,rgb,anchor="la"): d.text(xy,s,font=fnt,fill=rgb,anchor=anchor)
def tw(d,s,fnt): return d.textlength(s,font=fnt)
def dots(d,x_right,cy,rgb,r=6,gap=20):
    for i in range(3): d.ellipse([x_right-i*gap-r,cy-r,x_right-i*gap+r,cy+r],fill=rgb)

def url_chip(d, x, y, fill=None, outline=None):
    """Single-line monospace URL on a chip; returns bottom y."""
    pad=8; w=tw(d,URL,MONO)+2*pad; h=16+2*pad
    box=[x,y,x+w,y+h]
    if fill is not None: d.rectangle(box,fill=fill)
    if outline is not None: d.rectangle(box,outline=outline,width=2)
    t(d,(x+pad,y+pad),URL,MONO,PAL_RGB[K])
    return y+h

def footer(d):
    d.line([0,274,W,274],fill=PAL_RGB[K],width=1)
    t(d,(12,282),GH,B8,PAL_RGB[K]); t(d,(W-12,286),VERSION,B8,PAL_RGB[K],anchor="rm")

# ================================================================ Screen: setup
def screen_onboarding():
    img,d=canvas()
    d.rectangle([0,0,W,46],fill=PAL_RGB[R])
    t(d,(14,23),"open PicPak",H16,PAL_RGB[WHITE],anchor="lm")
    t(d,(W-14,23),"SETUP",H16,PAL_RGB[WHITE],anchor="rm")

    t(d,(14,74),"Wi-Fi not configured",H16,PAL_RGB[K])
    t(d,(14,112),"Set up from a desktop browser",B8,PAL_RGB[K])
    t(d,(14,128),"(Chrome / Edge):",B8,PAL_RGB[K])
    by=url_chip(d,14,150,fill=PAL_RGB[Y])
    t(d,(14,by+14),"or via CLI:",B8,PAL_RGB[K])
    t(d,(14,by+28),"dev-flash.sh provision",MONO,PAL_RGB[K])
    footer(d)
    return img

# ========================================================== Screen: console mode
def screen_console():
    img,d=canvas()
    d.rectangle([0,0,W,46],fill=PAL_RGB[Y])
    t(d,(14,23),"SETUP MODE",H16,PAL_RGB[K],anchor="lm")
    dots(d,W-16,23,PAL_RGB[R])

    t(d,(14,74),"Setup console ",H16,PAL_RGB[K])
    t(d,(14,96),"is open",H16,PAL_RGB[R])
    t(d,(14,128),"Connect, then choose",B8,PAL_RGB[K])
    t(d,(14,144),"'Change Wi-Fi' at:",B8,PAL_RGB[K])
    by=url_chip(d,14,166,outline=PAL_RGB[K])
    t(d,(14,by+14),"or via CLI:",B8,PAL_RGB[K])
    t(d,(14,by+28),"dev-flash.sh provision",MONO,PAL_RGB[K])
    footer(d)
    return img

# ===================================================================== packing
def quantize(img):
    px=img.load(); codes=bytearray(W*H); items=list(PAL_RGB.items()); cache={}
    for y in range(H):
        for x in range(W):
            rgb=px[x,y]; c=cache.get(rgb)
            if c is None:
                c=min(items,key=lambda kv:sum((a-b)**2 for a,b in zip(kv[1],rgb)))[0]; cache[rgb]=c
            codes[y*W+x]=c
    return codes
def pack(codes, flip_v=True):
    out=bytearray(W*H//4)
    for y in range(H):
        sy=(H-1-y) if flip_v else y
        for xb in range(W//4):
            b=0
            for i in range(4): b=(b<<2)|(codes[sy*W+xb*4+i]&3)
            out[y*(W//4)+xb]=b
    return bytes(out)
def preview(codes):
    im=Image.new("RGB",(W,H)); im.putdata([PAL_RGB[c] for c in codes]); return im
def emit(name,data):
    out=[f"static const uint8_t {name}[{len(data)}] = {{"]
    for i in range(0,len(data),16):
        out.append("  "+",".join(f"0x{b:02x}" for b in data[i:i+16])+",")
    out.append("};"); return "\n".join(out)

def main():
    o=os.path.dirname(__file__); parts=['#pragma once','#include <stdint.h>','']
    for name,img in {"onboarding":screen_onboarding(),"console":screen_console()}.items():
        codes=quantize(img); pv=preview(codes)
        pv.save(f"{o}/{name}.png"); pv.resize((W*2,H*2),Image.NEAREST).save(f"{o}/{name}.x2.png")
        packed=pack(codes,True)    # pre-mirror for the panel's Y-flipped gate scan (factory PSR 0x07,0x29);
                                   # epd.c then streams a plain linear block (no per-frame transform on-device)
        parts += [f"/* {name} — 400x300 BWRY, pre-mirrored for the panel (gate-scan Y-flipped) */", emit(f"screen_{name}",packed), ""]
        h=Counter(codes); print(f"{name}: K={h[K]} W={h[WHITE]} Y={h[Y]} R={h[R]} packed={len(packed)}B")
    out=os.path.join(o,"..","main","screens.h")
    open(out,"w").write("\n".join(parts)); print(f"wrote {out} (+ preview PNGs in screens-src/)")

if __name__=="__main__": main()

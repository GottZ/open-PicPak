#!/usr/bin/env python3
"""Embed main/render.be into main/render_script.h as a C string (build step).
The .be script is the source of truth; the header is a generated artifact."""
import os
HERE = os.path.dirname(__file__)
BE = os.path.join(HERE, "..", "main", "render.be")
H = os.path.join(HERE, "..", "main", "render_script.h")
src = open(BE).read()
esc = src.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
open(H, "w").write('#pragma once\n/* generated from render.be by host/gen_render.py — do not edit here */\n'
                   'static const char RENDER_BE[] =\n    "%s";\n' % esc)
print("wrote", os.path.relpath(H), "(%d escaped chars)" % len(esc))

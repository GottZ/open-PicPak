#!/usr/bin/env python3
"""Embed main/policy.be into main/policy_script.h as a C string (build step), the net-phase
counterpart to gen_render.py. The .be script is the source of truth; the header is a
generated artifact (not committed)."""
import os
HERE = os.path.dirname(__file__)
BE = os.path.join(HERE, "..", "main", "policy.be")
H = os.path.join(HERE, "..", "main", "policy_script.h")
src = open(BE).read()
esc = src.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
open(H, "w").write('#pragma once\n/* generated from policy.be by host/gen_policy.py — do not edit here */\n'
                   'static const char POLICY_BE[] =\n    "%s";\n' % esc)
print("wrote", os.path.relpath(H), "(%d escaped chars)" % len(esc))

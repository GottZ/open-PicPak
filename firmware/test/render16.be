# host-PNG test fixture: exercise the 16px font + the text/text16 nil-safety hardening.
# Run via host/host_render against the live fb.c -> host/fb2png.py -> inspect the PNG.
fill(1)                                      # white background
text16(14, 16, "Hello 16px font", 0)         # 16px black
text16(14, 42, "0123456789 BWRY", 3)         # 16px red
text16(14, 68, "abcdefghijklm nop", 0)       # lowercase coverage
text(14, 100, "8px font for size compare", 0)
text16(14, 120, "scale x2:", 0)
text16(150, 116, "BIG", 0, 2)                # scaled 16px

# H4 negative probes: a non-string slot must be a clean no-op, never a crash.
text(14, 230, nil)                           # nil string  -> hardened text() returns nil
text16(14, 250, 42, 0)                       # int as string slot -> hardened text16() returns nil
text16(14, 272, "rendered past the nils OK", 0)   # proves execution continued

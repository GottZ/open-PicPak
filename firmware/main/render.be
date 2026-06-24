# PicPak Berry render demo — Hello World + full clean device vars + shapes + Pacman biting a red square.
# Factory NVS is JSON; Berry parses it itself (json module) — "config IS berry".
# Palette: 0=BLACK 1=WHITE 2=YELLOW 3=RED. Designed top-to-bottom; fb applies the panel y-flip.
import json
var K=0 var W=1 var Y=2 var R=3

fill(W)

# --- shapes BEHIND the text (drawn first; black text painted over them stays legible) ---
circle(58,24,22,Y)
circle(150,150,40,Y)

# --- red prey square + Pacman (right, facing LEFT) ---
rect(214,134,32,32,R,true)
disc(322,150,70,Y)
triangle(322,150, 248,108, 248,192, W)
disc(308,112,7,K)

# --- decorative lower-left ---
circle(70,262,18,R)

# --- parse factory identity from NVS JSON ---
var sn = "?"
var raw = nvs_str("storage","dev_sn")
if raw != nil
  var j = json.load(raw)
  if j != nil  sn = j.find("serial_number","?")  end
end
var cmap = {"N":"black","W":"white","B":"blue","R":"red"}
var color = "?"
if size(sn) > 0  color = cmap.find(sn[size(sn)-1],"?")  end
var rt = "?"
var cfg = nvs_str("storage","dev_config")
if cfg != nil
  var c = json.load(cfg)
  if c != nil  rt = str(int(c.find("refresh_time",0) / 3600)) + "h"  end
end
var lib = nvs_str("slots","library_id")
if lib == nil  lib = "?"  end
var batt = "n/a"
if dev_batt_mv() > 0  batt = "~" + str(dev_batt_mv()) + "mV " + str(dev_batt_pct()) + "%"  end

# --- foreground: headline + underline + variable column ---
text(14,12,"Hello World",K,2)
rect(14,30,176,4,Y,true)
text(14,42,"SN:   " + sn,K)
text(14,55,"MAC:  " + dev_mac(),K)
text(14,68,"BT:   " + dev_bt_mac(),K)
text(14,81,"Lib:  " + lib,K)
text(14,94,"Chip: " + dev_chip(),K)
text(14,107,"Color: " + color,K)
text(14,120,"Refresh: " + rt,K)
text(14,133,"Batt: " + batt,K)
text(14,146,"Up: " + str(int(dev_uptime_ms()/1000)) + "s  Reset: " + dev_reset(),K)

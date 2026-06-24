# host-PNG test fixture for qr(): two real QR payloads + an overlong no-crash probe.
fill(1)
text16(8, 4, "WiFi join:", 0)
qr(8, 30, "WIFI:T:WPA;S:PicPak;P:hunter2;;", 3)        # Wi-Fi join QR (black on white, scale 3)
text16(230, 4, "Web UI:", 0)
qr(238, 30, "http://192.168.1.50", 3)                  # LAN URL QR

# negative probe: a payload too long for QR_MAXVER(10) -> qr() draws nothing (no partial, no crash)
var big = ""
for i : 0 .. 40
  big += "0123456789ABCDEFGHIJ"                         # 41*20 = 820 chars, well over V10
end
qr(8, 236, big, 2)
text16(8, 244, "after overlong qr: OK", 0)             # proves execution continued past the failed qr

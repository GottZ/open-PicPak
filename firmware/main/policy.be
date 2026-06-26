# Net-phase Berry policy: scan visible APs, match them against store keys wifi.*,
# then connect by priority and RSSI. main.c stops WiFi after the policy window.
import json

def trim_pw(s)
  if s == nil
    return ""
  end
  var v = str(s)
  var b = 0
  var e = size(v) - 1
  while b <= e && (v[b] == " " || v[b] == "\t" || v[b] == "\r" || v[b] == "\n")
    b = b + 1
  end
  while e >= b && (v[e] == " " || v[e] == "\t" || v[e] == "\r" || v[e] == "\n")
    e = e - 1
  end
  if b > e
    return ""
  end
  return v[b..e]
end

if !store_ok()
  print("POLICY no store -> legacy fallback")
else
  var aps = wifi_scan()
  var seen = {}
  for ap: aps
    var s = ap.find("ssid", nil)
    var r = ap.find("rssi", -128)
    if r == nil  r = -128  end
    if s != nil
      var old = seen.find(s, nil)
      if old == nil || r > old  seen[s] = r  end
    end
  end

  var tried = {}
  while true
    var best_key = nil
    var best_ssid = nil
    var best_pass = nil
    var best_prio = -2147483648
    var best_rssi = -129
    var keys = store_keys("wifi.")
    for key: keys
      if !tried.find(key, false)
        var raw = store_get(key)
        if raw != nil
          var cfg = json.load(raw.asstring())
          if cfg != nil
            var ssid = cfg.find("ssid", nil)
            var pass = cfg.find("pass", "")
            var prio = cfg.find("prio", 0)
            var rssi = seen.find(ssid, nil)
            if rssi != nil && (prio > best_prio || (prio == best_prio && rssi > best_rssi))
              var rotated = store_get("wpw." + key)
              best_key = key
              best_ssid = ssid
              best_pass = trim_pw(rotated == nil ? pass : rotated.asstring())
              best_prio = prio
              best_rssi = rssi
            end
          end
        end
      end
    end

    if best_key == nil
      print("POLICY no known visible WiFi")
      break
    end
    tried[best_key] = true
    print("POLICY try " + best_key + " ssid=" + best_ssid + " rssi=" + str(best_rssi))
    if wifi_connect(best_ssid, best_pass)
      store_set("net.ok", best_ssid)
      store_set("net.ssid", best_ssid)
      store_set("net.rssi", str(best_rssi))
      rtc_set("slot", best_key)
      print("POLICY connected " + best_ssid)
      break
    end
  end
end

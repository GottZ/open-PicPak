/* Berry net surface: wifi_scan / wifi_connect / wifi_ip / wifi_ssid / wifi_rssi / wifi_stop /
 * http_get. Thin, hardened bindings over net.c. Registered in the policy phase (WiFi up), NOT
 * in the render phase (RF already off there). Hardening: arity + type gate at the entry, never
 * be_toint/be_tostring an unchecked slot; values are length-aware (be_pushbytes) so embedded
 * NULs survive.
 */
#include "net.h"
#include "berry.h"
#include <string.h>
#include <stdlib.h>

#define HTTP_GET_MAX 16384   /* hard cap, well under the ~115 KB WiFi+VM RAM spike (H3) */

/* wifi_scan() -> list of {ssid, rssi, auth} (empty list on scan error). */
static int l_wifi_scan(bvm *vm)
{
    net_ap_t aps[24];
    int n = net_wifi_scan(aps, (int)(sizeof aps / sizeof aps[0]));
    be_newlist(vm);                                  /* result list at the top */
    for (int i = 0; i < n; i++) {
        be_newmap(vm);                               /* map (list now at -2) */
        be_pushstring(vm, "ssid"); be_pushstring(vm, aps[i].ssid); be_data_insert(vm, -3); be_pop(vm, 2);
        be_pushstring(vm, "rssi"); be_pushint(vm, aps[i].rssi);    be_data_insert(vm, -3); be_pop(vm, 2);
        be_pushstring(vm, "auth"); be_pushint(vm, aps[i].auth);    be_data_insert(vm, -3); be_pop(vm, 2);
        be_data_push(vm, -2);                        /* append map to list */
        be_pop(vm, 1);
    }
    be_return(vm);
}

/* wifi_connect(ssid, pass[, timeout]) -> bool. Binds to the FORCING net_wifi_try (CB1), not
 * the idempotent net_wifi_connect, so scan-match / rotation can (re)associate per candidate. */
static int l_wifi_connect(bvm *vm)
{
    if (be_top(vm) < 2 || !be_isstring(vm, 1) || !be_isstring(vm, 2)) { be_pushbool(vm, 0); be_return(vm); }
    int timeout = (be_top(vm) >= 3 && be_isint(vm, 3)) ? be_toint(vm, 3) : 20000;
    be_pushbool(vm, net_wifi_try(be_tostring(vm, 1), be_tostring(vm, 2), timeout));
    be_return(vm);
}

static int l_wifi_ip(bvm *vm)
{
    char ip[20];
    if (net_ip(ip, sizeof ip)) { be_pushstring(vm, ip); be_return(vm); }
    be_return_nil(vm);
}

static int l_wifi_ssid(bvm *vm)
{
    char s[33];
    if (net_ssid(s, sizeof s)) { be_pushstring(vm, s); be_return(vm); }
    be_return_nil(vm);
}

static int l_wifi_rssi(bvm *vm)
{
    int r;
    if (net_rssi(&r)) { be_pushint(vm, r); be_return(vm); }
    be_return_nil(vm);
}

static int l_wifi_stop(bvm *vm)
{
    net_wifi_stop();
    be_return_nil(vm);
}

/* http_get(url) -> bytes | nil. URL validated in net_http_get (H3); response capped at
 * HTTP_GET_MAX. Bytes (not string) so an embedded NUL survives (the caller .asstring()s it). */
static int l_http_get(bvm *vm)
{
    if (be_top(vm) < 1 || !be_isstring(vm, 1)) be_return_nil(vm);
    uint8_t *buf = malloc(HTTP_GET_MAX);
    if (!buf) be_return_nil(vm);
    size_t len = 0;
    bool ok = net_http_get(be_tostring(vm, 1), buf, HTTP_GET_MAX, &len);
    if (ok) be_pushbytes(vm, buf, len);              /* copies into the Berry bytes object */
    free(buf);
    if (!ok) be_return_nil(vm);
    be_return(vm);
}

void net_register(bvm *vm)
{
    be_regfunc(vm, "wifi_scan",    l_wifi_scan);
    be_regfunc(vm, "wifi_connect", l_wifi_connect);
    be_regfunc(vm, "wifi_ip",      l_wifi_ip);
    be_regfunc(vm, "wifi_ssid",    l_wifi_ssid);
    be_regfunc(vm, "wifi_rssi",    l_wifi_rssi);
    be_regfunc(vm, "wifi_stop",    l_wifi_stop);
    be_regfunc(vm, "http_get",     l_http_get);
}

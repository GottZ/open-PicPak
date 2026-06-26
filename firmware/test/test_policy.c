/* Host unit test for main/policy.be against mocked store_* / wifi_* bindings.
 * Compile + run from firmware/ after host/gen_policy.py:
 *   cc -O1 -w -I main -I components/berry/src -I components/berry/generate -I components/berry \
 *      test/test_policy.c components/berry/src/*.c components/berry/port/be_port.c \
 *      components/berry/port/be_modtab.c -o build/test_policy -lm && build/test_policy
 */
#include "policy_script.h"
#include "berry.h"
#include <stdbool.h>
#include <stdio.h>
#include <string.h>

typedef struct { const char *key, *val; } kv_t;
typedef struct { const char *ssid; int rssi; } ap_t;

typedef struct {
    const char *name;
    bool store_ok;
    kv_t store[12]; int nstore;
    ap_t aps[8]; int naps;
    const char *ok_ssid;
    const char *ok_pass;
    const char *want_ssid;
    const char *want_pass;
    int want_connects;
} scenario_t;

static scenario_t *S;
static int fails;
static int connects;
static char last_ssid[40], last_pass[80], net_ok[40], rtc_slot[80];

#define CHECK(c) do { if (!(c)) { printf("  FAIL: %s: %s\n", S->name, #c); fails++; } } while (0)

static const char *getv(const char *key)
{
    for (int i = 0; i < S->nstore; i++) if (strcmp(S->store[i].key, key) == 0) return S->store[i].val;
    return NULL;
}

static int h_store_ok(bvm *vm) { be_pushbool(vm, S->store_ok); be_return(vm); }

static int h_store_keys(bvm *vm)
{
    const char *prefix = (be_top(vm) >= 1 && be_isstring(vm, 1)) ? be_tostring(vm, 1) : "";
    size_t plen = strlen(prefix);
    be_newobject(vm, "list");
    if (S->store_ok) {
        for (int i = 0; i < S->nstore; i++) {
            if (plen && strncmp(S->store[i].key, prefix, plen) != 0) continue;
            be_pushstring(vm, S->store[i].key);
            be_data_push(vm, -2);
            be_pop(vm, 1);
        }
    }
    be_pop(vm, 1);
    be_return(vm);
}

static int h_store_get(bvm *vm)
{
    if (!S->store_ok || be_top(vm) < 1 || !be_isstring(vm, 1)) be_return_nil(vm);
    const char *v = getv(be_tostring(vm, 1));
    if (!v) be_return_nil(vm);
    be_pushbytes(vm, v, strlen(v));
    be_return(vm);
}

static int h_store_set(bvm *vm)
{
    if (be_top(vm) >= 2 && be_isstring(vm, 1) && be_isstring(vm, 2)) {
        const char *k = be_tostring(vm, 1), *v = be_tostring(vm, 2);
        if (strcmp(k, "net.ok") == 0) snprintf(net_ok, sizeof net_ok, "%s", v);
    }
    be_pushbool(vm, true); be_return(vm);
}

static int h_rtc_set(bvm *vm)
{
    if (be_top(vm) >= 2 && be_isstring(vm, 1) && be_isstring(vm, 2) && strcmp(be_tostring(vm, 1), "slot") == 0)
        snprintf(rtc_slot, sizeof rtc_slot, "%s", be_tostring(vm, 2));
    be_pushbool(vm, true); be_return(vm);
}

static void push_ap(bvm *vm, const ap_t *ap)
{
    be_newobject(vm, "map");
    be_pushstring(vm, "ssid"); be_pushstring(vm, ap->ssid); be_data_insert(vm, -3); be_pop(vm, 2);
    be_pushstring(vm, "rssi"); be_pushint(vm, ap->rssi);    be_data_insert(vm, -3); be_pop(vm, 2);
    be_pushstring(vm, "auth"); be_pushint(vm, 3);           be_data_insert(vm, -3); be_pop(vm, 2);
    be_pop(vm, 1);
}

static int h_wifi_scan(bvm *vm)
{
    be_newobject(vm, "list");
    for (int i = 0; i < S->naps; i++) {
        push_ap(vm, &S->aps[i]);
        be_data_push(vm, -2);
        be_pop(vm, 1);
    }
    be_pop(vm, 1);
    be_return(vm);
}

static int h_wifi_connect(bvm *vm)
{
    if (be_top(vm) < 2 || !be_isstring(vm, 1) || !be_isstring(vm, 2)) { be_pushbool(vm, false); be_return(vm); }
    connects++;
    snprintf(last_ssid, sizeof last_ssid, "%s", be_tostring(vm, 1));
    snprintf(last_pass, sizeof last_pass, "%s", be_tostring(vm, 2));
    be_pushbool(vm, S->ok_ssid && strcmp(last_ssid, S->ok_ssid) == 0 && strcmp(last_pass, S->ok_pass) == 0);
    be_return(vm);
}

static int h_nil(bvm *vm) { (void)vm; be_return_nil(vm); }

static void run_one(scenario_t *s)
{
    S = s; connects = 0; last_ssid[0] = last_pass[0] = net_ok[0] = rtc_slot[0] = '\0';
    bvm *vm = be_vm_new();
    be_regfunc(vm, "store_ok", h_store_ok);
    be_regfunc(vm, "store_keys", h_store_keys);
    be_regfunc(vm, "store_get", h_store_get);
    be_regfunc(vm, "store_set", h_store_set);
    be_regfunc(vm, "rtc_set", h_rtc_set);
    be_regfunc(vm, "wifi_scan", h_wifi_scan);
    be_regfunc(vm, "wifi_connect", h_wifi_connect);
    be_regfunc(vm, "wifi_ip", h_nil);
    be_regfunc(vm, "wifi_ssid", h_nil);
    be_regfunc(vm, "wifi_rssi", h_nil);
    be_regfunc(vm, "wifi_stop", h_nil);
    be_regfunc(vm, "http_get", h_nil);
    int r = be_loadstring(vm, POLICY_BE);
    if (r == BE_OK) r = be_pcall(vm, 0);
    if (r != BE_OK) { be_dumpexcept(vm); fails++; }
    CHECK(connects == s->want_connects);
    if (s->want_ssid) CHECK(strcmp(last_ssid, s->want_ssid) == 0);
    if (s->want_pass) CHECK(strcmp(last_pass, s->want_pass) == 0);
    if (s->want_ssid && connects > 0) CHECK(strcmp(net_ok, s->want_ssid) == 0);
    be_vm_delete(vm);
}

int main(void)
{
    scenario_t cases[] = {
        { "only second visible", true,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"home-pass\",\"prio\":0}"},
            {"wifi.office", "{\"ssid\":\"Office\",\"pass\":\"office-pass\",\"prio\":0}"} }, 2,
          { {"Office", -55} }, 1, "Office", "office-pass", "Office", "office-pass", 1 },
        { "unknown visible", true,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"home-pass\",\"prio\":0}"} }, 1,
          { {"Guest", -30} }, 1, NULL, NULL, NULL, NULL, 0 },
        { "rssi tie-break", true,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"home-pass\",\"prio\":5}"},
            {"wifi.office", "{\"ssid\":\"Office\",\"pass\":\"office-pass\",\"prio\":5}"} }, 2,
          { {"Home", -80}, {"Office", -40} }, 2, "Office", "office-pass", "Office", "office-pass", 1 },
        { "fallback after failed candidate", true,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"wrong\",\"prio\":10}"},
            {"wifi.office", "{\"ssid\":\"Office\",\"pass\":\"office-pass\",\"prio\":1}"} }, 2,
          { {"Home", -30}, {"Office", -60} }, 2, "Office", "office-pass", "Office", "office-pass", 2 },
        { "rotated password", true,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"old\",\"prio\":0}"},
            {"wpw.wifi.home", "new"} }, 2,
          { {"Home", -50} }, 1, "Home", "new", "Home", "new", 1 },
        { "store down", false,
          { {"wifi.home", "{\"ssid\":\"Home\",\"pass\":\"home-pass\",\"prio\":0}"} }, 1,
          { {"Home", -50} }, 1, NULL, NULL, NULL, NULL, 0 },
    };
    for (unsigned i = 0; i < sizeof cases / sizeof cases[0]; i++) run_one(&cases[i]);
    if (fails == 0) printf("test_policy: ALL PASS\n");
    else            printf("test_policy: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

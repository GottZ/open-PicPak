/* Device access exposed to Berry — raw values; the Berry script parses/maps them.
 *  dev_mac()/dev_bt_mac()  -> "AA:BB:CC:DD:EE:FF"
 *  nvs_str(ns,key)         -> raw NVS string value, or nil
 *  dev_chip()              -> "ESP32-C3 vX.Y"
 *  dev_uptime_ms()         -> int ; dev_reset() -> reset-reason string
 *  dev_batt_mv()/dev_batt_pct() -> battery voltage/percent (measured ONCE, early)
 * Battery is on ADC1_CH2 = GPIO2 (shared with the button; RE pinmap). Divider + curve
 * are the stock values recovered by disassembly (see below). */
#include "dev.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "esp_mac.h"
#include "esp_chip_info.h"
#include "esp_timer.h"
#include "esp_system.h"
#include "nvs.h"
#include "esp_adc/adc_oneshot.h"
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "driver/usb_serial_jtag.h"   /* dev_usb_connected() -> USB host (SOF) present */
#include "guard.h"                    /* dev_bad_boots()/dev_guard_threshold() */
#include "logbuf.h"                   /* dev_logbuf() -> RTC-RAM log ring (survives panic/reset) */
#include "ota.h"                      /* dev_boots() -> canonical otadiag/boots getter */

/* Stock battery formula recovered from the step6 CFW by disassembly:
 *   pinMv   = adc_cali_raw_to_voltage(raw)    (ADC_ATTEN_DB_12)
 *   vbatMv  = (pinMv * 145 + 50) / 100         (HW divider ~0.69 onto GPIO2 -> x1.45)
 *   percent = piecewise-linear LUT (3200mV=0%, +10%/100mV .. 4000=80%, >4089=100%)
 *   plausible window 2800..4300 mV. (Full stock method also does 5x20 median+valley.) */
#define BATT_NUM 145
#define BATT_DEN 100
#define BATT_PLAUSIBLE_MIN_MV 2800
#define BATT_PLAUSIBLE_MAX_MV 4300

static int s_batt_adc_mv = -1;  /* calibrated at-pin mV */
static int s_batt_mv     = -1;  /* battery mV (pin mV through the stock divider) */
static int s_batt_pct    = -1;

/* stock mV->percent: 3200..4000mV linear 0..80% (10% per 100mV), 4000..4089 -> 80..100%, clamped */
static int batt_pct(int mv)
{
    if (mv <= 3200) return 0;
    if (mv >= 4089) return 100;
    if (mv <= 4000) return (mv - 3200) / 10;
    return 80 + (mv - 4000) * 20 / (4089 - 4000);
}

/* Trimmed mean of N one-shot reads: a single read jitters more than the ~40 mV "charging"
 * delta the low-battery gate needs, so take BATT_SAMPLES reads, sort, drop the BATT_TRIM
 * lowest + highest, and mean the middle. (Stock does the same; ctx 019f08ee.) */
#define BATT_SAMPLES 8
#define BATT_TRIM    2   /* drop this many from each end before averaging */

static int cmp_int(const void *a, const void *b)
{
    int x = *(const int *)a, y = *(const int *)b;
    return (x > y) - (x < y);
}

/* Measure the battery as early as possible, before WiFi/EPD/Berry load the rail down. */
void dev_measure_battery(void)
{
    adc_oneshot_unit_handle_t adc = NULL;
    adc_oneshot_unit_init_cfg_t ucfg = { .unit_id = ADC_UNIT_1 };
    if (adc_oneshot_new_unit(&ucfg, &adc) != ESP_OK) return;
    adc_oneshot_chan_cfg_t ccfg = { .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT };
    adc_oneshot_config_channel(adc, ADC_CHANNEL_2, &ccfg);   /* GPIO2 = ADC1_CH2 */

    adc_cali_handle_t cali = NULL;
    adc_cali_curve_fitting_config_t cc = {
        .unit_id = ADC_UNIT_1, .chan = ADC_CHANNEL_2,
        .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT,
    };
    bool have_cali = (adc_cali_create_scheme_curve_fitting(&cc, &cali) == ESP_OK);

    int samp[BATT_SAMPLES];
    int n = 0;
    for (int i = 0; i < BATT_SAMPLES; i++) {
        int raw = 0;
        if (adc_oneshot_read(adc, ADC_CHANNEL_2, &raw) != ESP_OK) continue;
        int mv = -1;
        if (have_cali) adc_cali_raw_to_voltage(cali, raw, &mv);
        else mv = raw * 3100 / 4095;   /* rough fallback */
        samp[n++] = mv;
    }
    if (have_cali) adc_cali_delete_scheme_curve_fitting(cali);
    adc_oneshot_del_unit(adc);

    if (n == 0) return;   /* keep the previous reading rather than poison it with garbage */

    qsort(samp, n, sizeof samp[0], cmp_int);
    int lo = 0, hi = n;
    if (n > 2 * BATT_TRIM) { lo = BATT_TRIM; hi = n - BATT_TRIM; }   /* else: plain mean */
    long sum = 0;
    for (int i = lo; i < hi; i++) sum += samp[i];
    int pin_mv = (int)(sum / (hi - lo));

    s_batt_adc_mv = pin_mv;                                        /* pin mV (calibrated, trimmed) */
    s_batt_mv = (pin_mv * BATT_NUM + BATT_DEN / 2) / BATT_DEN;     /* stock divider -> battery mV */
    if (s_batt_mv < BATT_PLAUSIBLE_MIN_MV || s_batt_mv > BATT_PLAUSIBLE_MAX_MV)
        s_batt_mv = -1;                                           /* implausible (button held / no battery) */
    s_batt_pct = (s_batt_mv > 0) ? batt_pct(s_batt_mv) : -1;
    printf("[batt] n=%d trimmed pin_mv=%d vbat=%dmV pct=%d%% (stock x1.45 divider)\n",
           n, s_batt_adc_mv, s_batt_mv, s_batt_pct);
}

/* C-callable getters for the early measurement -> the low-battery gate reads the battery
 * without spinning up a Berry VM (Berry has its own l_batt_* bindings). <0 = unset/implausible. */
int dev_batt_mv(void)  { return s_batt_mv; }
int dev_batt_pct(void) { return s_batt_pct; }

static void mac_str(char *out, size_t cap, esp_mac_type_t t)
{
    uint8_t m[6] = {0};
    esp_read_mac(m, t);
    snprintf(out, cap, "%02X:%02X:%02X:%02X:%02X:%02X", m[0], m[1], m[2], m[3], m[4], m[5]);
}

static const char *reset_name(void)
{
    switch (esp_reset_reason()) {
        case ESP_RST_POWERON:   return "poweron";
        case ESP_RST_SW:        return "sw";
        case ESP_RST_PANIC:     return "panic";
        case ESP_RST_INT_WDT:   return "int_wdt";
        case ESP_RST_TASK_WDT:  return "task_wdt";
        case ESP_RST_WDT:       return "wdt";
        case ESP_RST_DEEPSLEEP: return "deepsleep";
        case ESP_RST_BROWNOUT:  return "brownout";
        case ESP_RST_USB:       return "usb";
        default:                return "other";
    }
}

void dev_dump_nvs(void)
{
    printf("[nvs] --- all entries (default partition) ---\n");
    nvs_iterator_t it = NULL;
    esp_err_t e = nvs_entry_find("nvs", NULL, NVS_TYPE_ANY, &it);
    int n = 0;
    while (e == ESP_OK && it) {
        nvs_entry_info_t info;
        nvs_entry_info(it, &info);
        printf("  ns=%-12s key=%-16s type=%d", info.namespace_name, info.key, info.type);
        nvs_handle_t h;
        if (nvs_open(info.namespace_name, NVS_READONLY, &h) == ESP_OK) {
            if (info.type == NVS_TYPE_STR) {
                char buf[320]; size_t len = sizeof buf;
                if (nvs_get_str(h, info.key, buf, &len) == ESP_OK) printf("  str(%u)=\"%s\"", (unsigned)len, buf);
            } else if (info.type == NVS_TYPE_BLOB) {
                uint8_t buf[256]; size_t len = sizeof buf;
                if (nvs_get_blob(h, info.key, buf, &len) == ESP_OK) {
                    printf("  blob(%u)=", (unsigned)len);
                    for (size_t i = 0; i < len && i < 64; i++) printf("%02x", buf[i]);
                    /* also show printable ASCII to spot embedded strings */
                    printf("  |");
                    for (size_t i = 0; i < len && i < 64; i++)
                        printf("%c", (buf[i] >= 32 && buf[i] < 127) ? buf[i] : '.');
                    printf("|");
                }
            }
            nvs_close(h);
        }
        printf("\n");
        n++;
        e = nvs_entry_next(&it);
    }
    if (it) nvs_release_iterator(it);
    printf("[nvs] --- %d entries ---\n", n);
}

static int l_mac(bvm *vm)    { char s[18]; mac_str(s, sizeof s, ESP_MAC_WIFI_STA); be_pushstring(vm, s); be_return(vm); }
static int l_bt_mac(bvm *vm) { char s[18]; mac_str(s, sizeof s, ESP_MAC_BT);       be_pushstring(vm, s); be_return(vm); }

static int l_nvs(bvm *vm)
{
    if (be_top(vm) < 2 || !be_isstring(vm, 1) || !be_isstring(vm, 2)) be_return_nil(vm);
    const char *ns = be_tostring(vm, 1), *key = be_tostring(vm, 2);
    nvs_handle_t h;
    if (nvs_open(ns, NVS_READONLY, &h) == ESP_OK) {
        char buf[320]; size_t len = sizeof buf;
        esp_err_t r = nvs_get_str(h, key, buf, &len);
        nvs_close(h);
        if (r == ESP_OK) { be_pushstring(vm, buf); be_return(vm); }
    }
    be_return_nil(vm);
}

static int l_chip(bvm *vm)
{
    esp_chip_info_t ci; esp_chip_info(&ci);
    char s[24];
    snprintf(s, sizeof s, "ESP32-C3 v%d.%d", ci.revision / 100, ci.revision % 100);
    be_pushstring(vm, s); be_return(vm);
}
static int l_uptime(bvm *vm) { be_pushint(vm, (bint)(esp_timer_get_time() / 1000)); be_return(vm); }
static int l_reset(bvm *vm)  { be_pushstring(vm, reset_name()); be_return(vm); }
static int l_batt_mv(bvm *vm)  { be_pushint(vm, s_batt_mv);  be_return(vm); }
static int l_batt_pct(bvm *vm) { be_pushint(vm, s_batt_pct); be_return(vm); }
/* USB host present? true while a host sends SOF packets (cable to a data host); false on
 * a pure power supply / battery. The forced re-assoc USB re-enum reset only happens with a host. */
static int l_usb_connected(bvm *vm) { be_pushbool(vm, usb_serial_jtag_is_connected()); be_return(vm); }
/* Reset-proof NVS boot counter (otadiag/boots) -> every boot incl. resets (unlike the RTC-RAM
 * boot count, which a USB_UART_CHIP_RESET nulls). Rising bc = re-enum/reset rate. */
static int l_boots(bvm *vm)
{
    be_pushint(vm, (bint)ota_boots()); be_return(vm);
}
static int l_bad_boots(bvm *vm)       { be_pushint(vm, (bint)guard_bad_boots()); be_return(vm); }
static int l_guard_threshold(bvm *vm) { be_pushint(vm, (bint)guard_threshold()); be_return(vm); }
/* RTC-RAM log ring (survives panic/SW-reset), URL/header-safe tail (last ~220 chars). Used to
 * exfiltrate the lines leading up to a crash over the telemetry GET when serial is unavailable. */
static int l_logbuf(bvm *vm)
{
    static char buf[1024];
    size_t n = logbuf_export(buf, sizeof buf);   /* \n->| , control dropped */
    for (size_t i = 0; i < n; i++) {             /* make the rest URL-query-safe */
        char c = buf[i];
        if (c == ' ' || c == '&' || c == '=' || c == '+' || c == '%' || c == '#' || c == '?') buf[i] = '_';
    }
    const char *tail = (n > 220) ? buf + (n - 220) : buf;
    be_pushstring(vm, tail); be_return(vm);
}

void dev_register(bvm *vm)
{
    be_regfunc(vm, "dev_mac",       l_mac);
    be_regfunc(vm, "dev_bt_mac",    l_bt_mac);
    be_regfunc(vm, "nvs_str",       l_nvs);
    be_regfunc(vm, "dev_chip",      l_chip);
    be_regfunc(vm, "dev_uptime_ms", l_uptime);
    be_regfunc(vm, "dev_reset",     l_reset);
    be_regfunc(vm, "dev_batt_mv",   l_batt_mv);
    be_regfunc(vm, "dev_batt_pct",  l_batt_pct);
    be_regfunc(vm, "dev_usb_connected", l_usb_connected);
    be_regfunc(vm, "dev_boots",         l_boots);
    be_regfunc(vm, "dev_bad_boots",     l_bad_boots);
    be_regfunc(vm, "dev_guard_threshold", l_guard_threshold);
    be_regfunc(vm, "dev_logbuf",        l_logbuf);
}

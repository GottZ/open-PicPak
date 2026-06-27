#include "lowbatt.h"
#include "esp_attr.h"   /* RTC_DATA_ATTR */
#include "nvs.h"
#include <stdio.h>

#define NS "picpak"   /* same namespace as config.c -> one device-config store */

/* Cross-wake state in RTC-RAM: survives a deep-sleep wake, nulled on cold boot / the
 * port-open rst:0x15 -> the device therefore always boots NORMAL (lock=0). */
RTC_DATA_ATTR static lowbatt_state_t s_lb;

static int clampi(int v, int lo, int hi) { return v < lo ? lo : (v > hi ? hi : v); }

static uint16_t load_u16(const char *key, uint16_t def, uint16_t lo, uint16_t hi)
{
    uint32_t v = def;
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READONLY, &h) == ESP_OK) {
        uint16_t x;
        if (nvs_get_u16(h, key, &x) == ESP_OK) v = x;
        nvs_close(h);
    }
    if (v < lo) v = lo;
    if (v > hi) v = hi;
    return (uint16_t)v;
}

static void store_u16(const char *key, uint16_t val)
{
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u16(h, key, val);
        nvs_commit(h);
        nvs_close(h);
    }
}

static lowbatt_cfg_t cfg_load(void)
{
    lowbatt_cfg_t c;
    c.arm_mv = load_u16("lb_arm_mv", LOWBATT_ARM_MV_DEF, LOWBATT_ARM_MV_MIN, LOWBATT_ARM_MV_MAX);
    /* CLEAR floored to ARM+100 (one LUT step) so the hysteresis can never invert, whatever
     * the stored values. */
    uint16_t clr_lo = (uint16_t)(c.arm_mv + 100);
    c.clr_mv = load_u16("lb_clr_mv", LOWBATT_CLR_MV_DEF, clr_lo, LOWBATT_CLR_MV_MAX);
    c.rise_mv = load_u16("lb_rise_mv", LOWBATT_RISE_MV_DEF, LOWBATT_RISE_MV_MIN, LOWBATT_RISE_MV_MAX);
    c.arm_streak = (uint8_t)load_u16("lb_streak", LOWBATT_STREAK_DEF, LOWBATT_STREAK_MIN, LOWBATT_STREAK_MAX);
    return c;
}

bool lowbatt_is_enabled(void)
{
#if LOWBATT_GATE_ENABLED
    return true;                       /* compile-time force-on */
#else
    nvs_handle_t h; uint8_t v = 0;     /* per-device runtime flag, default off */
    if (nvs_open(NS, NVS_READONLY, &h) == ESP_OK) {
        if (nvs_get_u8(h, "lb_on", &v) != ESP_OK) v = 0;
        nvs_close(h);
    }
    return v != 0;
#endif
}

bool lowbatt_is_locked(void) { return s_lb.lock; }

uint32_t lowbatt_wake_s(void)
{
    return load_u16("lb_wake_s", LOWBATT_WAKE_S_DEF, LOWBATT_WAKE_S_MIN, LOWBATT_WAKE_S_MAX);
}

lowbatt_action_t lowbatt_gate(int batt_mv, esp_sleep_wakeup_cause_t cause)
{
    bool button_wake = (cause == ESP_SLEEP_WAKEUP_GPIO);
    lowbatt_cfg_t cfg = cfg_load();
    lowbatt_result_t r = lowbatt_decide(batt_mv, button_wake, lowbatt_is_enabled(), s_lb, cfg);
    s_lb = r.next;   /* persist for the next wake (RTC-RAM) */
    return r.action;
}

void lowbatt_status(void)
{
    lowbatt_cfg_t c = cfg_load();
    printf("BATT gate: %s\r\n", lowbatt_is_enabled() ? "ON" : "off");
    printf("  arm=%umV clear=%umV rise=%umV streak=%u wake=%lus\r\n",
           c.arm_mv, c.clr_mv, c.rise_mv, c.arm_streak, (unsigned long)lowbatt_wake_s());
    printf("  state: lock=%d last_mv=%d low_streak=%u\r\n",
           s_lb.lock, s_lb.last_mv, s_lb.low_streak);
}

void lowbatt_set_arm(int mv)    { store_u16("lb_arm_mv",  (uint16_t)clampi(mv, LOWBATT_ARM_MV_MIN, LOWBATT_ARM_MV_MAX)); }
void lowbatt_set_clear(int mv)  { store_u16("lb_clr_mv",  (uint16_t)clampi(mv, LOWBATT_ARM_MV_MIN + 100, LOWBATT_CLR_MV_MAX)); }
void lowbatt_set_rise(int mv)   { store_u16("lb_rise_mv", (uint16_t)clampi(mv, LOWBATT_RISE_MV_MIN, LOWBATT_RISE_MV_MAX)); }
void lowbatt_set_wake(int s)    { store_u16("lb_wake_s",  (uint16_t)clampi(s,  LOWBATT_WAKE_S_MIN, LOWBATT_WAKE_S_MAX)); }
void lowbatt_set_streak(int n)  { store_u16("lb_streak",  (uint16_t)clampi(n,  LOWBATT_STREAK_MIN, LOWBATT_STREAK_MAX)); }

void lowbatt_set_enabled(bool on)
{
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u8(h, "lb_on", on ? 1 : 0);
        nvs_commit(h);
        nvs_close(h);
    }
}

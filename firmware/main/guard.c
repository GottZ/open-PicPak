#include "guard.h"
#include "guard_core.h"
#include "led.h"
#include "console.h"
#include <string.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_app_desc.h"
#include "nvs.h"

static const char *TAG = "guard";
#define GUARD_NS "guard"
#define GUARD_DEFAULT_STABLE_MS 20000   /* default uninterrupted uptime that counts as "stable" */

static uint8_t nvs_get_bad(void)
{
    nvs_handle_t h; uint8_t v = 0;
    if (nvs_open(GUARD_NS, NVS_READONLY, &h) == ESP_OK) {
        nvs_get_u8(h, "bad_boots", &v);
        nvs_close(h);
    }
    return v;
}

static void nvs_set_bad(uint8_t v)
{
    nvs_handle_t h;
    if (nvs_open(GUARD_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u8(h, "bad_boots", v);
        nvs_commit(h);
        nvs_close(h);
    }
}

/* Settings (threshold, stable-uptime) — compile-time defaults, overridable at runtime via
 * NVS (set from the console: GUARD THRESHOLD / GUARD UPTIME). */
static uint32_t nvs_get_u32_def(const char *key, uint32_t def)
{
    nvs_handle_t h; uint32_t v = def;
    if (nvs_open(GUARD_NS, NVS_READONLY, &h) == ESP_OK) {
        nvs_get_u32(h, key, &v);   /* leaves v=def if the key is absent */
        nvs_close(h);
    }
    return v;
}

static void nvs_set_u32_key(const char *key, uint32_t v)
{
    nvs_handle_t h;
    if (nvs_open(GUARD_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u32(h, key, v);
        nvs_commit(h);
        nvs_close(h);
    }
}

unsigned guard_threshold(void)
{
    uint32_t v = nvs_get_u32_def("threshold", GUARD_DEFAULT_THRESHOLD);
    if (v > 254u) v = 254u;   /* keep < 255 so the saturated counter can still exceed it */
    return (unsigned)v;
}
unsigned guard_stable_ms(void)       { return (unsigned)nvs_get_u32_def("stable_ms", GUARD_DEFAULT_STABLE_MS); }
void     guard_set_threshold(unsigned v) { nvs_set_u32_key("threshold", v); }
void     guard_set_stable_ms(unsigned v) { nvs_set_u32_key("stable_ms", v); }

/* Returns true if the running app differs from the one last seen (fresh flash / OTA),
 * and stores the current app's elf-sha for next boot. Uses the first 16 hex chars of the
 * elf-sha256 -> unique per build. */
static bool fresh_app_check_and_store(void)
{
    char cur[17] = {0};
    esp_app_get_elf_sha256(cur, sizeof(cur));   /* hex, NUL-terminated */

    nvs_handle_t h; bool fresh = true;
    if (nvs_open(GUARD_NS, NVS_READWRITE, &h) == ESP_OK) {
        char stored[17] = {0}; size_t sl = sizeof(stored);
        if (nvs_get_str(h, "app_sha", stored, &sl) == ESP_OK)
            fresh = (strncmp(stored, cur, sizeof(cur)) != 0);
        if (fresh) { nvs_set_str(h, "app_sha", cur); nvs_commit(h); }
        nvs_close(h);
    }
    return fresh;
}

static void stable_task(void *arg)
{
    (void)arg;
    unsigned ms = guard_stable_ms();
    vTaskDelay(pdMS_TO_TICKS(ms));
    nvs_set_bad(0);
    ESP_LOGI(TAG, "stable uptime (%u ms) reached -> bad_boots cleared", ms);
    vTaskDelete(NULL);
}

/* USB-reachable safe mode. No WiFi, no EPD, never sleeps. Loops the setup console so the
 * device stays addressable (GUARD CLEAR / SETWIFI / OTA / esptool). Never returns. */
static void safe_mode(uint8_t count)
{
    ESP_LOGE(TAG, "==== SAFE MODE ==== bad_boots=%u > %u. WiFi/EPD OFF, USB stays up. "
                  "Reflash via esptool, or 'GUARD CLEAR' in the console to resume.",
             count, guard_threshold());
    led_blink_start();   /* visible signal: device is in safe mode */
    for (;;) {
        uint32_t ignore = 0;
        console_run(0, &ignore, true, true);   /* force_open + forced_setup: safe mode stays
                                                * strictly disconnect-only -- no command-ready
                                                * self-exit, no banner churn (design 03 §5 B2) */
        /* console_run can return CONSOLE_PROCEED on timeout -> just stay in safe mode. */
        vTaskDelay(pdMS_TO_TICKS(50));
    }
}

void guard_check_and_run(void)
{
    bool fresh = fresh_app_check_and_store();
    uint8_t prev = nvs_get_bad();
    uint8_t thr  = (uint8_t)guard_threshold();
    guard_decision_t d = guard_on_boot(prev, fresh, thr);
    nvs_set_bad(d.new_count);
    ESP_LOGI(TAG, "boot guard: prev=%u fresh=%d -> bad_boots=%u safe=%d (threshold %u)",
             prev, fresh, d.new_count, d.safe_mode, thr);
    if (d.safe_mode)
        safe_mode(d.new_count);          /* never returns */
    /* 8192: the NVS commit in nvs_set_bad() overflows a 3072-byte stack on a full or
     * fragmented NVS -> stack-protection panic in this task (~stable_ms after boot). */
    xTaskCreate(stable_task, "guard_stable", 8192, NULL, 4, NULL);
}

void guard_mark_stable(void) { nvs_set_bad(0); }
unsigned guard_bad_boots(void) { return nvs_get_bad(); }
void guard_clear(void) { nvs_set_bad(0); }

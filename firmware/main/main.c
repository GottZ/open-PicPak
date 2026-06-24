/*
 * PicPak custom firmware — autonomous cycle + button wake + USB keep-awake.
 * Wake (timer OR button) -> WiFi -> HTTP pull -> display -> deep sleep.
 * Creds (SSID/pass/URL) live in NVS, provisioned via the serial console
 * (SETWIFI/SETURL). Sleep duration: default 1 h, overridden by the HTTP header
 * X-Next-Wake-Seconds (server/HA controls the frequency).
 *
 * Button (GPIO2, active-low): an additional deep-sleep wakeup source for an
 * immediate refresh. USB keep-awake: as long as a USB host sends SOF packets
 * (development/flash on the tether), the device does NOT go into deep sleep, but
 * stays awake and refreshes periodically -> reachable/flashable anytime, no
 * deep-sleep USB reconnect chaos. Without a USB host (battery/field) normal sleep.
 */
#include <stdio.h>
#include <string.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_log.h"
#include "esp_sleep.h"
#include "esp_attr.h"
#include "esp_timer.h"
#include "esp_system.h"
#include "nvs_flash.h"
#include "driver/gpio.h"
#include "driver/usb_serial_jtag.h"
#include "hal/usb_serial_jtag_ll.h"
#include "epd.h"
#include "net.h"
#include "config.h"
#include "console.h"
#include "led.h"
#include "logbuf.h"
#include "ota.h"
#include "guard.h"
#include "store.h"      /* littlefs "fs" persistence mount (Berry key-value backing store) */
#include "netberry.h"   /* Berry net surface (wifi_ functions + http_get) for the policy phase */
#include "screens.h"   /* baked-in 400x300 BWRY setup screens (onboarding / console) */
#include "berry.h"
#include "fb.h"         /* Berry graphics stdlib -> the 30000-byte framebuffer */
#include "dev.h"        /* Berry device stdlib: mac/serial/chip/uptime/battery + nvs_str */
#include "render_script.h"  /* RENDER_BE: embedded Berry render script (generated from render.be) */
#include "policy_script.h"  /* POLICY_BE: embedded Berry net-phase script (generated from policy.be) */

#define DEFAULT_WAKE_S 3600   /* 1 h, if the server delivers no header */
#define RETRY_WAKE_S   300    /* on WiFi/download error or missing config */
#define BTN_GPIO       2      /* button, active-low (RE-verified, wakeup-capable) */
#define KEEPALIVE_POLL_MS 100  /* USB/button poll interval in keep-awake mode */
#define ALWAYS_AWAKE 0         /* field build: deep-sleep between cycles. Set to 1 for a
                                  bench/test build that never sleeps (USB stays reachable;
                                  keep-awake is SOF-dependent and proved unreliable). */
#define SETTLE_MS      5000    /* quiet phase before/after the EPD refresh: with WiFi
                                  off the supply recovers between the WiFi peak and the
                                  EPD charge-pump peak (brownout decoupling) */

static const char *TAG = "picpak";

/* RTC-RAM survives deep sleep -> proves real timer wakes. */
RTC_DATA_ATTR static uint32_t s_boot_count;

/* Phase times (ms) of the last successful cycle. RTC-RAM survives the deep-sleep
 * wake (timer/button), but NOT the port-open rst:0x15 (which nulls RTC-RAM ->
 * boot_count stays 1; measured 2026-06-21). On the tether the values are therefore
 * output only on the 2nd cycle without a reset in between (keep-awake, or a
 * deep-sleep wake sequence) via ?tm= -> a single PRESS 1 never yields a ?tm=,
 * PRESS 2+ does. Full log push see X-Picpak-Log in net.c. */
RTC_DATA_ATTR static int32_t s_tm_scan, s_tm_dhcp, s_tm_fetch, s_tm_epd;
RTC_DATA_ATTR static uint32_t s_tm_cycle;   /* boot# of the captured timing, 0 = none yet */

/* Button GPIO2 as input with pull-up (idle HIGH, press = LOW). Configured once at
 * boot -> serves both the live poll in keep-awake mode and the deep-sleep GPIO
 * wakeup. */
static void configure_button(void)
{
    gpio_config_t btn = {
        .pin_bit_mask = 1ULL << BTN_GPIO,
        .mode = GPIO_MODE_INPUT,
        .pull_up_en = GPIO_PULLUP_ENABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    gpio_config(&btn);
}

static inline bool button_pressed(void)
{
    return gpio_get_level(BTN_GPIO) == 0;   /* active-low */
}

/* Triple-press detection via GPIO ISR -> works in ANY phase (console, run_cycle,
 * keep-awake), unlike the keep-awake-only poll. Three presses within 1.5s open the
 * setup console. The ISR only counts; a low-priority watcher does the NVS write +
 * restart (neither is ISR-safe). */
/* Triple-press detection by POLLING GPIO2 in a dedicated task. An edge ISR on this pin
 * was unreliable: USB-reset glitches (rst:0x15 on every host port-open) fired phantom
 * presses, while real presses were missed. Polling reads the *stable* level every 30ms,
 * so sub-30ms glitches are ignored and a real ~100ms press is caught reliably (same
 * mechanism as the working single-press refresh). 3 presses within 1.5s toggle the setup
 * console (force_setup) and reboot. Runs in every phase (own task), stack generous for
 * the NVS write in cfg_set_force_setup(). */
static void button_task(void *arg)
{
    int count = 0;
    int64_t first_us = 0, last_edge_us = 0;
    bool prev = button_pressed();   /* seed with the current level so a button held at
                                       boot is NOT counted as a press (avoids a toggle loop) */
    for (;;) {
        bool pressed = button_pressed();        /* gpio_get_level == 0, active-low */
        if (pressed && !prev) {                 /* falling edge = press */
            int64_t t = esp_timer_get_time();
            if (t - last_edge_us > 150000) {    /* debounce: ignore edges <150ms apart (contact bounce) */
                last_edge_us = t;
                if (count == 0 || t - first_us > 1500000) { count = 1; first_us = t; }
                else count++;
                if (count >= 3) {
                    count = 0;
                    /* Toggle: in setup mode -> leave (normal run); otherwise -> enter.
                     * force_setup stays set in NVS while the console is up (main clears it
                     * only after console_run returns), so reading it gives the current mode. */
                    bool leaving = cfg_force_setup();
                    ESP_LOGI(TAG, "triple-press -> %s setup mode", leaving ? "leave" : "enter");
                    cfg_set_force_setup(!leaving);  /* survives the browser's port-open reset */
                    vTaskDelay(pdMS_TO_TICKS(60));
                    esp_restart();                  /* boot: setup screen+console, or normal run */
                }
            }
        }
        prev = pressed;
        vTaskDelay(pdMS_TO_TICKS(30));
    }
}

/* Configure the button input + start the polling task. Called once, early in app_main,
 * so the triple-press works during every phase. No interrupt -> glitch-resistant. */
static void button_init(void)
{
    configure_button();   /* GPIO2 input + pull-up, no interrupt */
    xTaskCreate(button_task, "btn", 8192, NULL, 5, NULL);
}

static void enter_deep_sleep(uint32_t seconds)
{
    guard_mark_stable();   /* ordered sleep = FW healthy -> clear the bootloop counter */
#if ALWAYS_AWAKE
    /* Test build: never deep-sleep. keep-awake (SOF-based) did NOT keep the device awake
     * on the bench -> it slept on WiFi-fail and USB vanished. Stay awake so esptool/console
     * remain reachable; a button press reboots (= manual refresh). */
    ESP_LOGW(TAG, "ALWAYS_AWAKE: not sleeping (%lus requested) -> USB stays reachable; button=reboot",
             (unsigned long)seconds);
    led_on();
    configure_button();
    for (;;) {
        vTaskDelay(pdMS_TO_TICKS(200));
        if (button_pressed()) { ESP_LOGI(TAG, "button -> reboot/refresh"); esp_restart(); }
    }
#endif
    led_off();   /* deep sleep -> LED off */
    esp_sleep_enable_timer_wakeup((uint64_t)seconds * 1000000ULL);

    /* Button (GPIO2, active-low) as a second wakeup source. The ESP32-C3 has no
     * RTC-IO domain -> digital GPIO wakeup. Configure the pin only HERE (after the
     * WiFi phase) -> GPIO2 is never set during the USB-sensitive WiFi TX. */
    configure_button();
    esp_deep_sleep_enable_gpio_wakeup(1ULL << BTN_GPIO, ESP_GPIO_WAKEUP_GPIO_LOW);
    ESP_LOGI(TAG, "=> deep sleep %lus (wake: timer OR button GPIO%d)",
             (unsigned long)seconds, BTN_GPIO);
    vTaskDelay(pdMS_TO_TICKS(80));   /* let log/serial flush out */
    esp_deep_sleep_start();          /* never returns; wake = restart from app_main */
}

/* Render a baked-in static frame (onboarding / setup-console screen) on the EPD and
 * put the panel back to sleep. ~22s full refresh; SPI only, independent of WiFi/USB. */
static void show_static_frame(const uint8_t *frame)
{
    led_on();
    epd_init();
    epd_write_full(frame);
    epd_refresh();
    epd_sleep();
}

/* Render the current frame ON-DEVICE via the embedded Berry script (render.be ->
 * render_script.h). The script draws with the fb/dev stdlib; the result is the
 * 30000-byte EPD framebuffer fb_buffer(). Returns true on a clean run. */
static bool berry_render(void)
{
    bvm *vm = be_vm_new();
    fb_register(vm);
    dev_register(vm);
    store_register(vm);   /* store_set/get/del/has/keys/ok — Berry persistence (flash) */
    rtc_register(vm);     /* rtc_set/get/del/has — ephemeral cross-wake state (RTC RAM) */
    int r = be_loadstring(vm, RENDER_BE);
    if (r == BE_OK) r = be_pcall(vm, 0);
    if (r != BE_OK) { ESP_LOGE(TAG, "Berry render failed (res=%d)", r); be_dumpexcept(vm); }
    be_vm_delete(vm);
    return r == BE_OK;
}

/* Net-phase Berry: runs with WiFi UP (after the connect + OTA self-verify, before net_wifi_stop).
 * Registers the net + store + rtc + dev surface, NOT fb (no drawing here -- the render phase,
 * which runs after net_wifi_stop with RF off, owns the framebuffer). Best-effort: a script
 * failure only logs, never blocks the cycle. POLICY_BE starts as a benign observe/record script;
 * the multi-WLAN / rotation policy replaces it later. */
static void berry_policy(void)
{
    bvm *vm = be_vm_new();
    net_register(vm);
    store_register(vm);
    rtc_register(vm);
    dev_register(vm);
    int r = be_loadstring(vm, POLICY_BE);
    if (r == BE_OK) r = be_pcall(vm, 0);
    if (r != BE_OK) { ESP_LOGE(TAG, "Berry policy failed (res=%d)", r); be_dumpexcept(vm); }
    be_vm_delete(vm);
}

/* Best-effort WiFi (OTA-validate now; script/image pull in a later wave) -> render the
 * frame ON-DEVICE via Berry -> display. The device is autonomous: it renders even with
 * no network. Returns the next sleep duration. */
static uint32_t run_cycle_inner(const picpak_cfg_t *cfg)
{
    led_blink_start();   /* transfer phase -> blink */
    if (net_wifi_connect(cfg->ssid, cfg->pass, 20000)) {
        /* A freshly OTA'd app proves itself healthy once it has connectivity -> cancel
         * rollback before the next deep-sleep wake would otherwise roll it back. No-op
         * if not running in PENDING_VERIFY. */
        ota_mark_valid_if_pending();
        cfg_set_verified(true);
        /* Net-phase Berry while WiFi is still up (after OTA self-verify, before the RF goes
         * off). This is the only window with an active link AND the OTA health path done. */
        berry_policy();
        /* RF off BEFORE the EPD charge-pump peak (brownout decoupling), then settle so
         * the supply recovers between the WiFi peak and the EPD peak. */
        net_wifi_stop();
        vTaskDelay(pdMS_TO_TICKS(SETTLE_MS));
    } else {
        ESP_LOGW(TAG, "WiFi unavailable -> rendering offline (autonomous)");
    }

    led_on();   /* transfer done -> solid during render + EPD refresh */
    if (!berry_render())
        ESP_LOGW(TAG, "render failed -> displaying current framebuffer contents");

    int64_t t_epd0 = esp_timer_get_time();
    epd_init();
    epd_write_full(fb_buffer());
    epd_refresh();
    epd_sleep();
    ESP_LOGI(TAG, "TIMING epd=%lldms (display done)", (esp_timer_get_time() - t_epd0) / 1000);

    vTaskDelay(pdMS_TO_TICKS(SETTLE_MS));   /* let the EPD peaks decay before sleep */
    /* TODO(config-pull): the next wake interval should come from the Berry config script
     * (fetched per wake) / server, replacing this fixed default. */
    return DEFAULT_WAKE_S;
}

/* Keep the USB pad ON during run_cycle (1). The pad-detach was the OLD work-around for
 * the rst:0x15 (USB_UART_CHIP_RESET) brownout on WiFi TX -- now solved properly by the
 * adaptive TX power (start 11 dBm, see net.c). Detaching is therefore obsolete AND
 * harmful on the tether: after the reattach the host fails to re-enumerate (error -71),
 * usb_serial_jtag_is_connected() reads false, and the device wrongly drops to deep sleep
 * (no keep-awake, button/console dead, /dev/ttyACM* gone). So: leave the pad attached. */
#define TETHER_DEBUG_KEEP_USB 1

/* Wrapper: with the pad kept on (above) this is a passthrough. In the field (battery,
 * no host) the pad enable/disable would be a no-op anyway. */
static uint32_t run_cycle(const picpak_cfg_t *cfg)
{
#if !TETHER_DEBUG_KEEP_USB
    usb_serial_jtag_ll_phy_enable_pad(false);   /* detach USB from the bus */
#endif
    uint32_t nw = run_cycle_inner(cfg);
#if !TETHER_DEBUG_KEEP_USB
    usb_serial_jtag_ll_phy_enable_pad(true);    /* reconnect */
    /* Give the host time to re-enumerate: otherwise the keep-awake check
     * (usb_serial_jtag_is_connected) right afterwards sees 'false' too early -> the
     * device falsely goes into deep sleep and the USB device disappears on the tether. */
    for (int i = 0; i < 100 && !usb_serial_jtag_is_connected(); i++)
        vTaskDelay(pdMS_TO_TICKS(20));   /* wait up to ~2 s for re-enumeration */
#endif
    return nw;
}

/* As long as a USB host is connected (SOF packets): do NOT sleep. Instead stay awake
 * and either refresh after wake_s seconds OR on a button press (GPIO2) immediately.
 * USB presence + button are polled every KEEPALIVE_POLL_MS. Returns with the last
 * valid sleep duration as soon as USB is gone -> the caller then enters deep sleep
 * normally.
 * Note: is_connected() works over a FreeRTOS tick hook (SOF monitor), NOT over the
 * USB-Serial-JTAG driver -> no VBUS glitch during WiFi TX. */
static uint32_t run_keep_awake(const picpak_cfg_t *cfg, uint32_t wake_s)
{
    ESP_LOGI(TAG, "USB host detected -> keep-awake active (no deep sleep). "
                  "Refresh interval %lus; button GPIO%d triggers immediately.",
             (unsigned long)wake_s, BTN_GPIO);
    led_on();   /* keep-awake = awake -> LED solid on */
    /* button is already configured with its ISR in button_isr_init() (early in app_main);
     * do NOT re-run configure_button() here — it would disable the interrupt. */
    while (usb_serial_jtag_is_connected()) {
        bool by_button = false;
        for (uint32_t waited = 0; waited < wake_s * 1000UL; waited += KEEPALIVE_POLL_MS) {
            vTaskDelay(pdMS_TO_TICKS(KEEPALIVE_POLL_MS));
            if (!usb_serial_jtag_is_connected()) {
                ESP_LOGI(TAG, "USB disconnected -> switching to deep sleep (%lus)",
                         (unsigned long)wake_s);
                return wake_s;
            }
            if (button_pressed()) { by_button = true; break; }
        }
        if (by_button) {
            /* Single press = immediate refresh. A TRIPLE press is handled
             * asynchronously by the ISR watcher (force_setup + reboot), which fires
             * during this short wait if it was a triple -> then we never reach run_cycle. */
            ESP_LOGI(TAG, "button pressed -> immediate refresh (triple = setup, handled by ISR)");
            vTaskDelay(pdMS_TO_TICKS(800));                          /* let a possible triple complete */
            while (button_pressed()) vTaskDelay(pdMS_TO_TICKS(20));  /* debounce release */
        } else {
            ESP_LOGI(TAG, "keep-awake: periodic refresh");
        }
        wake_s = run_cycle(cfg);
        led_on();   /* awake: stop blinking after run_cycle (also on failure) */
    }
    return wake_s;
}

void app_main(void)
{
    /* Early: mirror ESP_LOG additionally into the RTC-RAM ring, so that the lines
     * arising in the USB-blind run_cycle window are retrievable later via the LOG
     * command. The ring itself survives resets/wakes (RTC-RAM). */
    logbuf_init();
    s_boot_count++;
    /* FIRST after wake: sample the battery before WiFi/EPD/Berry load the rail
     * (ADC1_CH2/GPIO2, before button_init configures GPIO2 with a pull-up). */
    dev_measure_battery();
    esp_sleep_wakeup_cause_t cause = esp_sleep_get_wakeup_cause();
    ESP_LOGI(TAG, "==== PicPak FW boot #%lu (wakeup_cause=%d; 4=TIMER, 7=GPIO/Button) ====",
             (unsigned long)s_boot_count, (int)cause);
    /* Reset reason of the PREVIOUS run: reveals a crash/brownout/WDT during the OTA
     * download (3=SW/esp_restart is normal; 4=PANIC, 5/6/7=WDT, 9=BROWNOUT would be
     * the problem). This line arises at the reboot itself -> it is exfiltrated on the
     * next frame.bin via X-Picpak-Log. */
    ESP_LOGI(TAG, "reset_reason=%d (1=POWERON 2=EXT 3=SW 4=PANIC 5=INT_WDT 6=TASK_WDT 7=WDT 9=BROWNOUT 11=USB)",
             (int)esp_reset_reason());
    if (s_tm_cycle) {
        ESP_LOGI(TAG, "TIMING (cycle #%lu): scan+auth=%ldms dhcp=%ldms fetch=%ldms epd=%ldms",
                 (unsigned long)s_tm_cycle, (long)s_tm_scan, (long)s_tm_dhcp,
                 (long)s_tm_fetch, (long)s_tm_epd);
    }

    esp_err_t nv = nvs_flash_init();
    if (nv == ESP_ERR_NVS_NO_FREE_PAGES || nv == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        nvs_flash_erase();
        nvs_flash_init();
    }
    ota_record_boot();   /* reset-proof diagnostics (NVS): boot counter + reset reason (after nvs_init) */
    guard_check_and_run();   /* bootloop guard: enters USB-only safe mode if looping (never returns then) */

    /* Mount the littlefs "fs" store STRICTLY AFTER the guard (so a FS defect can never mask
     * a bootloop) and never aborting: a missing/corrupt partition just degrades to
     * store_fs_ok()==false and the device runs autonomously. */
    store_mount();

    led_init();
    led_on();   /* awake: LED on as soon as the device runs */
    button_init();   /* triple-press -> toggle setup console, polled in any phase */

    /* Console only on a "real" boot (power-on/reset/USB connect), NEVER on a timer or
     * button wake: in the field (battery) it would only cost power and time; a button
     * press should refresh immediately, not enter setup.
     * EXCEPTION: an app freshly booted via OTA (PENDING_VERIFY, cause=SW) must SKIP the
     * console and run run_cycle directly -> connect quickly + mark_valid. Otherwise the
     * console USB driver churns the path and mark_valid is not reached before the next
     * (timer-wake) reboot -> auto-rollback. */
    bool ota_pending = ota_is_pending();
    bool fresh_boot = (cause != ESP_SLEEP_WAKEUP_TIMER && cause != ESP_SLEEP_WAKEUP_GPIO)
                   && !ota_pending;
    if (ota_pending)
        ESP_LOGI(TAG, "OTA boot (PENDING_VERIFY) -> console skipped, run_cycle directly");
    bool skip_fetch = false;
    uint32_t console_arg = 0;
    console_action_t cact = CONSOLE_PROCEED;
    bool forced = cfg_force_setup();   /* set by a triple-press on a previous run */
    if (fresh_boot) {
        picpak_cfg_t probe;
        bool have = cfg_load(&probe);
        /* Render the setup screen UP FRONT so it's visible immediately while the console
         * waits to be provisioned. forced -> console screen; creds not yet proven (incl.
         * no config) -> onboarding screen. ~22s EPD; the console's USB driver is installed
         * right after (console_run) and buffers any provisioning bytes that arrive. */
        if (forced)                  show_static_frame(screen_console);
        else if (!cfg_is_verified()) show_static_frame(screen_onboarding);

        /* Keep the console open (long window) on a triple-press request OR when there
         * are no usable creds yet; otherwise just a brief 3s peek before a normal run. */
        cact = console_run(s_boot_count, &console_arg, forced || !have);
        if (cact == CONSOLE_SLEEP) skip_fetch = true;
    }
    if (forced) cfg_set_force_setup(false);   /* triple-press request consumed */

    picpak_cfg_t cfg;
    if (!cfg_load(&cfg)) {
        /* Autonomous: render even without creds. cfg_load zeroed the struct -> the
         * best-effort WiFi connect below just fails and we render offline. Provision via
         * USB (SETWIFI/SETURL) or the web tool to enable OTA / future script+image pull. */
        ESP_LOGW(TAG, "no NVS config -> rendering autonomously (offline); provision for OTA/pull");
    }

    uint32_t next_wake;
    if (cact == CONSOLE_PRESS) {
        uint32_t n = console_arg ? console_arg : 1;
        ESP_LOGI(TAG, "PRESS test: %lu simulated button-press cycles", (unsigned long)n);
        next_wake = DEFAULT_WAKE_S;
        for (uint32_t i = 0; i < n; i++) {
            next_wake = run_cycle(&cfg);
            if (i + 1 < n) vTaskDelay(pdMS_TO_TICKS(3000));   /* pause between cycles */
        }
    } else if (skip_fetch) {
        next_wake = console_arg ? console_arg : DEFAULT_WAKE_S;
        ESP_LOGI(TAG, "SLEEP requested -> sleeping %lus without fetch", (unsigned long)next_wake);
    } else {
        next_wake = run_cycle(&cfg);
    }

    /* USB keep-awake: as long as a host is attached, do not sleep. A SLEEP explicitly
     * requested via the console (skip_fetch) deliberately keeps priority -> this way
     * the deep-sleep path can be tested on the tether too. */
    if (!skip_fetch && usb_serial_jtag_is_connected()) {
        next_wake = run_keep_awake(&cfg, next_wake);
    }

    enter_deep_sleep(next_wake);
}

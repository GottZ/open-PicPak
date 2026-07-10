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
#include "wifi_store.h" /* multi-WiFi store migration + presence check */
#include "netberry.h"   /* Berry net surface (wifi_ functions + http_get) for the policy phase */
#include "screens.h"   /* baked-in 400x300 BWRY setup screens (onboarding / console) */
#include "berry.h"
#include "fb.h"         /* Berry graphics stdlib -> the 30000-byte framebuffer */
#include "present_core.h"   /* A32 W2: present-gate decision (present only on a valid frame) */
#include "dev.h"        /* Berry device stdlib: mac/serial/chip/uptime/battery + nvs_str */
#include "lowbatt.h"    /* smart low-battery gate (timer-poll + voltage-rise charge detect) */
#include "cmd.h"        /* C2 Berry command executor + poll (Wave 3a/3b) */
#include "render_script.h"  /* RENDER_BE: embedded Berry render script (generated from render.be) */
#include "policy_script.h"  /* POLICY_BE: embedded Berry net-phase script (generated from policy.be) */

/* DEFAULT_WAKE_S (battery wake fallback) now lives in config.h (shared with cfg_wake_s / console). */
#define RETRY_WAKE_S   300    /* on WiFi/download error or missing config */
#define BTN_GPIO       2      /* button, active-low (RE-verified, wakeup-capable) */
#define KEEPALIVE_POLL_MS 100  /* USB/button poll interval in keep-awake mode */
#define C2_LONGPOLL_S     25   /* keep-awake C2 long-poll hold budget (matches the backend cap); the
                                * loop checks USB/button after each poll return -> <=this much latency */
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
/* Wave 2 -- EPD content-change gate: hash of the last DISPLAYED framebuffer. Survives a
 * deep-sleep wake (RTC-RAM); nulled on a cold boot / the port-open rst:0x15 -> _valid is
 * then false and the first cycle refreshes unconditionally (we cannot assume panel state). */
RTC_DATA_ATTR static uint32_t s_fb_hash;
RTC_DATA_ATTR static bool     s_fb_hash_valid;

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

/* Net-phase Berry: owns the multi-WiFi scan-match-connect policy before RF is stopped.
 * Registers the net + store + rtc + dev surface, NOT fb (no drawing here -- the render phase,
 * which runs after net_wifi_stop with RF off, owns the framebuffer). Best-effort: a script
 * failure only logs and leaves the cycle to render offline. */
static bool berry_policy(void)
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
    char ip[20];
    return r == BE_OK && net_ip(ip, sizeof ip);
}

/* FNV-1a over the framebuffer -> a cheap content fingerprint for the Wave-2 EPD gate.
 * 32-bit is plenty: a collision only ever risks one skipped refresh, self-correcting on
 * the next real change. Self-contained (no extra include). */
static uint32_t fb_hash(const uint8_t *p, size_t n)
{
    uint32_t h = 2166136261u;
    for (size_t i = 0; i < n; i++) { h ^= p[i]; h *= 16777619u; }
    return h;
}

/* Wave-2 EPD content-change gate: refresh the panel only when the rendered framebuffer changed
 * vs. the last DISPLAYED one (hash in RTC-RAM). Unchanged content skips the ~22 s refresh AND its
 * charge-pump brownout window. First cycle after a cold boot always refreshes (_valid == false).
 * Shared by the normal cycle and the low-battery screen so they use ONE fingerprint -> the charge
 * screen refreshes once on arming, then every identical low-power wake skips the panel. */
/* Wave-1b.2 split: the predicate. True iff the rendered framebuffer differs from the last DISPLAYED
 * one (or none displayed yet). Logs the skip case to preserve the prior behaviour. No side effects on
 * the stored hash -> the caller decides whether to present. Splitting the gate lets the USB-online
 * hold path (Wave 1b.3) drop WLAN around the actual refresh only, between this check and epd_present. */
static bool frame_changed(void)
{
    uint32_t h = fb_hash(fb_buffer(), EPD_FRAME_BYTES);
    if (s_fb_hash_valid && h == s_fb_hash) {
        ESP_LOGI(TAG, "EPD content unchanged (hash=%08lx) -> skip refresh", (unsigned long)h);
        return false;
    }
    return true;
}

/* Wave-1b.2 split: the action. Refresh the panel with the current framebuffer, record its hash, and
 * settle so the EPD peaks decay before sleep. Call only when frame_changed() (the wrapper does). */
static void epd_present(void)
{
    uint32_t h = fb_hash(fb_buffer(), EPD_FRAME_BYTES);
    int64_t t_epd0 = esp_timer_get_time();
    epd_init();
    epd_write_full(fb_buffer());
    epd_refresh();
    epd_sleep();
    ESP_LOGI(TAG, "TIMING epd=%lldms (display done, hash=%08lx)",
             (esp_timer_get_time() - t_epd0) / 1000, (unsigned long)h);
    s_fb_hash = h;
    s_fb_hash_valid = true;
    vTaskDelay(pdMS_TO_TICKS(SETTLE_MS));   /* let the EPD peaks decay before sleep */
}

static void display_framebuffer_if_changed(void)
{
    if (frame_changed()) epd_present();
}

/* Low-battery gate render: draw the charge screen via Berry (render.be branches on the RTC "lb"
 * flag set by the caller) and display it through the shared content-change gate. No WiFi runs. */
static void lowbatt_show_screen(void)
{
    led_on();
    if (!berry_render())
        ESP_LOGW(TAG, "low-batt render failed -> displaying current framebuffer contents");
    display_framebuffer_if_changed();
}

/* Connect for one cycle: multi-WiFi policy or the single-cred path. On success cancels a pending OTA
 * rollback (no-op unless PENDING_VERIFY) + marks creds verified; on a failed policy scan, stops the
 * half-started RF. Does NOT stop the RF on success -- the caller decides (field: stop before EPD;
 * hold: keep associated). Returns whether we ended up connected. */
static bool cycle_connect(const picpak_cfg_t *cfg)
{
    bool used_policy = wifi_store_has_entries();
    bool connected = used_policy ? berry_policy()
                                 : net_wifi_connect(cfg->ssid, cfg->pass, 20000);
    if (connected) {
        ota_mark_valid_if_pending();
        cfg_set_verified(true);
    } else {
        if (used_policy) net_wifi_stop();   /* scan may have started WiFi even without a match */
        ESP_LOGW(TAG, "WiFi unavailable -> rendering offline (autonomous)");
    }
    return connected;
}

/* One refresh cycle. Renders the frame ON-DEVICE via Berry -> display; autonomous even with no net.
 *
 * keep_online=false (field/battery, the default): connect -> RF off -> settle -> render -> display,
 * then the caller deep-sleeps. Byte-for-byte the prior behaviour.
 *
 * keep_online=true (USB data host, Wave 1b.3): hold the WLAN association across cycles. The connect
 * phase is SKIPPED while a link is already held -> no per-cycle re-assoc (PANIC guard, ctx 019efd80).
 * Render with WLAN up (fb is static, VM+WiFi coexist as in the policy phase); drop RF only around an
 * ACTUAL EPD refresh (Wave-2 gate as a predicate), then reconnect IMMEDIATELY -- the keep-awake wait
 * is up to an hour, so a lazy reconnect would strand the link. Returns the next sleep duration. */
static uint32_t run_cycle_inner(const picpak_cfg_t *cfg, bool keep_online)
{
    led_blink_start();   /* transfer phase -> blink */
    if (wifi_store_migrate_legacy(cfg))
        ESP_LOGI(TAG, "migrated legacy NVS WiFi into multi-WiFi store");

    bool connected = net_is_connected() ? true : cycle_connect(cfg);

    led_on();   /* transfer done -> solid during render + EPD refresh */

    uint32_t field_override = 0;   /* a C2 SLEEP intent overrides this cycle's wake interval */
    if (!keep_online) {
        if (connected) {
            /* Field path (battery): one C2 poll per wake while WLAN is up, BEFORE the
             * brownout-decoupling RF-off -> the device reconnects to the backend each cycle. A single
             * poll (no long-poll hold): on battery the device deep-sleeps between wakes, so there is no
             * connection to hold. c2_poll self-skips when C2 is unconfigured/unbonded. */
            uint32_t c2_sl = 0;
            cmd_intent_t c2in = c2_poll(&c2_sl, NULL, 0);
            if (c2in == CMD_INTENT_REBOOT) esp_restart();           /* never returns */
            if (c2in == CMD_INTENT_SLEEP && c2_sl) field_override = c2_sl;
            /* RF off BEFORE the EPD charge-pump peak (brownout decoupling), then settle so the
             * supply recovers between the WiFi peak and the EPD peak. */
            net_wifi_stop();
            vTaskDelay(pdMS_TO_TICKS(SETTLE_MS));
        }
        /* A32 W2 present-gate hardening: present ONLY on a valid, fully-populated frame.
         * s_fb is plain .bss (fb.c:16), zeroed on every deep-sleep wake, so presenting after
         * a failed render pushes an all-zero framebuffer through the content gate and blanks
         * the panel (palette 0 = BLACK). On render-fail keep the last frame -- E-paper is
         * bistable, the last frame physically survives (design 32, Naht B / W-A32.2). */
        if (present_gate_allows(berry_render()))
            display_framebuffer_if_changed();   /* Wave-2 content-change gate (shared fingerprint) */
        else
            ESP_LOGW(TAG, "render failed -> keeping last frame (no present)");
    } else {
        if (!berry_render())
            ESP_LOGW(TAG, "render failed -> displaying current framebuffer contents");
        if (frame_changed()) {              /* a real refresh: decouple WLAN, refresh, re-associate */
            net_wifi_stop();
            vTaskDelay(pdMS_TO_TICKS(SETTLE_MS));
            epd_present();
            cycle_connect(cfg);             /* immediate reconnect (keep-awake wait is ~1h) */
        }
        /* unchanged content: WLAN stays associated, panel untouched */
    }
    /* Next wake from NVS (Design 16: resolves the interval half of TODO(config-pull) — SETWAKE / NVS
     * picpak/wake_s, default DEFAULT_WAKE_S); a C2 SLEEP intent overrides it for this cycle. Pulling
     * the rest of the schedule from the Berry config script stays a later wave. */
    return field_override ? field_override : cfg_wake_s(DEFAULT_WAKE_S);
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
static uint32_t run_cycle(const picpak_cfg_t *cfg, bool keep_online)
{
#if !TETHER_DEBUG_KEEP_USB
    usb_serial_jtag_ll_phy_enable_pad(false);   /* detach USB from the bus */
#endif
    uint32_t nw = run_cycle_inner(cfg, keep_online);
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

/* Windowed USB-host presence. usb_serial_jtag_is_connected() reflects the SOF tick-hook,
 * but a single read is a 3 ms knife-edge: a transient SOF gap (a WiFi/EPD burst, or the
 * re-enum after rst:0x15) reads false and would wrongly drop the device into deep sleep.
 * Sample the flag over a ~300 ms window and require a majority -> rides out the gaps, so
 * "host gone" is only declared on a real, sustained SOF absence. This detects an ACTIVE
 * SOF-framing host; a dumb 5 V charger / suspended bus sends no SOF and reads false by
 * design (the C3 has no VBUS sense -> power-present is a separate, voltage-based concern). */
static bool usb_host_active(void)
{
    int hits = 0;
    for (int i = 0; i < 30; i++) {          /* 30 x 10 ms = 300 ms window */
        if (usb_serial_jtag_is_connected()) hits++;
        vTaskDelay(pdMS_TO_TICKS(10));
    }
    return hits >= 20;                       /* >= 2/3 of the window saw a host */
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
    while (usb_host_active()) {
        bool by_button = false, by_c2_refresh = false;
        /* The field run_cycle(false) stopped the WLAN; bring it back up so the C2 long-poll can run. */
        if (c2_keepawake_active() && !net_is_connected()) cycle_connect(cfg);
        int64_t t0 = esp_timer_get_time();
        while (esp_timer_get_time() - t0 < (int64_t)wake_s * 1000000LL) {
            /* C2 long-poll (Design 16): the backend holds the connection until a command for this
             * device lands or the budget elapses -> near-instant delivery, ~0 idle requests. Blocks up
             * to ~C2_LONGPOLL_S. c2_poll persists any ack BEFORE returning the intent, so actioning a
             * reboot here can't loop. When C2 is off/unbonded/disconnected we idle-tick instead. */
            if (c2_keepawake_active() && net_is_connected()) {
                uint32_t c2_sl = 0;
                cmd_intent_t in = c2_poll(&c2_sl, NULL, C2_LONGPOLL_S);
                if (in == CMD_INTENT_REBOOT) esp_restart();           /* never returns */
                if (in == CMD_INTENT_SLEEP)  return c2_sl ? c2_sl : wake_s;
                if (in == CMD_INTENT_REFRESH) { by_c2_refresh = true; break; }  /* -> run_cycle below */
            } else {
                vTaskDelay(pdMS_TO_TICKS(KEEPALIVE_POLL_MS));
            }
            /* Confirm a disconnect over a window before sleeping -> a transient SOF gap (WiFi/EPD
             * burst) no longer drops us into deep sleep. Checked after each poll return: a held
             * long-poll delays this by at most one budget, acceptable on USB power. */
            if (!usb_serial_jtag_is_connected() && !usb_host_active()) {
                ESP_LOGI(TAG, "USB host gone (windowed confirm) -> switching to deep sleep (%lus)",
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
            ESP_LOGI(TAG, "keep-awake: %s", by_c2_refresh ? "C2 refresh intent" : "periodic refresh");
        }
        wake_s = run_cycle(cfg, true);   /* USB data host present -> HOLD WLAN across cycles (Wave 1b.3) */
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

    /* Smart low-battery gate (default-off NVS flag picpak/lb_on): below ARM the device polls in
     * short deep sleeps and resumes on a detected voltage rise (no VBUS HW), instead of running a
     * normal cycle on a near-dead cell. Runs every wake on the early pre-load reading; off /
     * healthy / implausible falls through unchanged. Sleeps via enter_deep_sleep so the recovery
     * guard's bad-boot counter is cleared every low-power cycle (no false bootloop). */
    lowbatt_action_t lb = lowbatt_gate(dev_batt_mv(), cause);
    if (lb != LOWBATT_NORMAL) {
        if (lb == LOWBATT_ARM) {
            rtc_put("lb", "1");        /* render.be branches on this -> draw the charge screen */
            lowbatt_show_screen();     /* once; identical low-power wakes skip the panel (Wave-2) */
        }
        ESP_LOGI(TAG, "low-battery gate -> deep sleep %lus", (unsigned long)lowbatt_wake_s());
        enter_deep_sleep(lowbatt_wake_s());   /* never returns */
    }
    rtc_remove("lb");                  /* not low -> the normal screen renders this cycle */

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
        if (!have && probe.url[0] && wifi_store_has_entries()) have = true;
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
    bool have_cfg = cfg_load(&cfg);
    if (!have_cfg && !(cfg.url[0] && wifi_store_has_entries())) {
        /* Autonomous: render even without usable WiFi+URL config. Provision via USB
         * (SETWIFI/SETURL) or the web tool to enable OTA / future script+image pull. */
        ESP_LOGW(TAG, "no NVS config -> rendering autonomously (offline); provision for OTA/pull");
    }

    uint32_t next_wake;
    if (cact == CONSOLE_PRESS) {
        uint32_t n = console_arg ? console_arg : 1;
        ESP_LOGI(TAG, "PRESS test: %lu simulated button-press cycles", (unsigned long)n);
        next_wake = DEFAULT_WAKE_S;
        for (uint32_t i = 0; i < n; i++) {
            next_wake = run_cycle(&cfg, false);   /* PRESS = deep-sleep-cycle simulation -> field path */
            if (i + 1 < n) vTaskDelay(pdMS_TO_TICKS(3000));   /* pause between cycles */
        }
    } else if (skip_fetch) {
        next_wake = console_arg ? console_arg : DEFAULT_WAKE_S;
        ESP_LOGI(TAG, "SLEEP requested -> sleeping %lus without fetch", (unsigned long)next_wake);
    } else {
        next_wake = run_cycle(&cfg, false);   /* normal field cycle -> deep sleep after */
    }

    /* USB keep-awake: as long as a host is attached, do not sleep. A SLEEP explicitly
     * requested via the console (skip_fetch) deliberately keeps priority -> this way
     * the deep-sleep path can be tested on the tether too. */
    if (!skip_fetch && usb_host_active()) {
        next_wake = run_keep_awake(&cfg, next_wake);
    }

    enter_deep_sleep(next_wake);
}

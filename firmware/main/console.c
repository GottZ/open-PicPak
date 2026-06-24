#include "console.h"
#include "config.h"
#include "logbuf.h"
#include "net.h"
#include "ota.h"
#include "guard.h"
#include "store.h"
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "driver/usb_serial_jtag.h"
#include "driver/usb_serial_jtag_vfs.h"
#include "esp_heap_caps.h"
#include "esp_idf_version.h"

#define CON_LINE_MAX 160
#define WINDOW_MS    3000      /* brief first-char peek when config already exists */
#define CONSOLE_STAY (-1)      /* internal sentinel: command handled, loop continues */

static bool s_io_ready;
static uint32_t s_sleep_secs;   /* set by "SLEEP <s>"; 0 = not specified */

static void io_init(void)
{
    if (s_io_ready) return;
    /* Install the interrupt-driven USB-Serial-JTAG driver and redirect the stdio
     * VFS onto it -> blocking reads via read_bytes, printf TX over the same ring.
     * (IDF v5.4: usb_serial_jtag_vfs_use_driver.) */
    usb_serial_jtag_driver_config_t c = USB_SERIAL_JTAG_DRIVER_CONFIG_DEFAULT();
    usb_serial_jtag_driver_install(&c);
    usb_serial_jtag_vfs_use_driver();
    setvbuf(stdin, NULL, _IONBF, 0);
    setvbuf(stdout, NULL, _IONBF, 0);
    s_io_ready = true;
}

/* Read one character. timeout_ms < 0 = blocking. -1 = nothing read. */
static int read_char(int timeout_ms)
{
    uint8_t ch;
    TickType_t to = (timeout_ms < 0) ? portMAX_DELAY : pdMS_TO_TICKS(timeout_ms);
    return (usb_serial_jtag_read_bytes(&ch, 1, to) == 1) ? ch : -1;
}

/* Reads a line with echo. Waits for the FIRST character up to first_to_ms
 * (-1 = forever; 0 = block while a USB host stays attached, give up only on disconnect),
 * then blocks until CR/LF. Returns the length, -1 if no character arrived (no input). */
static int read_line(char *buf, size_t cap, int first_to_ms)
{
    int ch;
    if (first_to_ms == 0) {
        /* No creds / forced setup = the console is the ONLY control path. Don't time out
         * into a no-op sleep/restart; stay open while a host can still type. Give up only
         * when the USB host actually disconnects (field: the caller then sleeps). */
        do { ch = read_char(500); } while (ch < 0 && usb_serial_jtag_is_connected());
    } else {
        ch = read_char(first_to_ms);
    }
    if (ch < 0) return -1;
    size_t pos = 0;
    for (;;) {
        if (ch == '\r' || ch == '\n') {
            printf("\r\n");
            buf[pos] = '\0';
            return (int)pos;
        } else if (ch == 0x08 || ch == 0x7f) {      /* Backspace / DEL */
            if (pos > 0) { pos--; printf("\b \b"); }
        } else if (ch >= 0x20 && pos < cap - 1) {
            buf[pos++] = (char)ch;
            putchar(ch);                             /* echo */
        }
        ch = read_char(-1);
        if (ch < 0) { buf[pos] = '\0'; return (int)pos; }
    }
}

static void print_help(void)
{
    printf("Commands:\r\n"
           "  SETWIFI <ssid> <pass>   save WiFi (pass = rest of line)\r\n"
           "  SETURL  <url>           save frame URL\r\n"
           "  INFO                    show status\r\n"
           "  REFRESH                 fetch image now + sleep\r\n"
           "  SLEEP [s]               sleep without fetch (optional s seconds)\r\n"
           "  PRESS [n]               n simulated button-press cycles (test, default 1)\r\n"
           "  LOG [CLEAR]             show / clear the FW-internal log ring\r\n"
           "  NETCLR                  discard connect cache (BSSID/IP) -> next connect cold\r\n"
           "  OTA                     OTA status: running/boot/next partition + image state\r\n"
           "  GUARD [CLEAR|THRESHOLD n|UPTIME ms]  bootloop-guard: status / clear / tune\r\n"
           "  STORE OK|LIST|GET k|SET k v|DEL k   littlefs key-value store\r\n"
           "  ERASE                   delete config\r\n"
           "  HELP                    this help\r\n");
}

static void cmd_info(uint32_t boot_count)
{
    picpak_cfg_t cfg;
    bool have = cfg_load(&cfg);
    printf("--- PicPak FW ---\r\n");
    printf("boot_count : %lu\r\n", (unsigned long)boot_count);
    printf("idf        : %s\r\n", IDF_VER);
    printf("free heap  : %lu B\r\n", (unsigned long)heap_caps_get_free_size(MALLOC_CAP_DEFAULT));
    printf("ssid       : %s\r\n", cfg.ssid[0] ? cfg.ssid : "(empty)");
    printf("pass       : %s\r\n", cfg.pass[0] ? "(set)" : "(empty)");
    printf("url        : %s\r\n", cfg.url[0] ? cfg.url : "(empty)");
    printf("config     : %s\r\n", have ? "complete" : "INCOMPLETE (ssid+url required)");
}

/* Processes a line. Returns CONSOLE_STAY or an exit action. */
static int handle(char *line, uint32_t boot_count)
{
    char *p = line;
    while (*p == ' ') p++;
    if (*p == '\0') return CONSOLE_STAY;

    char *cmd = p;
    char *rest = strchr(p, ' ');
    if (rest) { *rest++ = '\0'; while (*rest == ' ') rest++; }
    else rest = p + strlen(p);     /* points to "" */

    if (strcasecmp(cmd, "SETWIFI") == 0) {
        char *ssid = rest;
        char *pass = strchr(rest, ' ');
        if (pass) { *pass++ = '\0'; while (*pass == ' ') pass++; }
        else pass = rest + strlen(rest);   /* empty password (open network) */
        if (ssid[0] == '\0') {
            printf("ERR  usage: SETWIFI <ssid> <pass>\r\n");
        } else {
            esp_err_t e = cfg_set_wifi(ssid, pass);
            if (e == ESP_OK) printf("OK   WiFi saved (ssid=\"%s\", pass=%s)\r\n",
                                    ssid, pass[0] ? "set" : "empty");
            else printf("ERR  NVS write error %d\r\n", (int)e);
        }
    } else if (strcasecmp(cmd, "SETURL") == 0) {
        if (rest[0] == '\0') {
            printf("ERR  usage: SETURL <url>\r\n");
        } else {
            esp_err_t e = cfg_set_url(rest);
            if (e == ESP_OK) printf("OK   URL saved (%s)\r\n", rest);
            else printf("ERR  NVS write error %d\r\n", (int)e);
        }
    } else if (strcasecmp(cmd, "INFO") == 0) {
        cmd_info(boot_count);
    } else if (strcasecmp(cmd, "ERASE") == 0) {
        esp_err_t e = cfg_erase();
        printf(e == ESP_OK ? "OK   config deleted\r\n" : "ERR  %d\r\n", (int)e);
    } else if (strcasecmp(cmd, "REFRESH") == 0) {
        printf("OK   -> fetch starting\r\n");
        return CONSOLE_REFRESH;
    } else if (strcasecmp(cmd, "SLEEP") == 0) {
        s_sleep_secs = (rest[0] != '\0') ? (uint32_t)strtoul(rest, NULL, 10) : 0;
        if (s_sleep_secs)
            printf("OK   -> sleeping %lus without fetch (button wakes immediately)\r\n",
                   (unsigned long)s_sleep_secs);
        else
            printf("OK   -> sleeping without fetch (default duration)\r\n");
        return CONSOLE_SLEEP;
    } else if (strcasecmp(cmd, "PRESS") == 0) {
        s_sleep_secs = (rest[0] != '\0') ? (uint32_t)strtoul(rest, NULL, 10) : 1;
        printf("OK   -> %lu simulated button-press cycles\r\n", (unsigned long)s_sleep_secs);
        return CONSOLE_PRESS;
    } else if (strcasecmp(cmd, "LOG") == 0) {
        if (strcasecmp(rest, "CLEAR") == 0) {
            logbuf_clear();
            printf("OK   log ring cleared\r\n");
        } else {
            logbuf_dump();
        }
    } else if (strcasecmp(cmd, "NETCLR") == 0) {
        net_cache_clear();
        printf("OK   connect cache discarded -> next connect does scan + DHCP\r\n");
    } else if (strcasecmp(cmd, "OTA") == 0) {
        ota_print_status();
    } else if (strcasecmp(cmd, "GUARD") == 0) {
        char *sub = rest;
        char *n = strchr(rest, ' ');
        if (n) { *n++ = '\0'; while (*n == ' ') n++; }
        if (strcasecmp(sub, "CLEAR") == 0) {
            guard_clear();
            printf("OK   bootloop counter cleared -> next reset is a normal boot\r\n");
        } else if (strcasecmp(sub, "THRESHOLD") == 0) {
            if (n && *n) {
                guard_set_threshold((unsigned)strtoul(n, NULL, 10));
                printf("OK   safe-mode threshold set to %u bad boots\r\n", guard_threshold());
            } else {
                printf("ERR  usage: GUARD THRESHOLD <n>\r\n");
            }
        } else if (strcasecmp(sub, "UPTIME") == 0) {
            if (n && *n) {
                guard_set_stable_ms((unsigned)strtoul(n, NULL, 10));
                printf("OK   stable-uptime set to %u ms\r\n", guard_stable_ms());
            } else {
                printf("ERR  usage: GUARD UPTIME <ms>\r\n");
            }
        } else {
            printf("guard: bad_boots=%u threshold=%u stable_ms=%u\r\n",
                   guard_bad_boots(), guard_threshold(), guard_stable_ms());
        }
    } else if (strcasecmp(cmd, "STORE") == 0) {
        /* littlefs key-value store. STORE never mounts itself and reports the real state
         * even in safe mode (where the store is intentionally not mounted). GET prints text
         * values; binary values (Berry bytes) are shown up to the first NUL. */
        char *sub = rest;
        char *arg = strchr(rest, ' ');
        if (arg) { *arg++ = '\0'; while (*arg == ' ') arg++; }
        else arg = rest + strlen(rest);   /* "" */
        if (strcasecmp(sub, "OK") == 0) {
            printf("STORE %s\r\n", store_fs_ok() ? "ok (fs mounted, writable)"
                                                 : "unavailable (no fs partition / mount failed)");
        } else if (strcasecmp(sub, "LIST") == 0) {
            store_list();
        } else if (strcasecmp(sub, "GET") == 0) {
            if (arg[0] == '\0') { printf("ERR  usage: STORE GET <key>\r\n"); }
            else {
                char vbuf[256];
                long len = store_fetch(arg, vbuf, sizeof vbuf);
                if (len < 0) printf("STORE (nil) \"%s\" not found\r\n", arg);
                else printf("STORE %s = \"%s\" (%ld B)\r\n", arg, vbuf, len);
            }
        } else if (strcasecmp(sub, "SET") == 0) {
            char *val = strchr(arg, ' ');
            if (val) { *val++ = '\0'; while (*val == ' ') val++; }
            else val = arg + strlen(arg);   /* empty value allowed */
            if (arg[0] == '\0') printf("ERR  usage: STORE SET <key> <value>\r\n");
            else printf(store_put(arg, val) ? "OK   stored\r\n"
                                            : "ERR  store failed (bad key / FS down / full)\r\n");
        } else if (strcasecmp(sub, "DEL") == 0) {
            if (arg[0] == '\0') printf("ERR  usage: STORE DEL <key>\r\n");
            else printf(store_remove(arg) ? "OK   deleted\r\n" : "ERR  not found / FS down\r\n");
        } else {
            printf("ERR  usage: STORE OK|LIST|GET k|SET k v|DEL k\r\n");
        }
    } else if (strcasecmp(cmd, "HELP") == 0 || strcasecmp(cmd, "?") == 0) {
        print_help();
    } else {
        printf("?    unknown: \"%s\" (HELP for help)\r\n", cmd);
    }
    return CONSOLE_STAY;
}

console_action_t console_run(uint32_t boot_count, uint32_t *sleep_secs_out, bool force_open)
{
    io_init();
    s_sleep_secs = 0;
    picpak_cfg_t cfg;
    bool have = cfg_load(&cfg);

    printf("\r\n=== PicPak Setup Console ===\r\n");
    if (force_open) {
        printf("Setup mode active. Send SETWIFI + SETURL (or use the web tool).\r\n");
    } else if (have) {
        printf("Config present. Press a key within %ds for setup, otherwise normal run.\r\n",
               WINDOW_MS / 1000);
    } else {
        printf("No config in NVS -> setup mode. SETWIFI + SETURL required.\r\n");
    }
    print_help();
    printf("> ");

    console_action_t action = CONSOLE_PROCEED;
    char line[CON_LINE_MAX];
    /* First-char window: a brief 3s peek only when config exists AND setup was not forced.
     * Otherwise (no creds / forced setup) stay open while a USB host is attached -- the
     * console is then the only way to provision; timing out into sleep/restart does nothing. */
    int first_to = (have && !force_open) ? WINDOW_MS : 0;
    int n = read_line(line, sizeof(line), first_to);
    if (n < 0) {
        printf("\r\n(no setup -> normal run)\r\n");
    } else {
        for (;;) {
            int a = handle(line, boot_count);
            if (a != CONSOLE_STAY) { action = (console_action_t)a; break; }
            printf("> ");
            /* Between commands: the config-exists path uses a short inactivity timeout (3s)
             * to proceed to a normal run. In setup mode (force_open) stay open while a USB
             * host is attached and give up only on disconnect -- the device never hangs
             * forever without a host, but the console stays usable as long as one is there. */
            n = read_line(line, sizeof(line), force_open ? 0 : WINDOW_MS);
            if (n < 0) {
                printf("\r\n(console timeout -> normal run)\r\n");
                break;
            }
        }
    }
    /* Before the WiFi phase, TEAR DOWN the interrupt-driven USB-Serial-JTAG driver
     * again: if it stays active it keeps the USB peripheral alive during the WiFi TX
     * (auth/assoc/HTTP) -> VBUS glitch on the USB tether -> USB_UART_CHIP_RESET
     * (reset loop on fetch). Without the driver the WiFi phase runs over the passive
     * boot-console path like phase 2/3 (fetches stably over USB). In the field (timer
     * wake) the driver is never installed anyway. Order: first switch the VFS back to
     * polling, then uninstall (otherwise printf into a torn-down driver). */
    fflush(stdout);
    usb_serial_jtag_vfs_use_nonblocking();
    usb_serial_jtag_driver_uninstall();
    s_io_ready = false;
    if (sleep_secs_out) *sleep_secs_out = s_sleep_secs;
    return action;
}

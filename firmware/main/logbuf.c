#include "logbuf.h"
#include <stdio.h>
#include <string.h>
#include <stdarg.h>
#include "freertos/FreeRTOS.h"
#include "esp_log.h"
#include "esp_attr.h"

#define LOGBUF_SZ  2048   /* RTC-RAM ring; ~25-40 log lines (1-2 cycles) */
#define LOG_LINE_MAX 192  /* stack buffer per log line in the hook */

/* RTC-RAM (.rtc.bss, no initializer): cold start -> 0/empty, warm reset
 * (rst:0x15) + deep-sleep wake -> preserved. Same behavior as s_tm_* and
 * s_boot_count in main.c -> the ring survives the port-open reset on fetch. */
RTC_DATA_ATTR static char     s_ring[LOGBUF_SZ];
RTC_DATA_ATTR static uint32_t s_head;   /* next write position */
RTC_DATA_ATTR static uint32_t s_len;    /* filled bytes (<= LOGBUF_SZ) */

static vprintf_like_t s_orig;           /* original log sink (serial) */
static portMUX_TYPE   s_mux = portMUX_INITIALIZER_UNLOCKED;

static void ring_put(const char *s, int n)
{
    portENTER_CRITICAL(&s_mux);
    for (int i = 0; i < n; i++) {
        s_ring[s_head] = s[i];
        if (++s_head >= LOGBUF_SZ) s_head = 0;
        if (s_len < LOGBUF_SZ) s_len++;
    }
    portEXIT_CRITICAL(&s_mux);
}

/* Runs in the context of the logging task. Keep it lean (stack!). va_copy is
 * mandatory: the va_list is consumed twice (ring + original sink). */
static int hook(const char *fmt, va_list ap)
{
    char line[LOG_LINE_MAX];
    va_list ap2;
    va_copy(ap2, ap);
    int n = vsnprintf(line, sizeof(line), fmt, ap2);
    va_end(ap2);
    if (n > 0) ring_put(line, n < LOG_LINE_MAX ? n : LOG_LINE_MAX - 1);
    return s_orig ? s_orig(fmt, ap) : 0;
}

void logbuf_init(void)
{
    if (!s_orig) s_orig = esp_log_set_vprintf(hook);
}

void logbuf_dump(void)
{
    /* Snapshot head/len under lock, then output lock-free: the dump runs in the
     * interactive console moment (no parallel logging expected), and putchar over
     * USB-serial must not block inside the critical section. */
    portENTER_CRITICAL(&s_mux);
    uint32_t len = s_len, head = s_head;
    portEXIT_CRITICAL(&s_mux);

    if (len == 0) { printf("(log ring empty)\r\n"); return; }
    uint32_t start = (head + LOGBUF_SZ - len) % LOGBUF_SZ;
    printf("--- log ring (%lu B) ---\r\n", (unsigned long)len);
    for (uint32_t i = 0; i < len; i++)
        putchar(s_ring[(start + i) % LOGBUF_SZ]);
    printf("\r\n--- end ---\r\n");
}

void logbuf_clear(void)
{
    portENTER_CRITICAL(&s_mux);
    s_head = 0;
    s_len = 0;
    portEXIT_CRITICAL(&s_mux);
}

size_t logbuf_export(char *dst, size_t cap)
{
    if (cap == 0) return 0;
    portENTER_CRITICAL(&s_mux);
    uint32_t len = s_len, head = s_head;
    portEXIT_CRITICAL(&s_mux);
    uint32_t start = (head + LOGBUF_SZ - len) % LOGBUF_SZ;
    size_t o = 0;
    for (uint32_t i = 0; i < len && o < cap - 1; i++) {
        char c = s_ring[(start + i) % LOGBUF_SZ];
        if (c == '\n') c = '|';                 /* header-safe line separator */
        else if ((unsigned char)c < 0x20 || c == 0x7f) continue;  /* drop \r + control chars */
        dst[o++] = c;
    }
    dst[o] = '\0';
    return o;
}

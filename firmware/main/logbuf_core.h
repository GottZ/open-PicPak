#ifndef LOGBUF_CORE_H
#define LOGBUF_CORE_H
/* Pure log-ring state helpers, shared by firmware code and host tests. */
#include <stdint.h>

#define LOGBUF_CAPACITY 2048u

typedef struct {
    uint32_t epoch;
    uint32_t head;
    uint32_t len;
    uint32_t total;
} logbuf_state_t;

static inline uint32_t logbuf__sat_add(uint32_t a, uint32_t b)
{
    return UINT32_MAX - a < b ? UINT32_MAX : a + b;
}

static inline void logbuf_state_clear(logbuf_state_t *s)
{
    s->head = 0;
    s->len = 0;
    s->total = 0;
}

static inline void logbuf_state_set_epoch(logbuf_state_t *s, uint32_t epoch)
{
    if (s->epoch == epoch) return;
    s->epoch = epoch;
    logbuf_state_clear(s);
}

static inline void logbuf_state_advance(logbuf_state_t *s, uint32_t n)
{
    if (n == 0) return;
    s->head = (s->head + n) % LOGBUF_CAPACITY;
    s->len = s->len + n > LOGBUF_CAPACITY ? LOGBUF_CAPACITY : s->len + n;
    s->total = logbuf__sat_add(s->total, n);
}

#endif /* LOGBUF_CORE_H */

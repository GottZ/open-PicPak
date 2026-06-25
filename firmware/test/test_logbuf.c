/* Host unit test for pure log-ring state helpers.
 * Compile + run on the host (no ESP hardware): cc -I main test/test_logbuf.c -o build/test_logbuf && build/test_logbuf */
#include "../main/logbuf_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

int main(void)
{
    logbuf_state_t s = { 0 };

    logbuf_state_advance(&s, 10);
    CHECK(s.head == 10);
    CHECK(s.len == 10);
    CHECK(s.total == 10);

    logbuf_state_advance(&s, LOGBUF_CAPACITY);
    CHECK(s.head == 10);
    CHECK(s.len == LOGBUF_CAPACITY);
    CHECK(s.total == LOGBUF_CAPACITY + 10u);

    logbuf_state_set_epoch(&s, 7);
    CHECK(s.epoch == 7);
    CHECK(s.head == 0 && s.len == 0 && s.total == 0);

    logbuf_state_advance(&s, 3);
    logbuf_state_set_epoch(&s, 7);
    CHECK(s.head == 3 && s.len == 3 && s.total == 3);

    s.total = UINT32_MAX - 1u;
    logbuf_state_advance(&s, 10);
    CHECK(s.total == UINT32_MAX);

    if (fails == 0) printf("test_logbuf: ALL PASS\n");
    else            printf("test_logbuf: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

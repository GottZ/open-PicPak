/* Host unit test for the RTC key-value pure logic (rtc_core.h).
 * cc test/test_rtc.c -o build/tr && ./build/tr
 * Negative-probes the bounds-check: an overflowing set must leave the buffer untouched. */
#include "../main/rtc_core.h"
#include <stdio.h>
#include <string.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

static int val_is(const uint8_t *buf, uint16_t len, const char *key, const char *want)
{
    const uint8_t *v; uint16_t vl;
    if (!rtc_kv_get(buf, len, key, &v, &vl)) return 0;
    return vl == strlen(want) && memcmp(v, want, vl) == 0;
}

int main(void)
{
    uint8_t buf[64];          /* small cap to make overflow easy to probe */
    uint16_t len = 0;

    /* 1. Roundtrip. */
    CHECK(rtc_kv_set(buf, sizeof buf, &len, "slot", "home", 4));
    CHECK(rtc_kv_has(buf, len, "slot"));
    CHECK(val_is(buf, len, "slot", "home"));

    /* 2. Second key coexists. */
    CHECK(rtc_kv_set(buf, sizeof buf, &len, "retry", "2", 1));
    CHECK(val_is(buf, len, "slot", "home") && val_is(buf, len, "retry", "2"));

    /* 3. Update existing key in place (value replaced, not duplicated). */
    CHECK(rtc_kv_set(buf, sizeof buf, &len, "slot", "office", 6));
    CHECK(val_is(buf, len, "slot", "office"));
    CHECK(val_is(buf, len, "retry", "2"));            /* the other key is intact */

    /* 4. Missing key: clean false, no crash. */
    CHECK(!rtc_kv_has(buf, len, "nope"));
    CHECK(!rtc_kv_get(buf, len, "nope", NULL, NULL));

    /* 5. Invalid args rejected. */
    CHECK(!rtc_kv_set(buf, sizeof buf, &len, "", "x", 1));
    char longk[RTC_KEY_MAX + 3]; memset(longk, 'k', sizeof longk - 1); longk[sizeof longk - 1] = 0;
    CHECK(!rtc_kv_set(buf, sizeof buf, &len, longk, "x", 1));
    CHECK(!rtc_kv_set(buf, sizeof buf, &len, "big", "x", RTC_VAL_MAX + 1));

    /* 6. BOUNDS-CHECK (the core negative probe): a set that does not fit must return false
     *    and leave the buffer + length completely untouched (vs an unchecked memcpy/OOB).
     *    With "slot"/"retry" present in a 64B buffer, a RTC_VAL_MAX value already overflows. */
    uint16_t len_before = len;
    uint8_t snapshot[64]; memcpy(snapshot, buf, len);
    char big[RTC_VAL_MAX]; memset(big, 'A', sizeof big);
    bool rc = rtc_kv_set(buf, sizeof buf, &len, "huge", big, sizeof big);
    CHECK(rc == false);                               /* it did not fit */
    CHECK(len == len_before);                         /* length untouched */
    CHECK(memcmp(buf, snapshot, len) == 0);           /* bytes untouched */
    CHECK(val_is(buf, len, "slot", "office"));        /* pre-existing data survived the failed set */
    CHECK(val_is(buf, len, "retry", "2"));
    CHECK(!rtc_kv_has(buf, len, "huge"));             /* the failed key was not added */

    /* 7. Delete. */
    CHECK(rtc_kv_del(buf, &len, "slot"));
    CHECK(!rtc_kv_has(buf, len, "slot"));
    CHECK(val_is(buf, len, "retry", "2"));            /* delete compacts without corrupting neighbours */
    CHECK(!rtc_kv_del(buf, &len, "slot"));            /* deleting a missing key -> false */

    if (fails == 0) printf("test_rtc: ALL PASS\n");
    else            printf("test_rtc: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

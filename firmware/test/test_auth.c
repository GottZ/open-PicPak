/* Host unit test for the HOTP pure logic (auth_core.h).
 * Compile + run on the host (no ESP hardware): cc -I main test/test_auth.c -o build/test_auth && build/test_auth */
#include "../main/auth_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

int main(void)
{
    const uint8_t key[] = "12345678901234567890";
    const uint32_t hotp6[] = {
        755224, 287082, 359152, 969429, 338314,
        254676, 287922, 162583, 399871, 520489,
    };
    const uint32_t hotp8[] = {
        84755224, 94287082, 37359152, 26969429, 40338314,
        68254676, 18287922, 82162583, 73399871, 45520489,
    };

    for (uint64_t c = 0; c < 10; c++) {
        CHECK(auth_hotp(key, sizeof key - 1, c, 6) == hotp6[c]);
        CHECK(auth_hotp(key, sizeof key - 1, c, 8) == hotp8[c]);
    }

    CHECK(auth_hotp(key, sizeof key - 1, 0, 5) == 0);
    CHECK(auth_hotp(key, sizeof key - 1, 0, 9) == 0);

    if (fails == 0) printf("test_auth: ALL PASS\n");
    else            printf("test_auth: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

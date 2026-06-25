/* Host unit test for pure OTA diagnostic helpers.
 * Compile + run on the host (no ESP hardware): cc -I main test/test_ota.c -o build/test_ota && build/test_ota */
#include "../main/ota_core.h"
#include <stdio.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

int main(void)
{
    CHECK(ota_boots_next(0) == 1);
    CHECK(ota_boots_next(41) == 42);
    CHECK(ota_boots_next(UINT32_MAX - 1u) == UINT32_MAX);
    CHECK(ota_boots_next(UINT32_MAX) == UINT32_MAX);

    if (fails == 0) printf("test_ota: ALL PASS\n");
    else            printf("test_ota: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

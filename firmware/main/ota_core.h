#ifndef OTA_CORE_H
#define OTA_CORE_H
/* Pure OTA diagnostic helpers, shared by firmware code and host tests. */
#include <stdint.h>

static inline uint32_t ota_boots_next(uint32_t current)
{
    return current == UINT32_MAX ? UINT32_MAX : current + 1u;
}

#endif /* OTA_CORE_H */

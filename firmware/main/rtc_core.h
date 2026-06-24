#ifndef RTC_CORE_H
#define RTC_CORE_H
/* RTC key-value pure logic — operates on a caller-provided byte buffer + length, no
 * hardware, so it is host-unit-testable. The Berry bindings (in store.c) back it with a
 * small RTC_DATA_ATTR buffer that survives a deep-sleep wake but is nulled on a cold boot
 * / the port-open rst:0x15 (measured) — i.e. ephemeral cross-wake state only.
 *
 * Format: a flat sequence of entries [u8 keylen][key][u16 vallen LE][val]. Tiny by design.
 * The set bounds-check happens BEFORE any mutation: on overflow the buffer + length are
 * left untouched (a real bounds-check, not a canary against the adjacent heap).
 */
#include <stdbool.h>
#include <stdint.h>
#include <string.h>

#define RTC_KEY_MAX 32
#define RTC_VAL_MAX 64

/* Find the entry for key (length klen). Returns its byte offset and (via esz) its total
 * size, or -1 if absent. Stops on any structural inconsistency (defensive against a
 * garbage RTC buffer that survived as noise). */
static int rtc__find(const uint8_t *buf, uint16_t len, const char *key, uint16_t klen, uint16_t *esz)
{
    uint16_t o = 0;
    while ((uint32_t)o + 3u <= len) {
        uint8_t kl = buf[o];
        if ((uint32_t)o + 1u + kl + 2u > len) break;
        uint16_t vl = (uint16_t)buf[o + 1 + kl] | ((uint16_t)buf[o + 1 + kl + 1] << 8);
        uint16_t entry = (uint16_t)(1 + kl + 2 + vl);
        if ((uint32_t)o + entry > len) break;
        if (kl == klen && memcmp(buf + o + 1, key, klen) == 0) { if (esz) *esz = entry; return (int)o; }
        o = (uint16_t)(o + entry);
    }
    return -1;
}

/* Set key=val. false (buffer + length untouched) on invalid key/value or overflow. */
static bool rtc_kv_set(uint8_t *buf, uint16_t cap, uint16_t *len,
                       const char *key, const void *val, uint16_t vlen)
{
    if (!key) return false;
    size_t klen = strlen(key);
    if (klen < 1 || klen > RTC_KEY_MAX || vlen > RTC_VAL_MAX) return false;
    uint16_t entry = (uint16_t)(1 + klen + 2 + vlen);
    uint16_t oldsz = 0;
    int off = rtc__find(buf, *len, key, (uint16_t)klen, &oldsz);
    uint16_t base = (uint16_t)(*len - (off >= 0 ? oldsz : 0));   /* length after dropping any old entry */
    if ((uint32_t)base + entry > cap) return false;             /* bounds-check FIRST, no mutation on fail */
    if (off >= 0) {                                             /* drop old entry (compact) */
        memmove(buf + off, buf + off + oldsz, (size_t)(*len - off - oldsz));
        *len = (uint16_t)(*len - oldsz);
    }
    uint8_t *p = buf + *len;                                    /* append fresh entry */
    *p++ = (uint8_t)klen;
    memcpy(p, key, klen); p += klen;
    *p++ = (uint8_t)(vlen & 0xff);
    *p++ = (uint8_t)(vlen >> 8);
    memcpy(p, val, vlen);
    *len = (uint16_t)(*len + entry);
    return true;
}

static bool rtc_kv_get(const uint8_t *buf, uint16_t len, const char *key,
                       const uint8_t **val, uint16_t *vlen)
{
    if (!key) return false;
    size_t klen = strlen(key);
    if (klen < 1 || klen > RTC_KEY_MAX) return false;
    uint16_t esz = 0;
    int off = rtc__find(buf, len, key, (uint16_t)klen, &esz);
    if (off < 0) return false;
    uint16_t kl = buf[off];
    uint16_t vl = (uint16_t)buf[off + 1 + kl] | ((uint16_t)buf[off + 1 + kl + 1] << 8);
    if (val)  *val = buf + off + 1 + kl + 2;
    if (vlen) *vlen = vl;
    return true;
}

static bool rtc_kv_has(const uint8_t *buf, uint16_t len, const char *key)
{
    return rtc_kv_get(buf, len, key, NULL, NULL);
}

static bool rtc_kv_del(uint8_t *buf, uint16_t *len, const char *key)
{
    if (!key) return false;
    size_t klen = strlen(key);
    if (klen < 1 || klen > RTC_KEY_MAX) return false;
    uint16_t esz = 0;
    int off = rtc__find(buf, *len, key, (uint16_t)klen, &esz);
    if (off < 0) return false;
    memmove(buf + off, buf + off + esz, (size_t)(*len - off - esz));
    *len = (uint16_t)(*len - esz);
    return true;
}

#endif /* RTC_CORE_H */

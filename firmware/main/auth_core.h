#ifndef AUTH_CORE_H
#define AUTH_CORE_H
/* Pure HOTP core: self-contained SHA-1 + HMAC-SHA1 + RFC 4226 truncation.
 * No ESP-IDF or mbedtls dependency, so the exact firmware code is host-testable. */
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#define AUTH_SHA1_LEN 20
#define AUTH_SHA1_BLOCK_LEN 64

typedef struct {
    uint32_t h[5];
    uint64_t len;
    uint8_t buf[AUTH_SHA1_BLOCK_LEN];
    size_t buf_len;
} auth_sha1_ctx_t;

static inline uint32_t auth__rotl32(uint32_t v, unsigned n)
{
    return (v << n) | (v >> (32u - n));
}

static inline uint32_t auth__load_be32(const uint8_t *p)
{
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
           ((uint32_t)p[2] << 8) | (uint32_t)p[3];
}

static inline void auth__store_be32(uint8_t *p, uint32_t v)
{
    p[0] = (uint8_t)(v >> 24);
    p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);
    p[3] = (uint8_t)v;
}

static inline void auth__store_be64(uint8_t *p, uint64_t v)
{
    for (int i = 7; i >= 0; i--) {
        p[i] = (uint8_t)v;
        v >>= 8;
    }
}

static inline void auth__sha1_block(auth_sha1_ctx_t *ctx, const uint8_t block[AUTH_SHA1_BLOCK_LEN])
{
    uint32_t w[80];
    for (int i = 0; i < 16; i++) {
        w[i] = auth__load_be32(block + (size_t)i * 4u);
    }
    for (int i = 16; i < 80; i++) {
        w[i] = auth__rotl32(w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16], 1);
    }

    uint32_t a = ctx->h[0];
    uint32_t b = ctx->h[1];
    uint32_t c = ctx->h[2];
    uint32_t d = ctx->h[3];
    uint32_t e = ctx->h[4];

    for (int i = 0; i < 80; i++) {
        uint32_t f, k;
        if (i < 20) {
            f = (b & c) | ((~b) & d);
            k = 0x5a827999u;
        } else if (i < 40) {
            f = b ^ c ^ d;
            k = 0x6ed9eba1u;
        } else if (i < 60) {
            f = (b & c) | (b & d) | (c & d);
            k = 0x8f1bbcdcu;
        } else {
            f = b ^ c ^ d;
            k = 0xca62c1d6u;
        }
        uint32_t t = auth__rotl32(a, 5) + f + e + k + w[i];
        e = d;
        d = c;
        c = auth__rotl32(b, 30);
        b = a;
        a = t;
    }

    ctx->h[0] += a;
    ctx->h[1] += b;
    ctx->h[2] += c;
    ctx->h[3] += d;
    ctx->h[4] += e;
}

static inline void auth__sha1_init(auth_sha1_ctx_t *ctx)
{
    ctx->h[0] = 0x67452301u;
    ctx->h[1] = 0xefcdab89u;
    ctx->h[2] = 0x98badcfeu;
    ctx->h[3] = 0x10325476u;
    ctx->h[4] = 0xc3d2e1f0u;
    ctx->len = 0;
    ctx->buf_len = 0;
}

static inline void auth__sha1_update(auth_sha1_ctx_t *ctx, const uint8_t *data, size_t len)
{
    ctx->len += (uint64_t)len * 8u;
    while (len > 0) {
        size_t take = AUTH_SHA1_BLOCK_LEN - ctx->buf_len;
        if (take > len) take = len;
        memcpy(ctx->buf + ctx->buf_len, data, take);
        ctx->buf_len += take;
        data += take;
        len -= take;
        if (ctx->buf_len == AUTH_SHA1_BLOCK_LEN) {
            auth__sha1_block(ctx, ctx->buf);
            ctx->buf_len = 0;
        }
    }
}

static inline void auth__sha1_final(auth_sha1_ctx_t *ctx, uint8_t out[AUTH_SHA1_LEN])
{
    uint8_t pad[AUTH_SHA1_BLOCK_LEN] = { 0x80 };
    uint8_t bits[8];
    auth__store_be64(bits, ctx->len);

    size_t pad_len = (ctx->buf_len < 56u) ? (56u - ctx->buf_len) : (120u - ctx->buf_len);
    auth__sha1_update(ctx, pad, pad_len);
    auth__sha1_update(ctx, bits, sizeof bits);

    for (int i = 0; i < 5; i++) {
        auth__store_be32(out + (size_t)i * 4u, ctx->h[i]);
    }
}

static inline void auth__sha1(const uint8_t *data, size_t len, uint8_t out[AUTH_SHA1_LEN])
{
    auth_sha1_ctx_t ctx;
    auth__sha1_init(&ctx);
    auth__sha1_update(&ctx, data, len);
    auth__sha1_final(&ctx, out);
}

static inline void auth_hmac_sha1(const uint8_t *key, size_t klen,
                                  const uint8_t *msg, size_t mlen,
                                  uint8_t mac[AUTH_SHA1_LEN])
{
    uint8_t khash[AUTH_SHA1_LEN];
    uint8_t ipad[AUTH_SHA1_BLOCK_LEN];
    uint8_t opad[AUTH_SHA1_BLOCK_LEN];

    if (klen > AUTH_SHA1_BLOCK_LEN) {
        auth__sha1(key, klen, khash);
        key = khash;
        klen = sizeof khash;
    }

    memset(ipad, 0x36, sizeof ipad);
    memset(opad, 0x5c, sizeof opad);
    for (size_t i = 0; i < klen; i++) {
        ipad[i] ^= key[i];
        opad[i] ^= key[i];
    }

    uint8_t inner[AUTH_SHA1_LEN];
    auth_sha1_ctx_t ctx;
    auth__sha1_init(&ctx);
    auth__sha1_update(&ctx, ipad, sizeof ipad);
    auth__sha1_update(&ctx, msg, mlen);
    auth__sha1_final(&ctx, inner);

    auth__sha1_init(&ctx);
    auth__sha1_update(&ctx, opad, sizeof opad);
    auth__sha1_update(&ctx, inner, sizeof inner);
    auth__sha1_final(&ctx, mac);
}

static inline uint32_t auth__pow10(int digits)
{
    uint32_t n = 1;
    for (int i = 0; i < digits; i++) n *= 10u;
    return n;
}

static inline uint32_t auth_hotp(const uint8_t *k, size_t klen, uint64_t c, int digits)
{
    if (digits < 6 || digits > 8) return 0;

    uint8_t msg[8];
    uint8_t mac[AUTH_SHA1_LEN];
    auth__store_be64(msg, c);
    auth_hmac_sha1(k, klen, msg, sizeof msg, mac);

    unsigned off = mac[AUTH_SHA1_LEN - 1] & 0x0fu;
    uint32_t bin = ((uint32_t)(mac[off] & 0x7fu) << 24) |
                   ((uint32_t)mac[off + 1u] << 16) |
                   ((uint32_t)mac[off + 2u] << 8) |
                   (uint32_t)mac[off + 3u];
    return bin % auth__pow10(digits);
}

#endif /* AUTH_CORE_H */

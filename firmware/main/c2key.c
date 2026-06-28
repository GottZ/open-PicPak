/* C2 long-term device identity — ECDSA P-256 keygen/sign (Doc 15). See c2key.h. */
#include "c2key.h"
#include <string.h>
#include "nvs.h"
#include "esp_random.h"
#include "esp_log.h"
#include "mbedtls/ecdsa.h"
#include "mbedtls/ecp.h"
#include "mbedtls/md.h"
#include "mbedtls/sha256.h"
#include "mbedtls/platform_util.h"

static const char *TAG = "c2key";
#define C2_NS  "picpak"
#define SK_KEY "c2_sk"   /* 32 B private scalar d */
#define PK_KEY "c2_pk"   /* 65 B uncompressed public point (0x04||X||Y) */

/* Hardware TRNG for ECDSA k + keygen. esp_fill_random is a true RNG with the RF subsystem up, which
 * is the case during the re-key flow (WiFi just connected). */
static int rng_cb(void *p, unsigned char *buf, size_t len)
{
    (void)p;
    esp_fill_random(buf, len);
    return 0;
}

static bool nvs_get(const char *key, uint8_t *buf, size_t want)
{
    nvs_handle_t h;
    size_t l = want;
    if (nvs_open(C2_NS, NVS_READONLY, &h) != ESP_OK) return false;
    esp_err_t e = nvs_get_blob(h, key, buf, &l);
    nvs_close(h);
    return e == ESP_OK && l == want;
}

bool c2_key_present(void)
{
    uint8_t d[32];
    bool ok = nvs_get(SK_KEY, d, sizeof d);
    mbedtls_platform_zeroize(d, sizeof d);
    return ok;
}

/* Generate a fresh keypair, persist the private scalar + public point, and return the public point. */
static bool gen_and_store(uint8_t *pub, size_t cap, size_t *publen)
{
    mbedtls_ecdsa_context kp;
    mbedtls_ecdsa_init(&kp);
    bool ok = false;
    uint8_t d[32];
    size_t dl = 0;
    if (mbedtls_ecdsa_genkey(&kp, MBEDTLS_ECP_DP_SECP256R1, rng_cb, NULL) == 0 &&
        mbedtls_ecp_write_key_ext(&kp, &dl, d, sizeof d) == 0 && dl == sizeof d &&
        mbedtls_ecp_write_public_key(&kp, MBEDTLS_ECP_PF_UNCOMPRESSED, publen, pub, cap) == 0) {
        nvs_handle_t h;
        if (nvs_open(C2_NS, NVS_READWRITE, &h) == ESP_OK) {
            esp_err_t e = nvs_set_blob(h, SK_KEY, d, sizeof d);
            if (e == ESP_OK) e = nvs_set_blob(h, PK_KEY, pub, *publen);
            if (e == ESP_OK) e = nvs_commit(h);
            nvs_close(h);
            ok = (e == ESP_OK);
        }
    }
    mbedtls_platform_zeroize(d, sizeof d);
    mbedtls_ecdsa_free(&kp);
    if (ok) ESP_LOGI(TAG, "bonded: new ECDSA P-256 keypair (pub %u B)", (unsigned)*publen);
    else    ESP_LOGE(TAG, "keygen/store failed");
    return ok;
}

bool c2_key_pubkey(uint8_t *out, size_t cap, size_t *outlen)
{
    if (!out || cap < 65 || !outlen) return false;
    /* Existing key: return the stored public point (no recompute needed). */
    size_t l = 65;
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READONLY, &h) == ESP_OK) {
        esp_err_t e = nvs_get_blob(h, PK_KEY, out, &l);
        nvs_close(h);
        if (e == ESP_OK && l == 65) { *outlen = l; return true; }
    }
    return gen_and_store(out, cap, outlen);   /* first call -> bond */
}

bool c2_key_sign(const uint8_t *msg, size_t msglen, uint8_t *sig, size_t cap, size_t *siglen)
{
    uint8_t d[32];
    if (!sig || !siglen || !nvs_get(SK_KEY, d, sizeof d)) return false;
    uint8_t hash[32];
    bool ok = false;
    if (mbedtls_sha256(msg, msglen, hash, 0) == 0) {
        mbedtls_ecdsa_context kp;
        mbedtls_ecdsa_init(&kp);
        /* read_key sets the group + private scalar; ECDSA signing needs only those (not the public
         * point), so no Q recompute is required here. */
        if (mbedtls_ecp_read_key(MBEDTLS_ECP_DP_SECP256R1, &kp, d, sizeof d) == 0 &&
            mbedtls_ecdsa_write_signature(&kp, MBEDTLS_MD_SHA256, hash, sizeof hash,
                                          sig, cap, siglen, rng_cb, NULL) == 0) {
            ok = true;
        }
        mbedtls_ecdsa_free(&kp);
    }
    mbedtls_platform_zeroize(d, sizeof d);
    if (!ok) ESP_LOGW(TAG, "sign failed");
    return ok;
}

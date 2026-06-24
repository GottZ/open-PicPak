/* Host unit test for the key-value store pure logic (store_core.h).
 * Compile + run on the host (no ESP hardware):  cc test/test_store.c -o /tmp/ts && /tmp/ts
 * Part of P1 test-first: negative-probes the key schema, NUL-safety and atomic replace. */
#include "../main/store_core.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

static int fails = 0;
#define CHECK(cond) do { if (!(cond)) { printf("  FAIL: %s\n", #cond); fails++; } } while (0)

/* collect keys from store_kv_each into a sorted-ish count + membership check */
struct collect { int n; char seen[16][STORE_KEY_MAX + 1]; };
static void collect_cb(const char *key, void *ctx) {
    struct collect *c = ctx;
    if (c->n < 16) { snprintf(c->seen[c->n], sizeof c->seen[0], "%s", key); c->n++; }
}
static int has_key(struct collect *c, const char *k) {
    for (int i = 0; i < c->n; i++) if (strcmp(c->seen[i], k) == 0) return 1;
    return 0;
}

int main(void)
{
    char base[] = "/tmp/picpak-store-test-XXXXXX";
    if (!mkdtemp(base)) { perror("mkdtemp"); return 2; }
    char tmpd[128]; snprintf(tmpd, sizeof tmpd, "%s/%s", base, STORE_TMPDIR);
    mkdir(tmpd, 0777);   /* the staging dir (on device: created by store_mount's gc_tmp) */

    char buf[STORE_VAL_MAX + 16];
    size_t outlen;

    /* 1. Roundtrip. */
    CHECK(store_kv_set(base, "wifi.home", "secret", 6));
    CHECK(store_kv_size(base, "wifi.home") == 6);
    CHECK(store_kv_has(base, "wifi.home"));
    CHECK(store_kv_read(base, "wifi.home", buf, sizeof buf, &outlen));
    CHECK(outlen == 6 && memcmp(buf, "secret", 6) == 0);

    /* 2. NUL-safety: a value with an embedded NUL survives byte-for-byte (length-aware path). */
    const char nulval[8] = { 'a', 'b', 0, 'c', 'd', 0, 'e', 'f' };
    CHECK(store_kv_set(base, "blob", nulval, sizeof nulval));
    CHECK(store_kv_size(base, "blob") == (long)sizeof nulval);
    CHECK(store_kv_read(base, "blob", buf, sizeof buf, &outlen));
    CHECK(outlen == sizeof nulval && memcmp(buf, nulval, sizeof nulval) == 0);

    /* 3. Atomic replace: overwriting yields the new value, not a mix. */
    CHECK(store_kv_set(base, "wifi.home", "newpassword", 11));
    CHECK(store_kv_read(base, "wifi.home", buf, sizeof buf, &outlen));
    CHECK(outlen == 11 && memcmp(buf, "newpassword", 11) == 0);

    /* 4. Empty value is valid and distinct from "missing". */
    CHECK(store_kv_set(base, "empty", "", 0));
    CHECK(store_kv_has(base, "empty"));
    CHECK(store_kv_size(base, "empty") == 0);
    CHECK(store_kv_read(base, "empty", buf, sizeof buf, &outlen) && outlen == 0);

    /* 5. Key schema rejects (negative-probe path traversal / hidden / collision / length). */
    CHECK(!store_kv_set(base, "../etc/passwd", "x", 1));
    CHECK(!store_kv_set(base, "a/b", "x", 1));
    CHECK(!store_kv_set(base, "", "x", 1));
    CHECK(!store_kv_set(base, ".hidden", "x", 1));
    CHECK(!store_kv_set(base, ".tmp", "x", 1));        /* must not be able to address the staging dir */
    CHECK(!store_kv_set(base, "..", "x", 1));
    CHECK(!store_kv_set(base, "sp ace", "x", 1));
    char longkey[STORE_KEY_MAX + 5];
    memset(longkey, 'k', sizeof longkey - 1); longkey[sizeof longkey - 1] = 0;
    CHECK(!store_kv_set(base, longkey, "x", 1));        /* over STORE_KEY_MAX */
    CHECK(store_key_ok("a.b-c_D9"));                    /* the allowed charset is accepted */

    /* 6. Oversize value rejected, nothing written. */
    CHECK(!store_kv_set(base, "huge", buf, STORE_VAL_MAX + 1));
    CHECK(!store_kv_has(base, "huge"));

    /* 7. Missing key: read/size/has are clean failures, not crashes. */
    CHECK(!store_kv_has(base, "nope"));
    CHECK(store_kv_size(base, "nope") == -1);
    CHECK(!store_kv_read(base, "nope", buf, sizeof buf, &outlen));

    /* 8. Listing: sees the keys, NEVER the /.tmp/ staging dir; prefix filter works. */
    struct collect all = { 0 };
    int n = store_kv_each(base, NULL, collect_cb, &all);
    CHECK(n == all.n);
    CHECK(has_key(&all, "wifi.home") && has_key(&all, "blob") && has_key(&all, "empty"));
    CHECK(!has_key(&all, ".tmp"));                      /* staging dir is invisible to listing */
    struct collect pref = { 0 };
    store_kv_each(base, "wifi.", collect_cb, &pref);
    CHECK(pref.n == 1 && has_key(&pref, "wifi.home"));

    /* 9. Delete. */
    CHECK(store_kv_del(base, "blob"));
    CHECK(!store_kv_has(base, "blob"));
    CHECK(!store_kv_del(base, "blob"));                 /* deleting a missing key -> false */

    if (fails == 0) printf("test_store: ALL PASS\n");
    else            printf("test_store: %d FAIL\n", fails);
    return fails ? 1 : 0;
}

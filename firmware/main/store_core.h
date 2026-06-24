#ifndef STORE_CORE_H
#define STORE_CORE_H
/* Key-value store pure logic — POSIX file ops only, no Berry / no esp_littlefs, so it is
 * host-unit-testable against a real temp directory (the littlefs VFS emulates the same
 * POSIX surface on the device). The Berry bindings in store.c wrap these with FS_BASE.
 *
 * Layout: one file per key, flat under <base>/. Values are length-aware (NUL-safe). Writes
 * are atomic: write <base>/.tmp/<key> -> fsync -> close -> rename(<base>/.tmp/<key>, <base>/<key>).
 * A power loss leaves at most a stale <base>/.tmp/<key> (GC'd on mount), never a half key.
 */
#include <stdbool.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <dirent.h>
#include <sys/stat.h>

#define STORE_KEY_MAX 64        /* max key length; well under LFS_NAME_MAX (255) incl. the /.tmp/ prefix */
#define STORE_VAL_MAX 16384     /* max value size; a script loop must not fill the partition */
#define STORE_TMPDIR  ".tmp"    /* staging subdir under base; disjoint from the key charset */

/* Key schema (reject, never sanitize): [A-Za-z0-9._-], no '/', no leading '.', length 1..MAX.
 * This makes path traversal ("..", "/") and a collision with the /.tmp/ staging dir impossible. */
static bool store_key_ok(const char *key)
{
    if (!key) return false;
    size_t n = strlen(key);
    if (n < 1 || n > STORE_KEY_MAX) return false;
    if (key[0] == '.') return false;                 /* no leading dot -> excludes ".tmp", "..", hidden */
    for (size_t i = 0; i < n; i++) {
        char c = key[i];
        bool ok = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                  (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-';
        if (!ok) return false;                       /* rejects '/', whitespace, control, etc. */
    }
    return true;
}

/* Build "<base>/<sub>/<key>" (sub==NULL -> "<base>/<key>"). Truncation -> false (defensive;
 * key_ok already bounds the key, but never let a path silently truncate). */
static bool store__path(char *out, size_t cap, const char *base, const char *sub, const char *key)
{
    int n = sub ? snprintf(out, cap, "%s/%s/%s", base, sub, key)
                : snprintf(out, cap, "%s/%s", base, key);
    return n > 0 && (size_t)n < cap;
}

/* Atomic set. false on invalid key, oversize value, or any FS error (value untouched on failure). */
static bool store_kv_set(const char *base, const char *key, const void *val, size_t len)
{
    if (!store_key_ok(key) || len > STORE_VAL_MAX) return false;
    char tmp[160], fin[160];
    if (!store__path(tmp, sizeof tmp, base, STORE_TMPDIR, key)) return false;
    if (!store__path(fin, sizeof fin, base, NULL, key)) return false;

    int fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    if (fd < 0) return false;
    const char *p = (const char *)val;
    size_t left = len;
    while (left) {
        ssize_t w = write(fd, p, left);
        if (w <= 0) { close(fd); unlink(tmp); return false; }
        p += (size_t)w; left -= (size_t)w;
    }
    if (fsync(fd) != 0) { close(fd); unlink(tmp); return false; }   /* littlefs commits data only on sync */
    if (close(fd) != 0) { unlink(tmp); return false; }
    if (rename(tmp, fin) != 0) { unlink(tmp); return false; }       /* single metadata commit -> atomic */
    return true;
}

/* Size of the value for key, or -1 if missing/invalid. */
static long store_kv_size(const char *base, const char *key)
{
    if (!store_key_ok(key)) return -1;
    char fin[160];
    if (!store__path(fin, sizeof fin, base, NULL, key)) return -1;
    struct stat st;
    if (stat(fin, &st) != 0 || !S_ISREG(st.st_mode)) return -1;
    return (long)st.st_size;
}

/* Read up to cap bytes of key into buf; *outlen = bytes read. false if missing/invalid/too big for cap. */
static bool store_kv_read(const char *base, const char *key, void *buf, size_t cap, size_t *outlen)
{
    if (!store_key_ok(key)) return false;
    char fin[160];
    if (!store__path(fin, sizeof fin, base, NULL, key)) return false;
    int fd = open(fin, O_RDONLY);
    if (fd < 0) return false;
    size_t got = 0;
    char *p = (char *)buf;
    while (got < cap) {
        ssize_t r = read(fd, p + got, cap - got);
        if (r < 0) { close(fd); return false; }
        if (r == 0) break;          /* EOF */
        got += (size_t)r;
    }
    /* if there is still data left (file bigger than cap), treat as failure (caller's cap is the limit) */
    char probe;
    ssize_t more = read(fd, &probe, 1);
    close(fd);
    if (more > 0) return false;
    if (outlen) *outlen = got;
    return true;
}

static bool store_kv_has(const char *base, const char *key)
{
    return store_kv_size(base, key) >= 0;
}

static bool store_kv_del(const char *base, const char *key)
{
    if (!store_key_ok(key)) return false;
    char fin[160];
    if (!store__path(fin, sizeof fin, base, NULL, key)) return false;
    return unlink(fin) == 0;
}

/* Iterate valid keys under base (optionally filtered by prefix), invoking cb(key, ctx) for each.
 * Skips ".", "..", the /.tmp/ staging dir and anything failing key_ok. Returns the count, or -1. */
typedef void (*store_key_cb)(const char *key, void *ctx);
static int store_kv_each(const char *base, const char *prefix, store_key_cb cb, void *ctx)
{
    DIR *d = opendir(base);
    if (!d) return -1;
    struct dirent *e;
    int count = 0;
    size_t plen = prefix ? strlen(prefix) : 0;
    while ((e = readdir(d)) != NULL) {
        const char *name = e->d_name;
        if (!store_key_ok(name)) continue;                  /* skips ".", "..", ".tmp", invalid */
        if (plen && strncmp(name, prefix, plen) != 0) continue;
        if (cb) cb(name, ctx);
        count++;
    }
    closedir(d);
    return count;
}

#endif /* STORE_CORE_H */

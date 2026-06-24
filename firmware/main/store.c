/*
 * store — persistence layer mount.
 *
 * Mounts the "fs" littlefs partition and establishes the no-abort/degrade behaviour, the
 * mount-status flag, and the orphan temp-file GC. The key-value API (store_set/get/del/
 * has/keys) is built on top of this in a separate unit.
 */
#include "store.h"
#include "store_core.h"   /* pure POSIX KV logic (host-unit-tested) */
#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <dirent.h>
#include <sys/stat.h>
#include <unistd.h>
#include "esp_log.h"
#include "esp_littlefs.h"

static const char *TAG = "store";

#define FS_LABEL "fs"          /* mount by LABEL, not by a subtype scan */
#define FS_BASE  "/fs"         /* VFS mount point; Berry keys live flat under here */
#define FS_TMP   FS_BASE "/.tmp"   /* atomic-write staging dir (used by the KV layer); GC'd on mount */

static bool s_fs_ok = false;

bool store_fs_ok(void) { return s_fs_ok; }

/*
 * Remove orphan temp files left by an atomic write (write -> fsync -> rename) that was
 * cut short by power loss. littlefs does not reclaim visible files itself, so without
 * this they accumulate across power-loss events and eat the partition. The
 * staging dir is also (re)created here so the KV layer can rely on it existing.
 * The temp namespace "/.tmp/" is disjoint from the allowed key charset, so a Berry key
 * can never collide with it.
 */
static void gc_tmp(void)
{
    mkdir(FS_TMP, 0777);   /* ensure the staging dir exists (idempotent) */
    DIR *d = opendir(FS_TMP);
    if (!d) return;
    struct dirent *e;
    /* FS_TMP "/" + a full LFS_NAME_MAX (255) entry + NUL — sized so the worst case can't
     * truncate (the toolchain's -Werror=format-truncation proves it from these bounds). */
    char path[sizeof(FS_TMP) + 1 + 255 + 1];
    int n = 0;
    while ((e = readdir(d)) != NULL) {
        if (e->d_name[0] == '.' &&
            (e->d_name[1] == '\0' || (e->d_name[1] == '.' && e->d_name[2] == '\0')))
            continue;   /* skip "." / ".." */
        snprintf(path, sizeof(path), "%s/%s", FS_TMP, e->d_name);
        if (unlink(path) == 0) n++;
    }
    closedir(d);
    if (n) ESP_LOGW(TAG, "GC: removed %d orphan temp file(s)", n);
}

void store_mount(void)
{
    /* format_if_mount_failed=true: an empty or corrupt partition is (re)formatted so the
     * device recovers autonomously instead of hanging. NO ESP_ERROR_CHECK anywhere here —
     * a mount failure must degrade, never abort (no-abort invariant). */
    esp_vfs_littlefs_conf_t conf = {
        .base_path = FS_BASE,
        .partition_label = FS_LABEL,
        .format_if_mount_failed = true,
        .dont_mount = false,
    };

    esp_err_t err = esp_vfs_littlefs_register(&conf);
    if (err != ESP_OK) {
        /* ESP_ERR_NOT_FOUND -> no "fs" partition (device not yet table-reflashed): run
         * without the store. Any other error -> mount + format both failed: likewise
         * degrade. Either way the firmware continues; store_fs_ok() stays false. */
        ESP_LOGW(TAG, "littlefs mount failed (%s) -> store disabled, continuing autonomously",
                 esp_err_to_name(err));
        s_fs_ok = false;
        return;
    }

    size_t total = 0, used = 0;
    if (esp_littlefs_info(FS_LABEL, &total, &used) == ESP_OK)
        ESP_LOGI(TAG, "littlefs '%s' mounted at %s: %u/%u bytes used",
                 FS_LABEL, FS_BASE, (unsigned)used, (unsigned)total);
    else
        ESP_LOGI(TAG, "littlefs '%s' mounted at %s", FS_LABEL, FS_BASE);

    s_fs_ok = true;
    gc_tmp();
}

/* ------------------------------------------------------------------------- *
 * Console-facing raw helpers (no Berry) — used by the STORE console verbs.   *
 * They wrap the KV core with FS_BASE and honour s_fs_ok (never mount).       *
 * ------------------------------------------------------------------------- */

bool store_put(const char *key, const char *val)
{
    if (!s_fs_ok || !val) return false;
    return store_kv_set(FS_BASE, key, val, strlen(val));
}

/* Reads key into buf (NUL-terminated if it fits). Returns the byte length, or -1 if missing. */
long store_fetch(const char *key, char *buf, size_t cap)
{
    if (!s_fs_ok || cap == 0) return -1;
    long sz = store_kv_size(FS_BASE, key);
    if (sz < 0) return -1;
    size_t got = 0;
    size_t want = (size_t)sz < cap - 1 ? (size_t)sz : cap - 1;
    if (!store_kv_read(FS_BASE, key, buf, want, &got)) {
        /* value larger than buf: read what fits for display, still report it didn't fit */
        return sz;
    }
    buf[got] = '\0';
    return (long)got;
}

bool store_remove(const char *key)
{
    return s_fs_ok && store_kv_del(FS_BASE, key);
}

static void print_key_cb(const char *key, void *ctx)
{
    int *n = (int *)ctx;
    printf("  %s\r\n", key);
    (*n)++;
}

void store_list(void)
{
    if (!s_fs_ok) { printf("STORE unavailable (no fs)\r\n"); return; }
    int n = 0;
    store_kv_each(FS_BASE, NULL, print_key_cb, &n);
    printf("STORE %d key(s)\r\n", n);
}

/* ------------------------------------------------------------------------- *
 * Berry bindings (store_*) — hardened: arity + type gate at the entry, never *
 * be_toint/be_tostring on an unchecked slot (the dev.c:143 idiom). Values    *
 * are length-aware (be_tobytes/be_pushbytes) so embedded NULs survive.       *
 * ------------------------------------------------------------------------- */

static int l_store_set(bvm *vm)
{
    if (!s_fs_ok || be_top(vm) < 2 || !be_isstring(vm, 1)) { be_pushbool(vm, 0); be_return(vm); }
    const char *key = be_tostring(vm, 1);
    const void *val = NULL;
    size_t len = 0;
    if (be_isbytes(vm, 2)) {
        val = be_tobytes(vm, 2, &len);                 /* NUL-safe (blobs) */
    } else if (be_isstring(vm, 2)) {
        val = be_tostring(vm, 2);                       /* configs/creds/JSON: a C string */
        len = val ? strlen((const char *)val) : 0;
    } else {
        be_pushbool(vm, 0); be_return(vm);
    }
    be_pushbool(vm, store_kv_set(FS_BASE, key, val, len));
    be_return(vm);
}

static int l_store_get(bvm *vm)
{
    if (!s_fs_ok || be_top(vm) < 1 || !be_isstring(vm, 1)) be_return_nil(vm);
    const char *key = be_tostring(vm, 1);
    long sz = store_kv_size(FS_BASE, key);
    if (sz < 0 || sz > STORE_VAL_MAX) be_return_nil(vm);   /* missing, or absurd (set caps at MAX) */
    void *buf = malloc(sz ? (size_t)sz : 1);
    if (!buf) be_return_nil(vm);
    size_t got = 0;
    if (!store_kv_read(FS_BASE, key, buf, (size_t)sz, &got)) { free(buf); be_return_nil(vm); }
    be_pushbytes(vm, buf, got);                          /* length-aware -> NUL-safe */
    free(buf);
    be_return(vm);
}

static int l_store_has(bvm *vm)
{
    int r = (s_fs_ok && be_top(vm) >= 1 && be_isstring(vm, 1)) ? store_kv_has(FS_BASE, be_tostring(vm, 1)) : 0;
    be_pushbool(vm, r);
    be_return(vm);
}

static int l_store_del(bvm *vm)
{
    int r = (s_fs_ok && be_top(vm) >= 1 && be_isstring(vm, 1)) ? store_kv_del(FS_BASE, be_tostring(vm, 1)) : 0;
    be_pushbool(vm, r);
    be_return(vm);
}

static int l_store_ok(bvm *vm)
{
    be_pushbool(vm, s_fs_ok);
    be_return(vm);
}

static void keys_push_cb(const char *key, void *ctx)
{
    bvm *vm = (bvm *)ctx;
    be_pushstring(vm, key);   /* item on top, the list is just below at index -2 */
    be_data_push(vm, -2);     /* append the item to the list */
    be_pop(vm, 1);            /* drop the item, leaving the list on top */
}

static int l_store_keys(bvm *vm)
{
    const char *prefix = (be_top(vm) >= 1 && be_isstring(vm, 1)) ? be_tostring(vm, 1) : NULL;
    be_newlist(vm);           /* the result list, now on top of the stack */
    if (s_fs_ok)
        store_kv_each(FS_BASE, prefix, keys_push_cb, vm);
    be_return(vm);            /* return the (possibly empty) list */
}

void store_register(bvm *vm)
{
    be_regfunc(vm, "store_set",  l_store_set);
    be_regfunc(vm, "store_get",  l_store_get);
    be_regfunc(vm, "store_del",  l_store_del);
    be_regfunc(vm, "store_has",  l_store_has);
    be_regfunc(vm, "store_keys", l_store_keys);
    be_regfunc(vm, "store_ok",   l_store_ok);
}

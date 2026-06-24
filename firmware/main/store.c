/*
 * store — persistence layer mount.
 *
 * Mounts the "fs" littlefs partition and establishes the no-abort/degrade behaviour, the
 * mount-status flag, and the orphan temp-file GC. The key-value API (store_set/get/del/
 * has/keys) is built on top of this in a separate unit.
 */
#include "store.h"
#include <string.h>
#include <stdio.h>
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

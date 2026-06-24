#pragma once
/*
 * store — persistence layer: the "fs" littlefs partition that backs the Berry-owned
 * key-value / blob store. This unit provides only the mount; the key-value API is added
 * on top of it separately.
 *
 * Invariants:
 *   - store_mount() MUST be called AFTER guard_check_and_run() and MUST NOT abort:
 *     a filesystem defect must not mask the bootloop guard (that would brick the device).
 *   - If the "fs" partition is absent (a device not yet partition-reflashed) or unmountable,
 *     the mount degrades to store_fs_ok()==false and the device keeps running autonomously.
 *   - The littlefs namespace is Berry-only; firmware state stays in NVS.
 */

#include <stdbool.h>
#include <stddef.h>
#include "berry.h"

/* Mount the "fs" littlefs partition (by label). No-abort; sets the internal mount flag.
 * Call exactly once, strictly after the bootloop guard, before any Berry VM. */
void store_mount(void);

/* True iff the filesystem is mounted and writable (separates "key absent" from "FS gone"). */
bool store_fs_ok(void);

/* Register the Berry key-value bindings (store_set/get/del/has/keys/ok) on a VM,
 * like fb_register/dev_register. */
void store_register(bvm *vm);

/* Console-facing raw helpers (no Berry) for the STORE verbs. All honour store_fs_ok()
 * and never mount. store_fetch: NUL-terminates buf if it fits, returns byte length or -1. */
bool store_put(const char *key, const char *val);
long store_fetch(const char *key, char *buf, size_t cap);
bool store_remove(const char *key);
void store_list(void);

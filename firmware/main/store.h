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

/* Mount the "fs" littlefs partition (by label). No-abort; sets the internal mount flag.
 * Call exactly once, strictly after the bootloop guard, before any Berry VM. */
void store_mount(void);

/* True iff the filesystem is mounted and writable (separates "key absent" from "FS gone"). */
bool store_fs_ok(void);

// The Zecurity client core (libzecurity_ffi, client/ffi) for iOS.
//
// zc_start_daemon is for the packet tunnel extension. The zc_* UI functions
// are called by the Flutter app through dart:ffi; they are declared here so
// the Runner can reference them, which keeps the linker from dropping them.
// Every char* a zc_* function returns is JSON, freed with zc_free.
#pragma once
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/// Tunnel extension: apply network settings routing exactly these IPs
/// ("ip,ip,...", one /32 each); return 0 once applied.
typedef int32_t (*zc_establish_fn)(const char *routes);
/// Tunnel extension: apply network settings with no resource routes.
typedef void (*zc_release_fn)(void);

/// Tunnel extension: starts the client daemon (once per process) with the
/// App Group container as its files directory. 0 on success.
int32_t zc_start_daemon(const char *files_dir, const char *os_version,
                        zc_establish_fn establish, zc_release_fn release);

// UI process (dart:ffi).
char *zc_init(const char *files_dir);
char *zc_config(void);
char *zc_setup(const char *args_json);
char *zc_request(const char *request_json);
char *zc_login_start(void);
char *zc_login_poll(void);
char *zc_logout(void);
void zc_free(char *p);

#ifdef __cplusplus
}
#endif

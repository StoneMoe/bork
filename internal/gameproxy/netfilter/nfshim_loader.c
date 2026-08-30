//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"

static SRWLOCK api_lock = SRWLOCK_INIT;
static HMODULE module;
static bork_nf_api loaded_api;
static char *loaded_path;
static int loaded_path_len;
static uint64_t owner_token;
static int pinned;

static FARPROC load_symbol(HMODULE handle, const char *name) {
    return GetProcAddress(handle, name);
}

static int resolve_api(HMODULE handle, bork_nf_api *api) {
#define BORK_LOAD(field, type, name) do { \
    api->field = (type)load_symbol(handle, name); \
    if (api->field == NULL) return 0; \
} while (0)
    BORK_LOAD(set_options, bork_set_options_fn, "nf_setOptions");
    BORK_LOAD(init, bork_init_fn, "nf_init");
    BORK_LOAD(free, bork_free_fn, "nf_free");
    BORK_LOAD(set_rules, bork_set_rules_fn, "nf_setRulesEx");
    BORK_LOAD(tcp_post_receive, bork_tcp_post_receive_fn, "nf_tcpPostReceive");
    BORK_LOAD(tcp_close, bork_tcp_close_fn, "nf_tcpClose");
    BORK_LOAD(udp_post_receive, bork_udp_post_receive_fn, "nf_udpPostReceive");
    BORK_LOAD(udp_state, bork_udp_state_fn, "nf_udpSetConnectionState");
    BORK_LOAD(get_udp_info, bork_get_udp_info_fn, "nf_getUDPConnInfo");
    BORK_LOAD(get_process_name, bork_get_process_name_fn, "nf_getProcessNameW");
#undef BORK_LOAD
    return 1;
}

static wchar_t *utf8_path(const char *path, int length) {
    int units = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, length, NULL, 0);
    if (units <= 0) return NULL;
    wchar_t *wide = HeapAlloc(GetProcessHeap(), 0, (size_t)(units + 1) * sizeof(wchar_t));
    if (wide == NULL) return NULL;
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, length, wide, units) != units) {
        HeapFree(GetProcessHeap(), 0, wide);
        return NULL;
    }
    wide[units] = L'\0';
    return wide;
}

static void abandon_owner(uint64_t token) {
    AcquireSRWLockExclusive(&api_lock);
    if (owner_token == token) owner_token = 0;
    ReleaseSRWLockExclusive(&api_lock);
}

int32_t bork_api_acquire(uint64_t token, const char *path, int path_len,
                         const char *normalized, int normalized_len, uint32_t *system_error) {
    AcquireSRWLockExclusive(&api_lock);
    if (owner_token != 0) {
        ReleaseSRWLockExclusive(&api_lock);
        return BORK_STATUS_OWNER_BUSY;
    }
    if (module != NULL) {
        if (loaded_path_len != normalized_len || memcmp(loaded_path, normalized, (size_t)normalized_len) != 0) {
            ReleaseSRWLockExclusive(&api_lock);
            return BORK_STATUS_DLL_MISMATCH;
        }
        owner_token = token;
        ReleaseSRWLockExclusive(&api_lock);
        return NF_STATUS_SUCCESS;
    }
    owner_token = token;
    ReleaseSRWLockExclusive(&api_lock);

    wchar_t *wide = utf8_path(path, path_len);
    if (wide == NULL) {
        *system_error = GetLastError();
        abandon_owner(token);
        return BORK_STATUS_CONVERSION;
    }
    HMODULE handle = LoadLibraryExW(wide, NULL, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
    HeapFree(GetProcessHeap(), 0, wide);
    if (handle == NULL) {
        *system_error = GetLastError();
        abandon_owner(token);
        return BORK_STATUS_LOAD_LIBRARY;
    }
    bork_nf_api api = {0};
    if (!resolve_api(handle, &api)) {
        *system_error = GetLastError();
        FreeLibrary(handle);
        abandon_owner(token);
        return BORK_STATUS_SYMBOL;
    }
    char *path_copy = HeapAlloc(GetProcessHeap(), 0, (size_t)normalized_len);
    if (path_copy == NULL) {
        *system_error = ERROR_NOT_ENOUGH_MEMORY;
        FreeLibrary(handle);
        abandon_owner(token);
        return BORK_STATUS_LOAD_LIBRARY;
    }
    memcpy(path_copy, normalized, (size_t)normalized_len);
    AcquireSRWLockExclusive(&api_lock);
    module = handle;
    loaded_api = api;
    loaded_path = path_copy;
    loaded_path_len = normalized_len;
    ReleaseSRWLockExclusive(&api_lock);
    return NF_STATUS_SUCCESS;
}

void bork_api_note_init_attempt(uint64_t token) {
    AcquireSRWLockExclusive(&api_lock);
    if (owner_token == token) pinned = 1;
    ReleaseSRWLockExclusive(&api_lock);
}

int bork_api_snapshot(uint64_t token, bork_nf_api *api) {
    AcquireSRWLockShared(&api_lock);
    int owned = owner_token == token && module != NULL;
    if (owned) *api = loaded_api;
    ReleaseSRWLockShared(&api_lock);
    return owned;
}

int bork_api_is_owner(uint64_t token) {
    AcquireSRWLockShared(&api_lock);
    int owned = owner_token == token;
    ReleaseSRWLockShared(&api_lock);
    return owned;
}

void bork_api_release(uint64_t token) {
    AcquireSRWLockExclusive(&api_lock);
    if (owner_token == token) {
        owner_token = 0;
        if (!pinned && module != NULL) {
            FreeLibrary(module);
            module = NULL;
            memset(&loaded_api, 0, sizeof(loaded_api));
            HeapFree(GetProcessHeap(), 0, loaded_path);
            loaded_path = NULL;
            loaded_path_len = 0;
        }
    }
    ReleaseSRWLockExclusive(&api_lock);
}

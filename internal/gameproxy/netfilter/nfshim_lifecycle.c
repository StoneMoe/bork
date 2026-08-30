//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"

extern NF_STATUS bork_nf_apply_rules(uint64_t token, const bork_nf_rules *rules);

static SRWLOCK callback_lock = SRWLOCK_INIT;
static CONDITION_VARIABLE callback_idle = CONDITION_VARIABLE_INIT;
static uint64_t callback_token;
static uint64_t active_callbacks;
static uint64_t entered_callbacks;
static uint64_t exited_callbacks;
static uint64_t rejected_callbacks;
static int callback_admission;

void bork_callback_install(uint64_t token) {
    AcquireSRWLockExclusive(&callback_lock);
    callback_token = token;
    callback_admission = 1;
    ReleaseSRWLockExclusive(&callback_lock);
}

int bork_callback_enter(uint64_t *token) {
    AcquireSRWLockExclusive(&callback_lock);
    if (!callback_admission || callback_token == 0) {
        rejected_callbacks++;
        ReleaseSRWLockExclusive(&callback_lock);
        return 0;
    }
    active_callbacks++;
    entered_callbacks++;
    *token = callback_token;
    ReleaseSRWLockExclusive(&callback_lock);
    return 1;
}

void bork_callback_exit(void) {
    AcquireSRWLockExclusive(&callback_lock);
    active_callbacks--;
    exited_callbacks++;
    if (active_callbacks == 0) WakeAllConditionVariable(&callback_idle);
    ReleaseSRWLockExclusive(&callback_lock);
}

void bork_nf_callbacks_disable(uint64_t token) {
    AcquireSRWLockExclusive(&callback_lock);
    if (callback_token == token) callback_admission = 0;
    ReleaseSRWLockExclusive(&callback_lock);
}

void bork_nf_callbacks_drain(uint64_t token) {
    AcquireSRWLockExclusive(&callback_lock);
    while (callback_token == token && active_callbacks != 0) {
        SleepConditionVariableSRW(&callback_idle, &callback_lock, INFINITE, 0);
    }
    ReleaseSRWLockExclusive(&callback_lock);
}

void bork_callback_clear(uint64_t token) {
    AcquireSRWLockExclusive(&callback_lock);
    if (callback_token == token && active_callbacks == 0) {
        callback_token = 0;
        callback_admission = 0;
    }
    ReleaseSRWLockExclusive(&callback_lock);
}

bork_nf_callback_stats bork_nf_callbacks_stats(void) {
    AcquireSRWLockShared(&callback_lock);
    bork_nf_callback_stats stats = {entered_callbacks, exited_callbacks, rejected_callbacks};
    ReleaseSRWLockShared(&callback_lock);
    return stats;
}

bork_nf_start_result bork_nf_start(uint64_t token,
                                   const char *dll_path, int dll_path_len,
                                   const char *normalized_path, int normalized_path_len,
                                   const char *driver_name, int driver_name_len,
                                   const bork_nf_rules *rules) {
    bork_nf_start_result result = {NF_STATUS_FAIL, 0, 0, 0};
    result.status = bork_api_acquire(token, dll_path, dll_path_len,
                                     normalized_path, normalized_path_len, &result.system_error);
    if (result.status != NF_STATUS_SUCCESS) return result;
    bork_callback_install(token);
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        result.status = NF_STATUS_NOT_INITIALIZED;
        return result;
    }
    char *driver = HeapAlloc(GetProcessHeap(), 0, (size_t)driver_name_len + 1);
    if (driver == NULL) {
        result.system_error = ERROR_NOT_ENOUGH_MEMORY;
        result.status = BORK_STATUS_CONVERSION;
        return result;
    }
    memcpy(driver, driver_name, (size_t)driver_name_len);
    driver[driver_name_len] = '\0';
    api.set_options(1, NFF_DISABLE_AUTO_REGISTER);
    bork_api_note_init_attempt(token);
    result.init_attempted = 1;
    result.status = api.init(driver, &bork_nf_event_handler);
    HeapFree(GetProcessHeap(), 0, driver);
    if (result.status != NF_STATUS_SUCCESS) return result;
    result.init_succeeded = 1;
    result.status = bork_nf_apply_rules(token, rules);
    return result;
}

void bork_nf_shutdown(uint64_t token, int call_free) {
    if (!bork_api_is_owner(token)) return;
    bork_nf_api api;
    if (call_free && bork_api_snapshot(token, &api)) api.free();
    bork_callback_clear(token);
    bork_api_release(token);
}

static void NFAPI_CC callback_thread_start(void) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

static void NFAPI_CC callback_thread_end(void) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

extern void NFAPI_CC bork_tcp_connect_request(ENDPOINT_ID, PNF_TCP_CONN_INFO);
extern void NFAPI_CC bork_tcp_connected(ENDPOINT_ID, PNF_TCP_CONN_INFO);
extern void NFAPI_CC bork_tcp_closed(ENDPOINT_ID, PNF_TCP_CONN_INFO);
extern void NFAPI_CC bork_tcp_receive(ENDPOINT_ID, const char *, int);
extern void NFAPI_CC bork_tcp_send(ENDPOINT_ID, const char *, int);
extern void NFAPI_CC bork_tcp_can_receive(ENDPOINT_ID);
extern void NFAPI_CC bork_tcp_can_send(ENDPOINT_ID);
extern void NFAPI_CC bork_udp_created(ENDPOINT_ID, PNF_UDP_CONN_INFO);
extern void NFAPI_CC bork_udp_connect_request(ENDPOINT_ID, PNF_UDP_CONN_REQUEST);
extern void NFAPI_CC bork_udp_closed(ENDPOINT_ID, PNF_UDP_CONN_INFO);
extern void NFAPI_CC bork_udp_receive(ENDPOINT_ID, const unsigned char *, const char *, int, PNF_UDP_OPTIONS);
extern void NFAPI_CC bork_udp_send(ENDPOINT_ID, const unsigned char *, const char *, int, PNF_UDP_OPTIONS);
extern void NFAPI_CC bork_udp_can_receive(ENDPOINT_ID);
extern void NFAPI_CC bork_udp_can_send(ENDPOINT_ID);

NF_EventHandler bork_nf_event_handler = {
    callback_thread_start, callback_thread_end,
    bork_tcp_connect_request, bork_tcp_connected, bork_tcp_closed,
    bork_tcp_receive, bork_tcp_send, bork_tcp_can_receive, bork_tcp_can_send,
    bork_udp_created, bork_udp_connect_request, bork_udp_closed,
    bork_udp_receive, bork_udp_send, bork_udp_can_receive, bork_udp_can_send
};

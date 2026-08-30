//go:build windows && amd64 && cgo && netfilter_sdk

#ifndef BORK_NFSHIM_INTERNAL_H
#define BORK_NFSHIM_INTERNAL_H

#define WIN32_LEAN_AND_MEAN
#define _WIN32_WINNT 0x0602
#include <winsock2.h>
#include <windows.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define _C_API
#define _NFAPI_STATIC_LIB
#include "sdk/nfsdk/wfp/include/nfapi.h"
#include "nfshim.h"

_Static_assert(sizeof(NF_RULE_EX) == 643, "unexpected NF_RULE_EX ABI");
_Static_assert(offsetof(NF_RULE_EX, localProxyProcessId) == 639, "unexpected NF_RULE_EX layout");
_Static_assert(sizeof(NF_TCP_CONN_INFO) == 67, "unexpected NF_TCP_CONN_INFO ABI");
_Static_assert(sizeof(NF_UDP_CONN_INFO) == 34, "unexpected NF_UDP_CONN_INFO ABI");
_Static_assert(sizeof(NF_EventHandler) == 128, "unexpected NF_EventHandler ABI");

enum {
    BORK_STATUS_LOAD_LIBRARY = -1000,
    BORK_STATUS_SYMBOL = -1001,
    BORK_STATUS_OWNER_BUSY = -1002,
    BORK_STATUS_DLL_MISMATCH = -1003,
    BORK_STATUS_CONVERSION = -1004
};

enum bork_nf_event {
    BORK_EVENT_TCP_CONNECT_REQUEST = 1,
    BORK_EVENT_TCP_CONNECTED,
    BORK_EVENT_TCP_RECEIVE,
    BORK_EVENT_TCP_SEND,
    BORK_EVENT_UDP_CREATED,
    BORK_EVENT_UDP_CONNECT_REQUEST,
    BORK_EVENT_UDP_RECEIVE,
    BORK_EVENT_UDP_SEND,
    BORK_EVENT_TCP_CLOSED,
    BORK_EVENT_UDP_CLOSED
};

enum bork_nf_reason {
    BORK_REASON_UNSUPPORTED = 1,
    BORK_REASON_INCOMING_TCP,
    BORK_REASON_IPV6,
    BORK_REASON_MALFORMED,
    BORK_REASON_PROCESS_PATH,
    BORK_REASON_UDP_QUERY
};

typedef void (NFAPI_CC *bork_set_options_fn)(DWORD, DWORD);
typedef NF_STATUS (NFAPI_CC *bork_init_fn)(const char *, NF_EventHandler *);
typedef void (NFAPI_CC *bork_free_fn)(void);
typedef NF_STATUS (NFAPI_CC *bork_set_rules_fn)(PNF_RULE_EX, int);
typedef NF_STATUS (NFAPI_CC *bork_tcp_post_receive_fn)(ENDPOINT_ID, const char *, int);
typedef NF_STATUS (NFAPI_CC *bork_tcp_close_fn)(ENDPOINT_ID);
typedef NF_STATUS (NFAPI_CC *bork_udp_post_receive_fn)(ENDPOINT_ID, const unsigned char *, const char *, int, PNF_UDP_OPTIONS);
typedef NF_STATUS (NFAPI_CC *bork_udp_state_fn)(ENDPOINT_ID, int);
typedef NF_STATUS (NFAPI_CC *bork_get_udp_info_fn)(ENDPOINT_ID, PNF_UDP_CONN_INFO);
typedef BOOL (NFAPI_CC *bork_get_process_name_fn)(DWORD, wchar_t *, DWORD);

typedef struct bork_nf_api {
    bork_set_options_fn set_options;
    bork_init_fn init;
    bork_free_fn free;
    bork_set_rules_fn set_rules;
    bork_tcp_post_receive_fn tcp_post_receive;
    bork_tcp_close_fn tcp_close;
    bork_udp_post_receive_fn udp_post_receive;
    bork_udp_state_fn udp_state;
    bork_get_udp_info_fn get_udp_info;
    bork_get_process_name_fn get_process_name;
} bork_nf_api;

typedef struct bork_ipv4_endpoint {
    uint8_t address[4];
    uint16_t port;
} bork_ipv4_endpoint;

int32_t bork_api_acquire(uint64_t token, const char *path, int path_len,
                         const char *normalized, int normalized_len, uint32_t *system_error);
void bork_api_note_init_attempt(uint64_t token);
int bork_api_snapshot(uint64_t token, bork_nf_api *api);
int bork_api_is_owner(uint64_t token);
void bork_api_release(uint64_t token);

void bork_callback_install(uint64_t token);
int bork_callback_enter(uint64_t *token);
void bork_callback_exit(void);
void bork_callback_clear(uint64_t token);

int bork_copy_tcp_info(const NF_TCP_CONN_INFO *packed, DWORD *pid, uint8_t *direction,
                       uint16_t *family, bork_ipv4_endpoint *local,
                       bork_ipv4_endpoint *remote);
int bork_copy_udp_info(const NF_UDP_CONN_INFO *packed, DWORD *pid,
                       uint16_t *family, bork_ipv4_endpoint *local);
int bork_copy_sockaddr(const unsigned char *raw, bork_ipv4_endpoint *endpoint);
int bork_process_path(uint64_t token, DWORD pid, char *utf8, int capacity, int *length);
void bork_fatal_tcp(uint64_t token, int event, uint64_t id, int reason, int32_t status);
void bork_fatal_udp(uint64_t token, int event, uint64_t id, int reason, int32_t status);

extern NF_EventHandler bork_nf_event_handler;

#endif

//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"
#include "_cgo_export.h"

void NFAPI_CC bork_udp_created(ENDPOINT_ID id, PNF_UDP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    DWORD pid = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    if (!bork_copy_udp_info(info, &pid, &family, &local) || family != AF_INET) {
        bork_fatal_udp(token, BORK_EVENT_UDP_CREATED, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    int unspecified = local.address[0] == 0 && local.address[1] == 0 &&
                      local.address[2] == 0 && local.address[3] == 0;
    if (local.port == 0 && !unspecified) {
        bork_fatal_udp(token, BORK_EVENT_UDP_CREATED, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    char path[MAX_PATH * 4];
    int path_len = 0;
    if (!bork_process_path(token, pid, path, sizeof(path), &path_len)) {
        bork_fatal_udp(token, BORK_EVENT_UDP_CREATED, id, BORK_REASON_PROCESS_PATH, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFUDPCreated(token, id, pid, path, path_len,
                   local.address[0], local.address[1], local.address[2], local.address[3], local.port);
    bork_callback_exit();
}

void NFAPI_CC bork_udp_connect_request(ENDPOINT_ID id, PNF_UDP_CONN_REQUEST request) {
    (void)request;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_fatal_udp(token, BORK_EVENT_UDP_CONNECT_REQUEST, id, BORK_REASON_UNSUPPORTED, NF_STATUS_SUCCESS);
    bork_callback_exit();
}

void NFAPI_CC bork_udp_closed(ENDPOINT_ID id, PNF_UDP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    DWORD pid = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    if (!bork_copy_udp_info(info, &pid, &family, &local) || family != AF_INET) {
        bork_fatal_udp(token, BORK_EVENT_UDP_CLOSED, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFUDPClosed(token, id);
    bork_callback_exit();
}

void NFAPI_CC bork_udp_receive(ENDPOINT_ID id, const unsigned char *remote,
                               const char *data, int length, PNF_UDP_OPTIONS options) {
    (void)remote;
    (void)options;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    int malformed = length < 0 || length > 65507 || (length > 0 && data == NULL);
    bork_fatal_udp(token, BORK_EVENT_UDP_RECEIVE, id,
                   malformed ? BORK_REASON_MALFORMED : BORK_REASON_UNSUPPORTED,
                   NF_STATUS_SUCCESS);
    bork_callback_exit();
}

void NFAPI_CC bork_udp_send(ENDPOINT_ID id, const unsigned char *remote_raw,
                            const char *data, int length, PNF_UDP_OPTIONS options) {
    (void)options;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    if (length < 0 || length > 65507 || (length > 0 && data == NULL)) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    bork_nf_api api;
    NF_UDP_CONN_INFO info;
    if (!bork_api_snapshot(token, &api)) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_UDP_QUERY, NF_STATUS_NOT_INITIALIZED);
        bork_callback_exit();
        return;
    }
    NF_STATUS query_status = api.get_udp_info(id, &info);
    if (query_status != NF_STATUS_SUCCESS) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_UDP_QUERY, query_status);
        bork_callback_exit();
        return;
    }
    DWORD pid = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    bork_ipv4_endpoint remote = {0};
    if (!bork_copy_udp_info(&info, &pid, &family, &local) ||
        !bork_copy_sockaddr(remote_raw, &remote) || family != AF_INET) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (local.port == 0 || remote.port == 0) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFUDPSend(token, id,
                local.address[0], local.address[1], local.address[2], local.address[3], local.port,
                remote.address[0], remote.address[1], remote.address[2], remote.address[3], remote.port,
                data, length);
    bork_callback_exit();
}

void NFAPI_CC bork_udp_can_receive(ENDPOINT_ID id) {
    (void)id;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

void NFAPI_CC bork_udp_can_send(ENDPOINT_ID id) {
    (void)id;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

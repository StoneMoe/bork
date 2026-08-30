//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"
#include "_cgo_export.h"

void NFAPI_CC bork_tcp_connect_request(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    (void)info;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id, BORK_REASON_UNSUPPORTED, NF_STATUS_SUCCESS);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_connected(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    DWORD pid = 0;
    uint8_t direction = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    bork_ipv4_endpoint remote = {0};
    if (!bork_copy_tcp_info(info, &pid, &direction, &family, &local, &remote)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECTED, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (family != AF_INET) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECTED, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (direction != NF_D_OUT) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECTED, id, BORK_REASON_INCOMING_TCP, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (local.port == 0 || remote.port == 0) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECTED, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    char path[MAX_PATH * 4];
    int path_len = 0;
    if (!bork_process_path(token, pid, path, sizeof(path), &path_len)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECTED, id, BORK_REASON_PROCESS_PATH, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFTCPConnected(token, id, pid, path, path_len,
                     local.address[0], local.address[1], local.address[2], local.address[3], local.port,
                     remote.address[0], remote.address[1], remote.address[2], remote.address[3], remote.port);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_closed(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    DWORD pid = 0;
    uint8_t direction = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    bork_ipv4_endpoint remote = {0};
    if (!bork_copy_tcp_info(info, &pid, &direction, &family, &local, &remote) || family != AF_INET) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CLOSED, id,
                       family == AF_INET6 ? BORK_REASON_IPV6 : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (direction != NF_D_OUT) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_CLOSED, id, BORK_REASON_INCOMING_TCP, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFTCPClosed(token, id);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_receive(ENDPOINT_ID id, const char *data, int length) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    int malformed = length < 0 || length > NF_TCP_PACKET_BUF_SIZE || (length > 0 && data == NULL);
    bork_fatal_tcp(token, BORK_EVENT_TCP_RECEIVE, id,
                   malformed ? BORK_REASON_MALFORMED : BORK_REASON_UNSUPPORTED,
                   NF_STATUS_SUCCESS);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_send(ENDPOINT_ID id, const char *data, int length) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    if (length < 0 || length > NF_TCP_PACKET_BUF_SIZE || (length > 0 && data == NULL)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    goNFTCPSend(token, id, data, length);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_can_receive(ENDPOINT_ID id) {
    (void)id;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_can_send(ENDPOINT_ID id) {
    (void)id;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    bork_callback_exit();
}

//go:build windows && amd64 && cgo && game_proxy

#include "nfshim_internal.h"
#include "_cgo_export.h"

static void bork_block_tcp_connect(PNF_TCP_CONN_INFO info) {
    if (info == NULL) return;
    uint32_t blocked = NF_BLOCK;
    memcpy((unsigned char *)info + offsetof(NF_TCP_CONN_INFO, filteringFlag), &blocked, sizeof(blocked));
}

static void bork_bypass_tcp(uint64_t token, int event, ENDPOINT_ID id, int reason) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        goNFFatal(token, event, id, reason, NF_STATUS_NOT_INITIALIZED, NF_STATUS_NOT_INITIALIZED);
        return;
    }
    NF_STATUS status = api.tcp_disable_filtering(id);
    if (status == NF_STATUS_SUCCESS) return;
    NF_STATUS cleanup = api.tcp_disable_filtering(id);
    if (cleanup != NF_STATUS_SUCCESS) goNFFatal(token, event, id, reason, status, cleanup);
}

void NFAPI_CC bork_tcp_connect_request(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    uint32_t filtering = 0;
    DWORD pid = 0;
    uint8_t direction = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    bork_ipv4_endpoint remote = {0};
    if (!bork_copy_tcp_info(info, &filtering, &pid, &direction, &family, &local, &remote)) {
        if (family == AF_INET6) {
            bork_bypass_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id, BORK_REASON_IPV6);
            bork_callback_exit();
            return;
        }
        bork_block_tcp_connect(info);
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (direction != NF_D_OUT || local.port == 0 || remote.port == 0) {
        bork_block_tcp_connect(info);
        bork_fatal_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id,
                       direction != NF_D_OUT ? BORK_REASON_INCOMING_TCP : BORK_REASON_MALFORMED,
                       NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (pid == GetCurrentProcessId()) {
        bork_bypass_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id, BORK_REASON_SELF_INTERCEPTION);
        bork_callback_exit();
        return;
    }
    if (remote.address[0] == 127) {
        bork_bypass_tcp(token, BORK_EVENT_TCP_CONNECT_REQUEST, id, BORK_REASON_LOCAL_ROUTE);
        bork_callback_exit();
        return;
    }
    char path[MAX_PATH * 4] = {0};
    int path_len = 0;
    (void)bork_process_path(token, pid, path, sizeof(path), &path_len);
    uint16_t redirect_port = goNFTCPConnectRequest(token, id, pid, path, path_len,
                                                    local.address[0], local.address[1], local.address[2], local.address[3], local.port,
                                                    remote.address[0], remote.address[1], remote.address[2], remote.address[3], remote.port);
    if (redirect_port == 0) {
        bork_block_tcp_connect(info);
        bork_callback_exit();
        return;
    }
    struct sockaddr_in redirect;
    memset(&redirect, 0, sizeof(redirect));
    redirect.sin_family = AF_INET;
    redirect.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    redirect.sin_port = htons(redirect_port);
    memset(info->remoteAddress, 0, NF_MAX_ADDRESS_LENGTH);
    memcpy(info->remoteAddress, &redirect, sizeof(redirect));
    DWORD proxy_pid = GetCurrentProcessId();
    memcpy((unsigned char *)info + offsetof(NF_TCP_CONN_INFO, processId), &proxy_pid, sizeof(proxy_pid));
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_connected(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    (void)id;
    (void)info;
}

void NFAPI_CC bork_tcp_closed(ENDPOINT_ID id, PNF_TCP_CONN_INFO info) {
    (void)info;
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    goNFTCPClosed(token, id);
    bork_callback_exit();
}

void NFAPI_CC bork_tcp_receive(ENDPOINT_ID id, const char *data, int length) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    if (length < 0 || length > NF_TCP_PACKET_BUF_SIZE || (length > 0 && data == NULL)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_RECEIVE, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_RECEIVE, id, BORK_REASON_POST_RECEIVE, NF_STATUS_NOT_INITIALIZED);
        bork_callback_exit();
        return;
    }
    NF_STATUS status = api.tcp_post_receive(id, data, length);
    if (status != NF_STATUS_SUCCESS) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_RECEIVE, id, BORK_REASON_POST_RECEIVE, status);
    }
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
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_SEND, id, BORK_REASON_POST_SEND, NF_STATUS_NOT_INITIALIZED);
        bork_callback_exit();
        return;
    }
    NF_STATUS status = api.tcp_post_send(id, data, length);
    if (status != NF_STATUS_SUCCESS) {
        bork_fatal_tcp(token, BORK_EVENT_TCP_SEND, id, BORK_REASON_POST_SEND, status);
    }
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

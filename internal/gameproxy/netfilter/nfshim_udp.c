//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"
#include "_cgo_export.h"

static void bork_bypass_udp(uint64_t token, int event, ENDPOINT_ID id, int reason) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        goNFFatal(token, event, id, reason, NF_STATUS_NOT_INITIALIZED, NF_STATUS_NOT_INITIALIZED);
        return;
    }
    NF_STATUS status = api.udp_disable_filtering(id);
    if (status == NF_STATUS_SUCCESS) return;
    NF_STATUS cleanup = api.udp_disable_filtering(id);
    if (cleanup != NF_STATUS_SUCCESS) goNFFatal(token, event, id, reason, status, cleanup);
}

void NFAPI_CC bork_udp_created(ENDPOINT_ID id, PNF_UDP_CONN_INFO info) {
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    DWORD pid = 0;
    uint16_t family = 0;
    bork_ipv4_endpoint local = {0};
    if (!bork_copy_udp_info(info, &pid, &family, &local) || family != AF_INET) {
        if (family == AF_INET6) {
            bork_bypass_udp(token, BORK_EVENT_UDP_CREATED, id, BORK_REASON_IPV6);
            bork_callback_exit();
            return;
        }
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
    if (pid == GetCurrentProcessId()) {
        bork_bypass_udp(token, BORK_EVENT_UDP_CREATED, id, BORK_REASON_SELF_INTERCEPTION);
        bork_callback_exit();
        return;
    }
    char path[MAX_PATH * 4] = {0};
    int path_len = 0;
    (void)bork_process_path(token, pid, path, sizeof(path), &path_len);
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
        if (family == AF_INET6) {
            bork_bypass_udp(token, BORK_EVENT_UDP_CLOSED, id, BORK_REASON_IPV6);
            bork_callback_exit();
            return;
        }
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
    uint64_t token;
    if (!bork_callback_enter(&token)) return;
    if (remote == NULL || length < 0 || length > 65507 || (length > 0 && data == NULL)) {
        bork_fatal_udp(token, BORK_EVENT_UDP_RECEIVE, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) {
        bork_fatal_udp(token, BORK_EVENT_UDP_RECEIVE, id, BORK_REASON_POST_RECEIVE, NF_STATUS_NOT_INITIALIZED);
        bork_callback_exit();
        return;
    }
    NF_STATUS status = api.udp_post_receive(id, remote, data, length, options);
    if (status != NF_STATUS_SUCCESS) {
        bork_fatal_udp(token, BORK_EVENT_UDP_RECEIVE, id, BORK_REASON_POST_RECEIVE, status);
    }
    bork_callback_exit();
}

void NFAPI_CC bork_udp_send(ENDPOINT_ID id, const unsigned char *remote_raw,
                            const char *data, int length, PNF_UDP_OPTIONS options) {
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
        if (family == AF_INET6) {
            bork_bypass_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_IPV6);
            bork_callback_exit();
            return;
        }
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id,
                       BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (pid == GetCurrentProcessId()) {
        bork_bypass_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_SELF_INTERCEPTION);
        bork_callback_exit();
        return;
    }
    if (local.port == 0 || remote.port == 0) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    if (options == NULL) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    uint32_t option_flags = 0;
    int32_t option_length = 0;
    memcpy(&option_flags, (const unsigned char *)options + offsetof(NF_UDP_OPTIONS, flags), sizeof(option_flags));
    memcpy(&option_length, (const unsigned char *)options + offsetof(NF_UDP_OPTIONS, optionsLength), sizeof(option_length));
    if (option_length < 0 || option_length > 65535) {
        bork_fatal_udp(token, BORK_EVENT_UDP_SEND, id, BORK_REASON_MALFORMED, NF_STATUS_SUCCESS);
        bork_callback_exit();
        return;
    }
    const char *option_data = option_length == 0 ? NULL :
        (const char *)options + offsetof(NF_UDP_OPTIONS, options);
    goNFUDPSend(token, id,
                local.address[0], local.address[1], local.address[2], local.address[3], local.port,
                remote.address[0], remote.address[1], remote.address[2], remote.address[3], remote.port,
                option_flags, (char *)option_data, option_length,
                (char *)data, length);
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

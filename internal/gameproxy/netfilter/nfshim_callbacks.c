//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"
#include "_cgo_export.h"

int bork_copy_sockaddr(const unsigned char *raw, bork_ipv4_endpoint *endpoint) {
    if (raw == NULL || endpoint == NULL) return 0;
    struct sockaddr_in address;
    memcpy(&address, raw, sizeof(address));
    if (address.sin_family != AF_INET) return 0;
    memcpy(endpoint->address, &address.sin_addr, sizeof(endpoint->address));
    endpoint->port = ntohs(address.sin_port);
    return 1;
}

int bork_copy_tcp_info(const NF_TCP_CONN_INFO *packed, DWORD *pid, uint8_t *direction,
                       uint16_t *family, bork_ipv4_endpoint *local,
                       bork_ipv4_endpoint *remote) {
    if (packed == NULL) return 0;
    unsigned char local_raw[NF_MAX_ADDRESS_LENGTH];
    unsigned char remote_raw[NF_MAX_ADDRESS_LENGTH];
    const unsigned char *bytes = (const unsigned char *)packed;
    memcpy(pid, bytes + offsetof(NF_TCP_CONN_INFO, processId), sizeof(*pid));
    memcpy(direction, bytes + offsetof(NF_TCP_CONN_INFO, direction), sizeof(*direction));
    memcpy(family, bytes + offsetof(NF_TCP_CONN_INFO, ip_family), sizeof(*family));
    memcpy(local_raw, bytes + offsetof(NF_TCP_CONN_INFO, localAddress), sizeof(local_raw));
    memcpy(remote_raw, bytes + offsetof(NF_TCP_CONN_INFO, remoteAddress), sizeof(remote_raw));
    return bork_copy_sockaddr(local_raw, local) && bork_copy_sockaddr(remote_raw, remote);
}

int bork_copy_udp_info(const NF_UDP_CONN_INFO *packed, DWORD *pid,
                       uint16_t *family, bork_ipv4_endpoint *local) {
    if (packed == NULL) return 0;
    unsigned char local_raw[NF_MAX_ADDRESS_LENGTH];
    const unsigned char *bytes = (const unsigned char *)packed;
    memcpy(pid, bytes + offsetof(NF_UDP_CONN_INFO, processId), sizeof(*pid));
    memcpy(family, bytes + offsetof(NF_UDP_CONN_INFO, ip_family), sizeof(*family));
    memcpy(local_raw, bytes + offsetof(NF_UDP_CONN_INFO, localAddress), sizeof(local_raw));
    return bork_copy_sockaddr(local_raw, local);
}

int bork_process_path(uint64_t token, DWORD pid, char *utf8, int capacity, int *length) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return 0;
    wchar_t wide[MAX_PATH];
    memset(wide, 0, sizeof(wide));
    if (!api.get_process_name(pid, wide, MAX_PATH)) return 0;
    int units = 0;
    while (units < MAX_PATH && wide[units] != L'\0') units++;
    if (units == 0 || units == MAX_PATH) return 0;
    int bytes = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, wide, units,
                                    utf8, capacity, NULL, NULL);
    if (bytes <= 0) return 0;
    *length = bytes;
    return 1;
}

void bork_fatal_tcp(uint64_t token, int event, uint64_t id, int reason, int32_t status) {
    bork_nf_api api;
    int32_t cleanup = NF_STATUS_NOT_INITIALIZED;
    if (bork_api_snapshot(token, &api)) cleanup = api.tcp_close(id);
    goNFFatal(token, event, id, reason, status, cleanup);
}

void bork_fatal_udp(uint64_t token, int event, uint64_t id, int reason, int32_t status) {
    bork_nf_api api;
    int32_t cleanup = NF_STATUS_NOT_INITIALIZED;
    if (bork_api_snapshot(token, &api)) cleanup = api.udp_state(id, 1);
    goNFFatal(token, event, id, reason, status, cleanup);
}

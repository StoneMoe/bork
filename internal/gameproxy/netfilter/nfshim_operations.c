//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"

int32_t bork_nf_tcp_post_receive(uint64_t token, uint64_t id, const char *data, int length) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    return api.tcp_post_receive(id, data, length);
}

int32_t bork_nf_tcp_close(uint64_t token, uint64_t id) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    return api.tcp_close(id);
}

int32_t bork_nf_udp_post_receive(uint64_t token, uint64_t id,
                                 uint8_t a, uint8_t b, uint8_t c, uint8_t d,
                                 uint16_t port, const char *data, int length) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    struct sockaddr_in source;
    memset(&source, 0, sizeof(source));
    source.sin_family = AF_INET;
    source.sin_port = htons(port);
    unsigned char address[4] = {a, b, c, d};
    memcpy(&source.sin_addr, address, sizeof(address));
    return api.udp_post_receive(id, (const unsigned char *)&source, data, length, NULL);
}

int32_t bork_nf_udp_suspend(uint64_t token, uint64_t id) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    return api.udp_state(id, 1);
}

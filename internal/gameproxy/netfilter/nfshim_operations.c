//go:build windows && amd64 && cgo && game_proxy

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
                                 uint16_t port, const char *data, int length,
                                 uint32_t option_flags, const char *option_data,
                                 int option_length) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    if (option_length < 0 || option_length > 65535 ||
        (option_length > 0 && option_data == NULL)) return NF_STATUS_FAIL;
    struct sockaddr_in source;
    memset(&source, 0, sizeof(source));
    source.sin_family = AF_INET;
    source.sin_port = htons(port);
    unsigned char address[4] = {a, b, c, d};
    memcpy(&source.sin_addr, address, sizeof(address));
    size_t option_size = sizeof(NF_UDP_OPTIONS) + (size_t)option_length;
    PNF_UDP_OPTIONS options = (PNF_UDP_OPTIONS)calloc(1, option_size);
    if (options == NULL) return NF_STATUS_FAIL;
    memcpy((unsigned char *)options + offsetof(NF_UDP_OPTIONS, flags), &option_flags, sizeof(option_flags));
    int32_t encoded_length = option_length;
    memcpy((unsigned char *)options + offsetof(NF_UDP_OPTIONS, optionsLength),
           &encoded_length, sizeof(encoded_length));
    if (option_length > 0) {
        memcpy((unsigned char *)options + offsetof(NF_UDP_OPTIONS, options),
               option_data, (size_t)option_length);
    }
    NF_STATUS status = api.udp_post_receive(id, (const unsigned char *)&source,
                                            data, length, options);
    free(options);
    return status;
}

int32_t bork_nf_udp_suspend(uint64_t token, uint64_t id) {
    bork_nf_api api;
    if (!bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    return api.udp_state(id, 1);
}

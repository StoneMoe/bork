//go:build windows && amd64 && cgo && netfilter_sdk

#ifndef BORK_NFSHIM_H
#define BORK_NFSHIM_H

#include <stdint.h>

typedef struct bork_nf_rules bork_nf_rules;

typedef struct bork_nf_start_result {
    int32_t status;
    uint32_t system_error;
    int init_attempted;
    int init_succeeded;
} bork_nf_start_result;

typedef struct bork_nf_callback_stats {
    uint64_t entered;
    uint64_t exited;
    uint64_t rejected;
} bork_nf_callback_stats;

bork_nf_rules *bork_nf_rules_new(int count);
void bork_nf_rules_free(bork_nf_rules *rules);
int bork_nf_rule_set(bork_nf_rules *rules, int index, int protocol,
                     uint8_t direction, uint16_t family, uint32_t flags,
                     const char *path, int path_len);

bork_nf_start_result bork_nf_start(uint64_t token,
                                   const char *dll_path, int dll_path_len,
                                   const char *normalized_path, int normalized_path_len,
                                   const char *driver_name, int driver_name_len,
                                   const bork_nf_rules *rules);
void bork_nf_callbacks_disable(uint64_t token);
void bork_nf_callbacks_drain(uint64_t token);
void bork_nf_shutdown(uint64_t token, int call_free);
bork_nf_callback_stats bork_nf_callbacks_stats(void);

int32_t bork_nf_tcp_post_receive(uint64_t token, uint64_t id, const char *data, int length);
int32_t bork_nf_tcp_close(uint64_t token, uint64_t id);
int32_t bork_nf_udp_post_receive(uint64_t token, uint64_t id,
                                 uint8_t a, uint8_t b, uint8_t c, uint8_t d,
                                 uint16_t port, const char *data, int length);
int32_t bork_nf_udp_suspend(uint64_t token, uint64_t id);

#endif

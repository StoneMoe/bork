//go:build windows && amd64 && cgo && netfilter_sdk

#include "nfshim_internal.h"

struct bork_nf_rules {
    NF_RULE_EX *items;
    int count;
};

bork_nf_rules *bork_nf_rules_new(int count) {
    if (count <= 0 || (size_t)count > SIZE_MAX / sizeof(NF_RULE_EX)) return NULL;
    bork_nf_rules *rules = HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, sizeof(*rules));
    if (rules == NULL) return NULL;
    rules->items = HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, (size_t)count * sizeof(NF_RULE_EX));
    if (rules->items == NULL) {
        HeapFree(GetProcessHeap(), 0, rules);
        return NULL;
    }
    rules->count = count;
    return rules;
}

void bork_nf_rules_free(bork_nf_rules *rules) {
    if (rules == NULL) return;
    HeapFree(GetProcessHeap(), 0, rules->items);
    HeapFree(GetProcessHeap(), 0, rules);
}

int bork_nf_rule_set(bork_nf_rules *rules, int index, int protocol,
                     uint8_t direction, uint16_t family, uint32_t flags,
                     const char *path, int path_len) {
    if (rules == NULL || index < 0 || index >= rules->count || path == NULL || path_len <= 0) return 0;
    int units = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, path_len, NULL, 0);
    if (units <= 0 || units > 259) return 0;
    NF_RULE_EX *rule = &rules->items[index];
    memset(rule, 0, sizeof(*rule));
    rule->protocol = protocol;
    rule->direction = direction;
    rule->ip_family = family;
    rule->filteringFlag = flags;
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, path_len,
                            rule->processName, units) != units) return 0;
    rule->processName[units] = L'\0';
    return 1;
}

NF_STATUS bork_nf_apply_rules(uint64_t token, const bork_nf_rules *rules) {
    bork_nf_api api;
    if (rules == NULL || !bork_api_snapshot(token, &api)) return NF_STATUS_NOT_INITIALIZED;
    return api.set_rules(rules->items, rules->count);
}

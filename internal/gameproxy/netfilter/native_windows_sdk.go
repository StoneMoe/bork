//go:build windows && amd64 && cgo && netfilter_sdk

package netfilter

/*
#include "nfshim.h"

static inline int bork_nf_rule_set_string(bork_nf_rules *rules, int index,
        int protocol, uint8_t direction, uint16_t family, uint32_t flags,
        _GoString_ path) {
    return bork_nf_rule_set(rules, index, protocol, direction, family, flags,
                            _GoStringPtr(path), (int)_GoStringLen(path));
}

static inline bork_nf_start_result bork_nf_start_strings(uint64_t token,
        _GoString_ dll_path, _GoString_ normalized_path, _GoString_ driver_name,
        const bork_nf_rules *rules) {
    return bork_nf_start(token,
        _GoStringPtr(dll_path), (int)_GoStringLen(dll_path),
        _GoStringPtr(normalized_path), (int)_GoStringLen(normalized_path),
        _GoStringPtr(driver_name), (int)_GoStringLen(driver_name), rules);
}

static inline int32_t bork_nf_tcp_post_receive_string(uint64_t token, uint64_t id,
        _GoString_ payload) {
    return bork_nf_tcp_post_receive(token, id, _GoStringPtr(payload), (int)_GoStringLen(payload));
}

static inline int32_t bork_nf_udp_post_receive_string(uint64_t token, uint64_t id,
        uint8_t a, uint8_t b, uint8_t c, uint8_t d, uint16_t port,
        _GoString_ payload) {
    return bork_nf_udp_post_receive(token, id, a, b, c, d, port,
                                    _GoStringPtr(payload), (int)_GoStringLen(payload));
}
*/
import "C"

import (
	"net/netip"

	"bork/internal/gameproxy/intercept"
)

type cgoNativeSDK struct{}

func newNativeBackend(dllPath, driverName string) (nativeBackend, error) {
	config, err := newNativeConfig(dllPath, driverName)
	if err != nil {
		return nil, err
	}
	return newSDKNativeBackend(config, cgoNativeSDK{}, &netFilterProcessCoordinator), nil
}

func (cgoNativeSDK) Start(token uint64, config nativeConfig, rules []nativeRule) nativeStartResult {
	nativeRules := C.bork_nf_rules_new(C.int(len(rules)))
	if nativeRules == nil {
		return nativeStartResult{status: nativeStatusFail, systemError: 8}
	}
	defer C.bork_nf_rules_free(nativeRules)
	for index, rule := range rules {
		if C.bork_nf_rule_set_string(
			nativeRules, C.int(index), C.int(rule.protocol), C.uint8_t(rule.direction),
			C.uint16_t(rule.family), C.uint32_t(rule.flags), rule.executablePath,
		) == 0 {
			return nativeStartResult{status: nativeStatusFail}
		}
	}
	result := C.bork_nf_start_strings(
		C.uint64_t(token), config.dllPath, config.normalizedDLLPath, config.driverName, nativeRules,
	)
	return nativeStartResult{
		status:        nativeStatus(result.status),
		systemError:   uint32(result.system_error),
		initAttempted: result.init_attempted != 0,
		initSucceeded: result.init_succeeded != 0,
	}
}

func (cgoNativeSDK) DisableCallbacks(token uint64) {
	C.bork_nf_callbacks_disable(C.uint64_t(token))
}

func (cgoNativeSDK) DrainCallbacks(token uint64) {
	C.bork_nf_callbacks_drain(C.uint64_t(token))
}

func (cgoNativeSDK) Shutdown(token uint64, callFree bool) {
	free := C.int(0)
	if callFree {
		free = 1
	}
	C.bork_nf_shutdown(C.uint64_t(token), free)
}

func (cgoNativeSDK) PostTCPReceive(token uint64, id intercept.NativeID, payload []byte) nativeStatus {
	return nativeStatus(C.bork_nf_tcp_post_receive_string(C.uint64_t(token), C.uint64_t(id), string(payload)))
}

func (cgoNativeSDK) CloseTCP(token uint64, id intercept.NativeID) nativeStatus {
	return nativeStatus(C.bork_nf_tcp_close(C.uint64_t(token), C.uint64_t(id)))
}

func (cgoNativeSDK) PostUDPReceive(token uint64, id intercept.NativeID, source netip.AddrPort, payload []byte) nativeStatus {
	address := source.Addr().As4()
	return nativeStatus(C.bork_nf_udp_post_receive_string(
		C.uint64_t(token), C.uint64_t(id),
		C.uint8_t(address[0]), C.uint8_t(address[1]), C.uint8_t(address[2]), C.uint8_t(address[3]),
		C.uint16_t(source.Port()), string(payload),
	))
}

func (cgoNativeSDK) SuspendUDP(token uint64, id intercept.NativeID) nativeStatus {
	return nativeStatus(C.bork_nf_udp_suspend(C.uint64_t(token), C.uint64_t(id)))
}

func nativeCallbackStats() nativeCallbackStatsSnapshot {
	stats := C.bork_nf_callbacks_stats()
	return nativeCallbackStatsSnapshot{entered: uint64(stats.entered), exited: uint64(stats.exited), rejected: uint64(stats.rejected)}
}

type nativeCallbackStatsSnapshot struct {
	entered  uint64
	exited   uint64
	rejected uint64
}

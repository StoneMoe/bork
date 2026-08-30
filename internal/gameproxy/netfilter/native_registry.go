package netfilter

import (
	"fmt"
	"sync"
	"sync/atomic"
)

var (
	nativeTokenCounter atomic.Uint64
	nativeOwnersMu     sync.RWMutex
	nativeOwners       = make(map[uint64]*sdkNativeBackend)
)

func registerNativeOwner(owner *sdkNativeBackend) uint64 {
	token := nativeTokenCounter.Add(1)
	if token == 0 {
		token = nativeTokenCounter.Add(1)
	}
	nativeOwnersMu.Lock()
	nativeOwners[token] = owner
	nativeOwnersMu.Unlock()
	return token
}

func nativeOwner(token uint64) *sdkNativeBackend {
	nativeOwnersMu.RLock()
	owner := nativeOwners[token]
	nativeOwnersMu.RUnlock()
	return owner
}

func unregisterNativeOwner(token uint64) {
	nativeOwnersMu.Lock()
	delete(nativeOwners, token)
	nativeOwnersMu.Unlock()
}

func dispatchNativeCallback(token uint64, event nativeEvent, callback func(*sdkNativeBackend)) {
	owner := nativeOwner(token)
	if owner == nil {
		return
	}
	defer func() {
		if value := recover(); value != nil {
			owner.reportFatal(&NativeCallbackError{Event: event, Cause: fmt.Errorf("callback panic: %v", value)})
		}
	}()
	callback(owner)
}

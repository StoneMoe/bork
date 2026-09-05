package netfilter

import (
	"errors"
	"testing"
)

func TestNativeStatus_invalid_endpoint_is_idempotent_only_for_close_and_suspend(t *testing.T) {
	tests := []struct {
		operation nativeOperation
		wantError bool
	}{
		{operation: nativeOperationCloseTCP},
		{operation: nativeOperationSuspendUDP},
		{operation: nativeOperationPostTCPReceive, wantError: true},
		{operation: nativeOperationPostUDPReceive, wantError: true},
		{operation: nativeOperationGetUDPInfo, wantError: true},
		{operation: nativeOperationInit, wantError: true},
		{operation: nativeOperationSetRules, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.operation.String(), func(t *testing.T) {
			// When
			err := nativeStatusError(test.operation, nativeStatusInvalidEndpointID)

			// Then
			if test.wantError && err == nil {
				t.Fatal("nativeStatusError() = nil, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("nativeStatusError() = %v, want nil", err)
			}
		})
	}
}

func TestNativeStatusError_preserves_operation_and_status(t *testing.T) {
	// When
	err := nativeStatusError(nativeOperationPostTCPReceive, nativeStatusIOError)

	// Then
	var statusErr *NativeStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type = %T, want *NativeStatusError", err)
	}
	if statusErr.Operation != nativeOperationPostTCPReceive || statusErr.Status != nativeStatusIOError {
		t.Fatalf("status error = %#v", statusErr)
	}
}

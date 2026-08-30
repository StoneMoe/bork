package netfilter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeBuildConstraint = "//go:build windows && amd64 && cgo && netfilter_sdk"

func TestNativeCFiles_share_exact_build_constraint(t *testing.T) {
	// Given
	files := []string{
		"nfshim.h", "nfshim_internal.h", "nfshim_loader.c", "nfshim_lifecycle.c",
		"nfshim_rules.c", "nfshim_callbacks.c", "nfshim_tcp.c", "nfshim_udp.c", "nfshim_operations.c",
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			// When
			content, err := os.ReadFile(filepath.Join(".", name))

			// Then
			if err != nil {
				t.Fatal(err)
			}
			firstLine, _, _ := strings.Cut(string(content), "\n")
			if firstLine != nativeBuildConstraint {
				t.Fatalf("first line = %q, want %q", firstLine, nativeBuildConstraint)
			}
		})
	}
}

func TestNativeCShim_pins_verified_SDK_ABI_and_dynamic_symbols(t *testing.T) {
	// Given
	content := readNativeSources(t)
	required := []string{
		"#define _C_API", `sdk/nfsdk/wfp/include/nfapi.h`,
		"sizeof(NF_RULE_EX) == 643", "offsetof(NF_RULE_EX, localProxyProcessId) == 639",
		"sizeof(NF_TCP_CONN_INFO) == 67", "sizeof(NF_UDP_CONN_INFO) == 34",
		"sizeof(NF_EventHandler) == 128", "LoadLibraryExW", "GetProcAddress",
		`"nf_setOptions"`, `"nf_init"`, `"nf_free"`, `"nf_setRulesEx"`,
		`"nf_tcpPostReceive"`, `"nf_tcpClose"`, `"nf_udpPostReceive"`,
		`"nf_udpSetConnectionState"`, `"nf_getUDPConnInfo"`, `"nf_getProcessNameW"`,
		"NFF_DISABLE_AUTO_REGISTER",
	}

	// Then
	for _, token := range required {
		if !strings.Contains(content, token) {
			t.Errorf("native C sources missing %q", token)
		}
	}
	for _, forbidden := range []string{"nf_registerDriver", "nf_tcpDisableFiltering", "nf_udpDisableFiltering"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("native C sources contain forbidden API %q", forbidden)
		}
	}
}

func TestNativeWindowsGoFiles_are_tagged_and_do_not_use_unsafe(t *testing.T) {
	// Given
	files := []string{"native_windows_sdk.go", "native_windows_callbacks.go"}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			// When
			content, err := os.ReadFile(filepath.Join(".", name))

			// Then
			if err != nil {
				t.Fatal(err)
			}
			source := string(content)
			firstLine, _, _ := strings.Cut(source, "\n")
			if firstLine != nativeBuildConstraint {
				t.Fatalf("first line = %q, want %q", firstLine, nativeBuildConstraint)
			}
			if strings.Contains(source, `"unsafe"`) {
				t.Fatal("Windows cgo source imports unsafe")
			}
		})
	}
}

func TestNativeCShim_reports_source_callback_event_on_failure(t *testing.T) {
	// Given
	tests := []struct {
		file      string
		function  string
		expected  string
		forbidden string
	}{
		{"nfshim_tcp.c", "bork_tcp_connected", "BORK_EVENT_TCP_CONNECTED", "BORK_EVENT_TCP_CLOSED"},
		{"nfshim_tcp.c", "bork_tcp_closed", "BORK_EVENT_TCP_CLOSED", "BORK_EVENT_TCP_CONNECTED"},
		{"nfshim_udp.c", "bork_udp_created", "BORK_EVENT_UDP_CREATED", "BORK_EVENT_UDP_CLOSED"},
		{"nfshim_udp.c", "bork_udp_closed", "BORK_EVENT_UDP_CLOSED", "BORK_EVENT_UDP_CREATED"},
	}

	for _, test := range tests {
		t.Run(test.function, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(".", test.file))
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(string(content), "void NFAPI_CC "+test.function+"(")
			if start < 0 {
				t.Fatalf("function %s not found", test.function)
			}
			body := string(content)[start:]
			if end := strings.Index(body[1:], "\nvoid NFAPI_CC "); end >= 0 {
				body = body[:end+1]
			}

			// Then
			if !strings.Contains(body, test.expected) {
				t.Errorf("%s does not report %s", test.function, test.expected)
			}
			if strings.Contains(body, test.forbidden) {
				t.Errorf("%s reports adjacent callback event %s", test.function, test.forbidden)
			}
		})
	}
}

func readNativeSources(t *testing.T) string {
	t.Helper()
	names := []string{"nfshim.h", "nfshim_internal.h", "nfshim_loader.c", "nfshim_lifecycle.c", "nfshim_rules.c", "nfshim_callbacks.c", "nfshim_tcp.c", "nfshim_udp.c", "nfshim_operations.c"}
	var sources strings.Builder
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		sources.Write(content)
	}
	return sources.String()
}

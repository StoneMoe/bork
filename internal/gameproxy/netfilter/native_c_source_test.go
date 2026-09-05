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
			firstLine = strings.TrimSuffix(firstLine, "\r")
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
		"offsetof(NF_UDP_OPTIONS, options) == 8",
		"sizeof(NF_EventHandler) == 128", "LoadLibraryExW", "GetProcAddress",
		`"nf_setOptions"`, `"nf_init"`, `"nf_free"`, `"nf_setRulesEx"`,
		`"nf_tcpPostSend"`, `"nf_tcpPostReceive"`, `"nf_tcpClose"`, `"nf_tcpDisableFiltering"`, `"nf_udpPostReceive"`,
		`"nf_udpSetConnectionState"`, `"nf_getUDPConnInfo"`, `"nf_getProcessNameW"`,
		`"nf_getProcessNameFromKernel"`, `"nf_adjustProcessPriviledges"`, `"nf_udpDisableFiltering"`,
		"api.set_options(1, NFF_DISABLE_AUTO_REGISTER | NFF_DISABLE_AUTO_START)",
	}

	// Then
	for _, token := range required {
		if !strings.Contains(content, token) {
			t.Errorf("native C sources missing %q", token)
		}
	}
	for _, forbidden := range []string{"nf_registerDriver"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("native C sources contain forbidden API %q", forbidden)
		}
	}
}

func TestNativeCShim_bypasses_IPv6_UDP_without_suspending_IPv4_failures(t *testing.T) {
	content, err := os.ReadFile("nfshim_callbacks.c")
	if err != nil {
		t.Fatal(err)
	}
	body := nativeFunctionBody(t, string(content), "bork_fatal_udp")
	want := "reason == BORK_REASON_IPV6 ? api.udp_disable_filtering(id) : api.udp_state(id, 1)"
	if !strings.Contains(body, want) {
		t.Fatalf("UDP endpoint cleanup does not bypass only IPv6: %s", body)
	}
}

func TestNativeCShim_silently_disables_filtering_for_IPv6_callbacks(t *testing.T) {
	tcpContent, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	udpContent, err := os.ReadFile("nfshim_udp.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body, call string
	}{
		{"TCP connect request", nativeFunctionBody(t, string(tcpContent), "bork_tcp_connect_request"), "bork_bypass_tcp"},
		{"UDP created", nativeFunctionBody(t, string(udpContent), "bork_udp_created"), "bork_bypass_udp"},
		{"UDP closed", nativeFunctionBody(t, string(udpContent), "bork_udp_closed"), "bork_bypass_udp"},
		{"UDP send", nativeFunctionBody(t, string(udpContent), "bork_udp_send"), "bork_bypass_udp"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(test.body, "family == AF_INET6") || !strings.Contains(test.body, test.call) {
				t.Fatalf("IPv6 callback does not disable filtering: %s", test.body)
			}
		})
	}
}

func TestNativeCShim_posts_valid_receive_without_closing_endpoint(t *testing.T) {
	tcpContent, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	udpContent, err := os.ReadFile("nfshim_udp.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body, post string
	}{
		{"TCP", nativeFunctionBody(t, string(tcpContent), "bork_tcp_receive"), "api.tcp_post_receive(id, data, length)"},
		{"UDP", nativeFunctionBody(t, string(udpContent), "bork_udp_receive"), "api.udp_post_receive(id, remote, data, length, options)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.Contains(test.body, "BORK_REASON_UNSUPPORTED") {
				t.Fatalf("valid receive is still treated as unsupported: %s", test.body)
			}
			if !strings.Contains(test.body, "BORK_REASON_MALFORMED") ||
				!strings.Contains(test.body, "BORK_REASON_POST_RECEIVE") ||
				!strings.Contains(test.body, test.post) {
				t.Fatalf("receive callback does not post valid data: %s", test.body)
			}
		})
	}
}

func TestNativeCShim_redirects_TCP_connect_requests_to_loopback_listener(t *testing.T) {
	content, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	connect := nativeFunctionBody(t, string(content), "bork_tcp_connect_request")
	for _, token := range []string{"goNFTCPConnectRequest", "INADDR_LOOPBACK", "htons(redirect_port)", "bork_block_tcp_connect(info)", "GetCurrentProcessId()", "offsetof(NF_TCP_CONN_INFO, processId)"} {
		if !strings.Contains(connect, token) {
			t.Fatalf("TCP connect redirect callback missing %q: %s", token, connect)
		}
	}
	if strings.Contains(connect, "NF_OFFLINE") {
		t.Fatalf("TCP connect redirect still uses offline emulation: %s", connect)
	}
}

func TestNativeCShim_bypasses_TCP_loopback_before_redirect(t *testing.T) {
	content, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	connect := nativeFunctionBody(t, string(content), "bork_tcp_connect_request")
	bypass := strings.Index(connect, "remote.address[0] == 127")
	redirect := strings.Index(connect, "goNFTCPConnectRequest")
	if bypass < 0 || redirect < 0 || bypass > redirect ||
		!strings.Contains(connect[bypass:redirect], "BORK_REASON_LOCAL_ROUTE") {
		t.Fatalf("TCP loopback is not bypassed before redirect: %s", connect)
	}
}

func TestNativeCShim_preserves_UDP_options_for_injected_responses(t *testing.T) {
	udpContent, err := os.ReadFile("nfshim_udp.c")
	if err != nil {
		t.Fatal(err)
	}
	operationsContent, err := os.ReadFile("nfshim_operations.c")
	if err != nil {
		t.Fatal(err)
	}
	send := nativeFunctionBody(t, string(udpContent), "bork_udp_send")
	post := nativeFunctionBody(t, string(operationsContent), "bork_nf_udp_post_receive")
	for _, token := range []string{"option_flags", "option_length", "goNFUDPSend"} {
		if !strings.Contains(send, token) {
			t.Fatalf("UDP send callback does not preserve %q: %s", token, send)
		}
	}
	for _, token := range []string{"option_flags", "option_data", "optionsLength", "api.udp_post_receive"} {
		if !strings.Contains(post, token) {
			t.Fatalf("UDP receive injection does not restore %q: %s", token, post)
		}
	}
	if strings.Contains(post, "data, length, NULL") {
		t.Fatalf("UDP receive injection still discards options: %s", post)
	}
}

func TestNativeCShim_blocks_rejected_TCP_connect_before_cleanup(t *testing.T) {
	content, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	connect := nativeFunctionBody(t, string(content), "bork_tcp_connect_request")
	for _, reason := range []string{"BORK_REASON_MALFORMED", "BORK_REASON_INCOMING_TCP"} {
		reasonAt := strings.Index(connect, reason)
		if reasonAt < 0 {
			t.Fatalf("connect callback missing %s", reason)
		}
		blockAt := strings.LastIndex(connect[:reasonAt], "bork_block_tcp_connect(info)")
		if blockAt < 0 {
			t.Fatalf("connect callback does not block before %s", reason)
		}
	}
}

func TestNativeWindowsCallbacks_reports_endpoint_errors_without_stopping_backend(t *testing.T) {
	content, err := os.ReadFile("native_windows_callbacks.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	if !strings.Contains(source, "owner.deliverEndpointError") || strings.Contains(source, "owner.reportFatal") {
		t.Fatal("native endpoint callback errors still stop the backend")
	}
}

func TestNativeCShim_falls_back_to_kernel_process_path_query(t *testing.T) {
	content, err := os.ReadFile("nfshim_callbacks.c")
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	userQuery := "if (!api.get_process_name(pid, wide, MAX_PATH))"
	kernelQuery := "if (!api.get_process_name_kernel(pid, wide, MAX_PATH)) return 0;"
	if !strings.Contains(source, userQuery) || !strings.Contains(source, kernelQuery) ||
		strings.Index(source, userQuery) > strings.Index(source, kernelQuery) {
		t.Fatal("process path query does not fall back from user mode to the kernel API")
	}
}

func TestNativeCShim_process_path_failure_uses_exact_rule_fallback(t *testing.T) {
	udpSource, err := os.ReadFile("nfshim_udp.c")
	if err != nil {
		t.Fatal(err)
	}
	tcpSource, err := os.ReadFile("nfshim_tcp.c")
	if err != nil {
		t.Fatal(err)
	}
	udpCreated := nativeFunctionBody(t, string(udpSource), "bork_udp_created")
	tcpConnect := nativeFunctionBody(t, string(tcpSource), "bork_tcp_connect_request")
	if !strings.Contains(udpCreated, "char path[MAX_PATH * 4] = {0};") ||
		!strings.Contains(udpCreated, "(void)bork_process_path") ||
		!strings.Contains(udpCreated, "goNFUDPCreated") ||
		strings.Contains(udpCreated, "BORK_REASON_PROCESS_PATH") {
		t.Fatal("UDP created does not preserve the exact-rule fallback for a missing process path")
	}
	if !strings.Contains(tcpConnect, "char path[MAX_PATH * 4] = {0};") ||
		!strings.Contains(tcpConnect, "(void)bork_process_path") ||
		!strings.Contains(tcpConnect, "goNFTCPConnectRequest") ||
		strings.Contains(tcpConnect, "BORK_REASON_PROCESS_PATH") {
		t.Fatal("TCP connect request does not preserve the exact-rule fallback for a missing process path")
	}
}

func TestNativeCShim_bypasses_current_process_UDP(t *testing.T) {
	content, err := os.ReadFile("nfshim_udp.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, function := range []string{"bork_udp_created", "bork_udp_send"} {
		body := nativeFunctionBody(t, string(content), function)
		if !strings.Contains(body, "pid == GetCurrentProcessId()") ||
			!strings.Contains(body, "bork_bypass_udp") {
			t.Fatalf("%s does not bypass current process UDP: %s", function, body)
		}
	}
}

func TestNativeCShim_reports_bypass_only_after_retry_fails(t *testing.T) {
	for _, test := range []struct {
		file, helper, disable string
	}{
		{"nfshim_tcp.c", "bork_bypass_tcp", "api.tcp_disable_filtering(id)"},
		{"nfshim_udp.c", "bork_bypass_udp", "api.udp_disable_filtering(id)"},
	} {
		content, err := os.ReadFile(test.file)
		if err != nil {
			t.Fatal(err)
		}
		body := nativeFunctionBody(t, string(content), test.helper)
		if strings.Count(body, test.disable) != 2 ||
			!strings.Contains(body, "if (cleanup != NF_STATUS_SUCCESS) goNFFatal") {
			t.Fatalf("%s does not retry before reporting: %s", test.helper, body)
		}
	}
}

func TestNativeCShim_encodes_process_ID_rules(t *testing.T) {
	content, err := os.ReadFile("nfshim_rules.c")
	if err != nil {
		t.Fatal(err)
	}
	body := nativeFunctionBody(t, string(content), "bork_nf_rule_set")
	if !strings.Contains(body, "uint32_t process_id") || !strings.Contains(body, "rule->processId = process_id") {
		t.Fatalf("native rule encoding omits process ID: %s", body)
	}
}

func TestNativeCShim_adjusts_process_privileges_before_init(t *testing.T) {
	content, err := os.ReadFile("nfshim_lifecycle.c")
	if err != nil {
		t.Fatal(err)
	}
	start := nativeFunctionBody(t, string(content), "bork_nf_start")
	adjust := strings.Index(start, "api.adjust_process_privileges();")
	init := strings.Index(start, "api.init(")
	if adjust < 0 || init < 0 || adjust > init {
		t.Fatal("process privileges are not adjusted before nf_init")
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
			firstLine = strings.TrimSuffix(firstLine, "\r")
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

func nativeFunctionBody(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, name+"(")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	body := source[start:]
	if end := strings.Index(body[1:], "\nvoid "); end >= 0 {
		body = body[:end+1]
	}
	return body
}

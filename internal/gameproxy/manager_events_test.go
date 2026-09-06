//go:build game_proxy

package gameproxy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

func TestManager_connection_events_are_bounded_sanitized_and_copied(t *testing.T) {
	manager := &Manager{changes: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &managerRun{ctx: ctx, redactions: []string{"player", "secret"}}
	manager.current = run
	manager.status = Status{Supported: true, State: StateStarting, Directories: []string{"games"}}

	for index := range maxConnectionEvents + 7 {
		manager.appendEvent(run, "error", fmt.Sprintf("event %d for player with secret", index))
	}

	status := manager.Status()
	if len(status.Events) != maxConnectionEvents {
		t.Fatalf("event count = %d, want %d", len(status.Events), maxConnectionEvents)
	}
	if !strings.Contains(status.Events[0].Message, "event 7") {
		t.Fatalf("oldest retained event = %q", status.Events[0].Message)
	}
	for _, event := range status.Events {
		if strings.Contains(event.Message, "player") || strings.Contains(event.Message, "secret") {
			t.Fatalf("event contains credentials: %q", event.Message)
		}
	}
	status.Events[0].Message = "mutated"
	status.Directories[0] = "mutated"
	if manager.Status().Events[0].Message == "mutated" {
		t.Fatal("Status returned manager-owned events")
	}
	if manager.Status().Directories[0] == "mutated" {
		t.Fatal("Status returned manager-owned directories")
	}
}

func TestFormatFlowEvent_correlates_request_and_response_without_payload(t *testing.T) {
	metadata := intercept.Metadata{
		NativeID: 42, ProcessID: 7, ExecutablePath: `C:\Games\rainbow.exe`,
		OriginalLocal:  netip.MustParseAddrPort("10.0.0.2:41000"),
		OriginalRemote: netip.MustParseAddrPort("203.0.113.8:443"),
	}
	request := formatFlowEvent(intercept.FlowEvent{
		RequestID: "tcp-42", Protocol: "TCP", Kind: intercept.FlowEventRequest, Metadata: metadata,
	})
	response := formatFlowEvent(intercept.FlowEvent{
		RequestID: "tcp-42", Protocol: "TCP", Kind: intercept.FlowEventResponse, Metadata: metadata, Bytes: 512,
	})
	failure := formatFlowEvent(intercept.FlowEvent{
		RequestID: "tcp-42", Protocol: "TCP", Kind: intercept.FlowEventFailed,
		Metadata: metadata, Err: errors.New("dial failed"),
	})

	for _, message := range []string{request, response, failure} {
		if !strings.Contains(message, "[request_id=tcp-42]") || !strings.Contains(message, "protocol=TCP") {
			t.Fatalf("uncorrelated flow event = %q", message)
		}
	}
	if !strings.Contains(request, "proxy request") || !strings.Contains(response, "first inbound data bytes=512") ||
		!strings.Contains(failure, "failed error=dial failed") {
		t.Fatalf("flow messages = %q / %q / %q", request, response, failure)
	}
	for _, message := range []string{request, response, failure} {
		if strings.Contains(message, "payload") {
			t.Fatalf("flow event contains payload: %q", message)
		}
	}
}

func TestFormatFlowEvent_reports_directional_UDP_traffic_by_flow(t *testing.T) {
	metadata := intercept.Metadata{
		NativeID: 42, ProcessID: 7, ExecutablePath: `C:\Games\rainbow.exe`,
		OriginalLocal:  netip.MustParseAddrPort("10.0.0.2:41000"),
		OriginalRemote: netip.MustParseAddrPort("203.0.113.8:9000"),
	}
	message := formatFlowEvent(intercept.FlowEvent{
		FlowID: "udp-42", Protocol: "UDP", Kind: intercept.FlowEventTraffic, Metadata: metadata,
		UploadPackets: 3, UploadBytes: 300, DownloadPackets: 4, DownloadBytes: 500,
	})
	for _, want := range []string{
		"[flow_id=udp-42]", "outbound_packets=3", "outbound_bytes=300",
		"inbound_packets=4", "inbound_bytes=500",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("UDP traffic event %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "request_id") || strings.Contains(message, "unsolicited") {
		t.Fatalf("UDP traffic event uses request semantics: %q", message)
	}
}

func TestManager_sanitizer_redacts_overlapping_credentials(t *testing.T) {
	run := &managerRun{redactions: []string{"abcdef", "abc"}}
	if got := run.sanitize("username abc password abcdef"); strings.Contains(got, "abc") || strings.Contains(got, "def") {
		t.Fatalf("sanitized message = %q", got)
	}
}

func TestSanitizeFlowEvent_does_not_corrupt_structured_fields(t *testing.T) {
	run := &managerRun{redactions: []string{"p"}}
	event := sanitizeFlowEvent(run, intercept.FlowEvent{
		RequestID: "tcp-42", Protocol: "TCP", Kind: intercept.FlowEventFailed,
		Metadata: intercept.Metadata{ExecutablePath: `C:\player\game.exe`},
		Err:      errors.New("password p"),
	})
	message := formatFlowEvent(event)
	if !strings.Contains(message, "[request_id=tcp-42]") || !strings.Contains(message, "protocol=TCP") {
		t.Fatalf("structured fields were sanitized: %q", message)
	}
	if strings.Contains(message, `C:\player`) || strings.Contains(message, "password p") {
		t.Fatalf("external fields were not sanitized: %q", message)
	}
	if level := flowEventLevel(event); level != "error" {
		t.Fatalf("flow event level = %q, want error", level)
	}
}

func TestManager_accepts_completion_event_while_current_run_is_stopping(t *testing.T) {
	manager := &Manager{changes: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	run := &managerRun{ctx: ctx}
	manager.current = run
	manager.status = Status{Supported: true, State: StateStopping}
	cancel()

	manager.appendStructuredEvent(run, "info", "flow completed")

	events := manager.Status().Events
	if len(events) != 1 || events[0].Message != "flow completed" {
		t.Fatalf("stopping events = %#v", events)
	}
}

func TestFormatFlowEvent_reports_dropped_diagnostics(t *testing.T) {
	event := intercept.FlowEvent{RequestID: "flow-events", Kind: intercept.FlowEventDropped, Bytes: 17}
	if message := formatFlowEvent(event); !strings.Contains(message, "dropped count=17") {
		t.Fatalf("dropped event message = %q", message)
	}
	if level := flowEventLevel(event); level != "warning" {
		t.Fatalf("dropped event level = %q, want warning", level)
	}
}

func TestFormatDataPathEvent_reports_boundaries_without_payload(t *testing.T) {
	message := formatDataPathEvent(iwan.Status{Generation: 7, DataPath: iwan.DataPathStats{
		OutboundStackPackets: 3, OutboundUDPPackets: 2, OutboundNodeDatagrams: 3,
		InboundNodeDatagrams: 5, InboundDataDatagrams: 3, InboundFragmentDatagrams: 2,
		InboundFragmentCompletedPackets: 1, InboundMalformedDatagrams: 1, InboundInvalidIPv4Packets: 1,
		InboundStackPackets: 4, InboundTCPPackets: 1,
		InboundUDPPackets: 1, InboundICMPPackets: 1, InboundOtherPackets: 1,
		InboundICMPDestinationUnreachablePackets: 1, InboundICMPPortUnreachablePackets: 1,
	}}, "anomaly")
	for _, want := range []string{
		"generation=7", "phase=anomaly", "outbound_stack_packets=3", "outbound_udp_packets=2",
		"outbound_node_datagrams=3", "inbound_node_datagrams=5",
		"inbound_data_datagrams=3", "inbound_fragment_datagrams=2",
		"inbound_fragment_completed_packets=1", "inbound_malformed_datagrams=1",
		"inbound_invalid_ipv4_packets=1",
		"inbound_stack_packets=4", "inbound_tcp_packets=1", "inbound_udp_packets=1",
		"inbound_icmp_packets=1", "inbound_other_packets=1",
		"inbound_icmp_destination_unreachable_packets=1", "inbound_icmp_port_unreachable_packets=1",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("data path event %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "payload") {
		t.Fatalf("data path event contains payload: %q", message)
	}
}

func TestDataPathEventTracker_logs_lifecycle_periodic_and_anomaly_snapshots(t *testing.T) {
	tracker := dataPathEventTracker{}
	startedAt := time.Unix(100, 0)
	status := iwan.Status{State: iwan.StateReady, Generation: 7}
	if events := tracker.observe(status, startedAt); len(events) != 1 || events[0].phase != "generation-start" {
		t.Fatalf("start events = %#v", events)
	}
	status.DataPath.OutboundStackPackets = 1
	if events := tracker.observe(status, startedAt.Add(9*time.Second)); len(events) != 0 {
		t.Fatalf("early periodic events = %#v", events)
	}
	status.DataPath.OutboundStackPackets = 2
	if events := tracker.observe(status, startedAt.Add(10*time.Second)); len(events) != 1 || events[0].phase != "periodic" {
		t.Fatalf("periodic events = %#v", events)
	}
	status.DataPath.InboundInvalidIPv4Packets = 1
	if events := tracker.observe(status, startedAt.Add(11*time.Second)); len(events) != 1 || events[0].phase != "anomaly" {
		t.Fatalf("anomaly events = %#v", events)
	}
	if events := tracker.observe(status, startedAt.Add(30*time.Second)); len(events) != 0 {
		t.Fatalf("unchanged events = %#v", events)
	}
	if events := tracker.finish(status); len(events) != 1 || events[0].phase != "generation-end" {
		t.Fatalf("finish events = %#v", events)
	}
}

func TestManager_completed_data_path_uses_exact_previous_generation_snapshot(t *testing.T) {
	log := &eventLog{}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 8})
	supervisor.appendDataPathEvent(iwan.DataPathEvent{
		Phase:  iwan.DataPathEventEnd,
		Status: iwan.Status{Generation: 7, DataPath: iwan.DataPathStats{OutboundStackPackets: 9}},
	})
	manager := &Manager{changes: make(chan struct{}, 1)}
	run := &managerRun{supervisor: supervisor, dataPath: &dataPathEventTracker{}}
	run.dataPath.observe(iwan.Status{
		State: iwan.StateReady, Generation: 7,
		DataPath: iwan.DataPathStats{OutboundStackPackets: 3},
	}, time.Unix(100, 0))
	manager.current = run

	manager.appendDataPathLifecycleEvents(run)

	if run.dataPath.active {
		t.Fatal("previous generation remained active")
	}
	events := manager.Status().Events
	if len(events) != 1 || !strings.Contains(events[0].Message, "generation=7 phase=generation-end") ||
		!strings.Contains(events[0].Message, "outbound_stack_packets=9") {
		t.Fatalf("completed generation events = %#v", events)
	}
}

func TestManager_periodic_sample_drains_generation_events_before_observing(t *testing.T) {
	log := &eventLog{}
	bridge := newFakeBridge(log)
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
	manager := newTestManager(log, supervisor, &fakeBridgeFactory{log: log, supported: true, bridge: bridge})
	if err := manager.Start(t.Context(), validStartInput()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	// Make only the traffic tick observable, without a status notification.
	supervisor.mu.Lock()
	supervisor.status = iwan.Status{State: iwan.StateReady, Generation: 2}
	supervisor.dataPathEvents = []iwan.DataPathEvent{
		{Phase: iwan.DataPathEventEnd, Status: iwan.Status{Generation: 1, DataPath: iwan.DataPathStats{OutboundStackPackets: 9}}},
		{Phase: iwan.DataPathEventStart, Status: supervisor.status},
	}
	supervisor.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		var ends []string
		started := false
		for _, event := range manager.Status().Events {
			if strings.Contains(event.Message, "generation=1 phase=generation-end") {
				ends = append(ends, event.Message)
			}
			started = started || strings.Contains(event.Message, "generation=2 phase=generation-start")
		}
		if started {
			if len(ends) != 1 || !strings.Contains(ends[0], "outbound_stack_packets=9") {
				t.Fatalf("periodic generation completion = %q", ends)
			}
			return
		}
		select {
		case <-manager.Changes():
		case <-ctx.Done():
			t.Fatal("periodic sample did not observe the generation")
		}
	}
}

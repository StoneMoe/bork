package gameproxy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

const (
	managerQueueSize    = 256
	managerIdleTimeout  = time.Minute
	dataPathLogInterval = 10 * time.Second
)

func (manager *Manager) run(run *managerRun, input StartInput) {
	defer close(run.done)
	rules, err := manager.dependencies.scanRules(input.Directories)
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("scan executable rules: %w", err))
		return
	}
	if rules.executableCount == 0 {
		manager.finishStartFailure(run, ErrNoExecutables)
		return
	}
	manager.updateExecutableCount(run, rules.executableCount)
	manager.appendEvent(run, "info", fmt.Sprintf("Found %d game executables", rules.executableCount))
	if err := run.ctx.Err(); err != nil {
		manager.finishStopped(run, err)
		return
	}
	if err := manager.dependencies.bridge.EnsureAvailable(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("ensure intercept bridge: %w", err))
		return
	}
	manager.appendEvent(run, "info", "Interception driver is available")
	supervisor, err := manager.dependencies.newSupervisor(iwan.Options{Node: input.Node})
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("construct iwan supervisor: %w", err))
		return
	}
	run.supervisor = supervisor
	manager.appendEvent(run, "info", "Connecting to iWAN server")
	if err := supervisor.Start(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("start iwan supervisor: %w", err))
		return
	}
	manager.appendEvent(run, "info", "Authenticating with iWAN server")
	if err := supervisor.WaitReady(run.ctx); err != nil {
		if run.ctx.Err() != nil {
			manager.finishStopped(run, run.ctx.Err())
		} else {
			manager.finishStartFailure(run, fmt.Errorf("wait for iwan: %w", err))
		}
		return
	}
	run.dataPath = &dataPathEventTracker{}
	iwanStatus := manager.appendDataPathLifecycleEvents(run)
	if iwanStatus.State != iwan.StateReady {
		manager.finishStartFailure(run, iwan.ErrNotReady)
		return
	}
	manager.appendDataPathEvents(run, run.dataPath.observe(iwanStatus, time.Now()))
	bridge, err := manager.dependencies.bridge.New(run.ctx, slices.Clone(rules.paths))
	if err != nil {
		manager.finishStartFailure(run, fmt.Errorf("construct intercept bridge: %w", err))
		return
	}
	run.bridge = bridge
	relay, err := intercept.New(intercept.Options{
		Bridge: bridge, Rules: rules.matcher, Dialer: supervisor, DNS: input.DNS,
		QueueSize: managerQueueSize, IdleTimeout: managerIdleTimeout, Clock: wallClock{},
		OnEndpointError: func(err error) {
			manager.appendEvent(run, "error", "Interception endpoint failed: "+errorString(err))
		},
		OnFlowEvent: func(event intercept.FlowEvent) {
			event = sanitizeFlowEvent(run, event)
			manager.appendStructuredEvent(run, flowEventLevel(event), formatFlowEvent(event))
		},
	})
	if err != nil {
		failure := errors.Join(fmt.Errorf("construct intercept relay: %w", err), bridge.Close())
		manager.finishStartFailure(run, failure)
		return
	}
	run.relay = relay
	run.rulePaths = slices.Clone(rules.paths)
	run.ruleMatcher = rules.matcher
	manager.appendEvent(run, "info", "Starting interception relay")
	relay.SetState(intercept.GenerationState{Generation: intercept.Generation(iwanStatus.Generation), Ready: true})
	if err := relay.Start(run.ctx); err != nil {
		manager.finishStartFailure(run, fmt.Errorf("start intercept relay: %w", err))
		return
	}
	relayResult := make(chan error, 1)
	run.relayResult = relayResult
	go func() { relayResult <- relay.Run(run.ctx) }()
	select {
	case err := <-relayResult:
		manager.finishStartFailure(run, fmt.Errorf("start intercept relay: %w", err))
		return
	case <-run.ctx.Done():
		manager.finishStopped(run, run.ctx.Err())
		return
	default:
	}
	if !manager.publishRunning(run, iwanStatus.Generation) {
		manager.finishStopped(run, context.Canceled)
		return
	}
	run.completeStart(nil)
	manager.watch(run)
}

func sanitizeFlowEvent(run *managerRun, event intercept.FlowEvent) intercept.FlowEvent {
	event.Metadata.ExecutablePath = run.sanitize(event.Metadata.ExecutablePath)
	if event.Err != nil {
		event.Err = errors.New(run.sanitize(errorString(event.Err)))
	}
	return event
}

func flowEventLevel(event intercept.FlowEvent) string {
	if event.Kind == intercept.FlowEventDropped {
		return "warning"
	}
	if event.Kind == intercept.FlowEventFailed || event.Err != nil {
		return "error"
	}
	return "info"
}

func formatFlowEvent(event intercept.FlowEvent) string {
	process := event.Metadata.ExecutablePath
	if process == "" {
		process = fmt.Sprintf("pid:%d", event.Metadata.ProcessID)
	}
	identifier := "request_id=" + event.RequestID
	if event.FlowID != "" {
		identifier = "flow_id=" + event.FlowID
	}
	prefix := fmt.Sprintf(
		"[%s] protocol=%s process=%q local=%s remote=%s",
		identifier, event.Protocol, process, event.Metadata.OriginalLocal, event.Metadata.OriginalRemote,
	)
	switch event.Kind {
	case intercept.FlowEventRequest:
		if event.Bytes > 0 {
			return fmt.Sprintf("%s proxy request bytes=%d", prefix, event.Bytes)
		}
		return prefix + " proxy request"
	case intercept.FlowEventResponse:
		if event.Protocol == "TCP" {
			return fmt.Sprintf("%s first inbound data bytes=%d", prefix, event.Bytes)
		}
		return fmt.Sprintf("%s server response bytes=%d", prefix, event.Bytes)
	case intercept.FlowEventOutbound:
		return fmt.Sprintf("%s first outbound datagram bytes=%d", prefix, event.Bytes)
	case intercept.FlowEventInbound:
		return fmt.Sprintf("%s first inbound datagram bytes=%d", prefix, event.Bytes)
	case intercept.FlowEventTraffic:
		return fmt.Sprintf(
			"%s traffic outbound_packets=%d outbound_bytes=%d inbound_packets=%d inbound_bytes=%d",
			prefix, event.UploadPackets, event.UploadBytes, event.DownloadPackets, event.DownloadBytes,
		)
	case intercept.FlowEventComplete:
		var message string
		if event.Protocol == "UDP" {
			message = fmt.Sprintf(
				"%s completed outbound_packets=%d outbound_bytes=%d inbound_packets=%d inbound_bytes=%d",
				prefix, event.UploadPackets, event.UploadBytes, event.DownloadPackets, event.DownloadBytes,
			)
		} else {
			message = fmt.Sprintf("%s completed upload_bytes=%d download_bytes=%d", prefix, event.UploadBytes, event.DownloadBytes)
		}
		if event.Err != nil {
			message += " error=" + errorString(event.Err)
		}
		return message
	case intercept.FlowEventFailed:
		return prefix + " failed error=" + errorString(event.Err)
	case intercept.FlowEventDropped:
		return fmt.Sprintf("[request_id=%s] flow diagnostics dropped count=%d", event.RequestID, event.Bytes)
	default:
		return prefix + " event=" + string(event.Kind)
	}
}

func (manager *Manager) watch(run *managerRun) {
	trafficTicker := time.NewTicker(time.Second)
	defer trafficTicker.Stop()
	for {
		select {
		case <-run.ctx.Done():
			manager.finishStopped(run, run.ctx.Err())
			return
		case err := <-run.relayResult:
			if run.ctx.Err() != nil {
				manager.finishStopped(run, run.ctx.Err())
			} else {
				manager.appendEvent(run, "fatal", "Interception relay stopped: "+errorString(err))
				manager.finishFailure(run, err)
			}
			return
		case <-trafficTicker.C:
			manager.updateTraffic(run, run.relay.Traffic())
			status := manager.appendDataPathLifecycleEvents(run)
			if status.State == iwan.StateReady {
				manager.appendDataPathEvents(run, run.dataPath.observe(status, time.Now()))
			}
		case <-run.supervisor.Changes():
			if run.ctx.Err() != nil {
				manager.finishStopped(run, run.ctx.Err())
				return
			}
			status := manager.appendDataPathLifecycleEvents(run)
			switch status.State {
			case iwan.StateReady:
				manager.appendDataPathEvents(run, run.dataPath.observe(status, time.Now()))
				manager.appendEvent(run, "info", "iWAN connection is ready")
				run.relay.SetState(intercept.GenerationState{Generation: intercept.Generation(status.Generation), Ready: true})
				manager.updateRuntime(run, StateRunning, status)
			case iwan.StateConnecting, iwan.StateAuthenticating, iwan.StateRetrying:
				run.relay.SetState(intercept.GenerationState{Generation: intercept.Generation(status.Generation)})
				manager.appendDataPathEvents(run, run.dataPath.finish(status))
				manager.appendEvent(run, "warning", iwanEventMessage(status))
				manager.updateRuntime(run, StateReconnecting, status)
			case iwan.StateFailed:
				manager.appendEvent(run, "fatal", "iWAN connection failed: "+errorString(status.Err))
				manager.finishFailure(run, status.Err)
				return
			case iwan.StateStopped:
				manager.appendEvent(run, "fatal", "iWAN supervisor stopped unexpectedly")
				manager.finishFailure(run, ErrSupervisorStopped)
				return
			}
		}
	}
}

func (manager *Manager) appendDataPathLifecycleEvents(run *managerRun) iwan.Status {
	status, events, dropped := run.supervisor.DrainDataPathEvents()
	if dropped > 0 {
		manager.appendEvent(run, "warning", fmt.Sprintf("iWAN data path diagnostics dropped count=%d", dropped))
	}
	for _, event := range events {
		switch event.Phase {
		case iwan.DataPathEventStart:
			manager.appendDataPathEvents(run, run.dataPath.observe(event.Status, time.Now()))
		case iwan.DataPathEventEnd:
			manager.appendDataPathEvents(run, run.dataPath.finish(event.Status))
		}
	}
	return status
}

type dataPathEvent struct {
	status iwan.Status
	phase  string
}

type dataPathEventTracker struct {
	active       bool
	generation   uint64
	lastObserved iwan.DataPathStats
	lastLoggedAt time.Time
}

func (tracker *dataPathEventTracker) observe(status iwan.Status, now time.Time) []dataPathEvent {
	if status.State != iwan.StateReady || status.Generation == 0 {
		return tracker.finish(status)
	}
	var events []dataPathEvent
	if tracker.active && tracker.generation != status.Generation {
		events = append(events, tracker.finish(status)...)
	}
	if !tracker.active {
		tracker.active = true
		tracker.generation = status.Generation
		tracker.lastObserved = status.DataPath
		tracker.lastLoggedAt = now
		return append(events, dataPathEvent{status: status, phase: "generation-start"})
	}
	changed := status.DataPath != tracker.lastObserved
	anomalyChanged := dataPathAnomalies(status.DataPath) != dataPathAnomalies(tracker.lastObserved)
	tracker.lastObserved = status.DataPath
	if !changed {
		return events
	}
	phase := ""
	if anomalyChanged {
		phase = "anomaly"
	} else if now.Sub(tracker.lastLoggedAt) >= dataPathLogInterval {
		phase = "periodic"
	}
	if phase != "" {
		tracker.lastLoggedAt = now
		events = append(events, dataPathEvent{status: status, phase: phase})
	}
	return events
}

func (tracker *dataPathEventTracker) finish(status iwan.Status) []dataPathEvent {
	if !tracker.active {
		return nil
	}
	if status.Generation == tracker.generation {
		tracker.lastObserved = status.DataPath
	}
	finalStatus := status
	finalStatus.Generation = tracker.generation
	finalStatus.DataPath = tracker.lastObserved
	tracker.active = false
	return []dataPathEvent{{status: finalStatus, phase: "generation-end"}}
}

type dataPathAnomalyCounters struct {
	malformed              uint64
	invalidIPv4            uint64
	destinationUnreachable uint64
	portUnreachable        uint64
}

func dataPathAnomalies(stats iwan.DataPathStats) dataPathAnomalyCounters {
	return dataPathAnomalyCounters{
		malformed:              stats.InboundMalformedDatagrams,
		invalidIPv4:            stats.InboundInvalidIPv4Packets,
		destinationUnreachable: stats.InboundICMPDestinationUnreachablePackets,
		portUnreachable:        stats.InboundICMPPortUnreachablePackets,
	}
}

func (manager *Manager) appendDataPathEvents(run *managerRun, events []dataPathEvent) {
	for _, event := range events {
		manager.appendStructuredEvent(run, "info", formatDataPathEvent(event.status, event.phase))
	}
}

func formatDataPathEvent(status iwan.Status, phase string) string {
	stats := status.DataPath
	return fmt.Sprintf(
		"iWAN data path generation=%d phase=%s outbound_stack_packets=%d outbound_stack_bytes=%d outbound_udp_packets=%d outbound_node_datagrams=%d outbound_node_bytes=%d inbound_node_datagrams=%d inbound_node_bytes=%d inbound_data_datagrams=%d inbound_data_bytes=%d inbound_fragment_datagrams=%d inbound_fragment_bytes=%d inbound_fragment_completed_packets=%d inbound_malformed_datagrams=%d inbound_invalid_ipv4_packets=%d inbound_stack_packets=%d inbound_stack_bytes=%d inbound_tcp_packets=%d inbound_udp_packets=%d inbound_icmp_packets=%d inbound_other_packets=%d inbound_icmp_destination_unreachable_packets=%d inbound_icmp_port_unreachable_packets=%d",
		status.Generation,
		phase,
		stats.OutboundStackPackets,
		stats.OutboundStackBytes,
		stats.OutboundUDPPackets,
		stats.OutboundNodeDatagrams,
		stats.OutboundNodeBytes,
		stats.InboundNodeDatagrams,
		stats.InboundNodeBytes,
		stats.InboundDataDatagrams,
		stats.InboundDataBytes,
		stats.InboundFragmentDatagrams,
		stats.InboundFragmentBytes,
		stats.InboundFragmentCompletedPackets,
		stats.InboundMalformedDatagrams,
		stats.InboundInvalidIPv4Packets,
		stats.InboundStackPackets,
		stats.InboundStackBytes,
		stats.InboundTCPPackets,
		stats.InboundUDPPackets,
		stats.InboundICMPPackets,
		stats.InboundOtherPackets,
		stats.InboundICMPDestinationUnreachablePackets,
		stats.InboundICMPPortUnreachablePackets,
	)
}

func iwanEventMessage(status iwan.Status) string {
	switch status.State {
	case iwan.StateConnecting:
		return "Reconnecting to iWAN server"
	case iwan.StateAuthenticating:
		return "Authenticating with iWAN server"
	case iwan.StateRetrying:
		return "iWAN connection lost; retrying: " + errorString(status.Err)
	default:
		return "iWAN connection state changed"
	}
}

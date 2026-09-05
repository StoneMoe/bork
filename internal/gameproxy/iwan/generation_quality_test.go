//go:build game_proxy

package iwan

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"bork/internal/gameproxy/netstack"
)

func TestGeneration_echoMatchesOnceBeforeDeadline(t *testing.T) {
	owner := &Supervisor{desired: true, status: Status{State: StateReady, Generation: 1}}
	current := generation{id: 1, owner: owner, timings: defaultRuntimeTimings(), echo: Echo{MinimumDelay: ^uint32(0)}}
	sent := time.Now()
	current.echoSent = sent
	current.recordEchoResponse(sent.Add(-time.Microsecond), sent.Add(time.Millisecond))
	if len(owner.linkHistory) != 0 || current.echoSent.IsZero() {
		t.Fatal("unmatched response completed the probe")
	}
	// The wire timestamp has neither nanosecond precision nor a monotonic clock.
	wireSent := time.UnixMicro(sent.UnixMicro())
	delay := 12*time.Millisecond + 345*time.Nanosecond
	current.recordEchoResponse(wireSent, sent.Add(delay))
	current.recordEchoResponse(wireSent, sent.Add(20*time.Millisecond))
	quality := owner.linkQualityLocked(sent.Add(time.Second))
	if len(quality.History) != 1 || quality.RTTMillis == nil || *quality.RTTMillis != float64(delay)/float64(time.Millisecond) ||
		quality.LossPercent == nil || *quality.LossPercent != 0 || current.echo.CurrentDelay != 12_000 {
		t.Fatalf("matched/duplicate quality = %#v, wire = %#v", quality, current.echo)
	}
	wireStats := current.echo
	current.echoSent = sent.Add(2 * time.Second)
	wireSent = time.UnixMicro(current.echoSent.UnixMicro())
	deadline := sent.Add(4 * time.Second)
	current.recordEchoResponse(wireSent, deadline)
	current.recordEchoResponse(wireSent, deadline.Add(time.Millisecond))
	quality = owner.linkQualityLocked(deadline)
	if len(quality.History) != 2 || quality.RTTMillis != nil || quality.LossPercent == nil || *quality.LossPercent != 50 ||
		!quality.History[1].At.Equal(deadline) || quality.History[1].RTTMillis != nil || current.echo != wireStats {
		t.Fatalf("deadline/late quality = %#v, wire = %#v", quality, current.echo)
	}
	current.echoSent = deadline
	current.recordEchoResponse(wireSent, deadline.Add(time.Millisecond))
	if len(owner.linkHistory) != 2 || current.echoSent.IsZero() {
		t.Fatal("old response completed a new probe")
	}
}

func TestGeneration_echoTelemetryDoesNotTightenLiveness(t *testing.T) {
	session := goldenSession(t)
	owner := &Supervisor{desired: true, status: Status{State: StateReady, Generation: 1}}
	current := generation{id: 1, session: session, owner: owner, timings: defaultRuntimeTimings()}
	request := current.buildEchoRequest(time.Now())
	response, err := BuildEchoResponse(session, request)
	if err != nil {
		t.Fatal(err)
	}
	response[1], response[2] = 0xff, response[2]^1
	signControl(response)
	for _, packet := range [][]byte{response[:signedHeaderSize], response[:signedHeaderSize+8], response} {
		if valid, err := current.handleInbound(packet); !valid || err != nil {
			t.Fatalf("legacy ECHO liveness = %v, %v", valid, err)
		}
	}
	if len(owner.linkHistory) != 0 {
		t.Fatal("unsolicited legacy responses produced telemetry")
	}
	current.echoSent = time.Now()
	request = current.buildEchoRequest(current.echoSent)
	response, err = BuildEchoResponse(session, request)
	if err != nil {
		t.Fatal(err)
	}
	response[1], response[2] = 0xff, response[2]^1
	signControl(response)
	if valid, err := current.handleInbound(response[:signedHeaderSize+8]); !valid || err != nil || len(owner.linkHistory) != 1 {
		t.Fatalf("matched short legacy ECHO = %v, %v, samples %d", valid, err, len(owner.linkHistory))
	}
}

func TestGeneration_echoExpiresBeforeNextSendAndIgnoresWriteFailure(t *testing.T) {
	server := newFakeReferenceServer(t)
	connection, err := net.DialUDP("udp4", nil, server.connection.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	owner := &Supervisor{desired: true, status: Status{State: StateReady, Generation: 1}}
	current := generation{id: 1, session: goldenSession(t), connection: connection, owner: owner, timings: defaultRuntimeTimings()}
	now := time.Now()
	if err := current.sendEcho(now); err != nil {
		t.Fatal(err)
	}
	if len(owner.linkHistory) != 0 || current.echoSent != now {
		t.Fatal("unfinished probe affected loss or lost its monotonic send time")
	}
	next := now.Add(current.timings.echoInterval)
	if err := current.sendEcho(next); err != nil {
		t.Fatal(err)
	}
	if len(owner.linkHistory) != 1 || owner.linkHistory[0].RTTMillis != nil || owner.linkHistory[0].LossPercent != 100 || current.echoSent != next {
		t.Fatal("next send did not complete exactly the previous timeout")
	}
	current.recordEchoResponse(time.UnixMicro(next.UnixMicro()), next.Add(time.Millisecond))
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := current.sendEcho(next.Add(current.timings.echoInterval)); err == nil {
		t.Fatal("closed socket write succeeded")
	}
	if !current.echoSent.IsZero() || len(owner.linkHistory) != 2 || owner.linkHistory[1].LossPercent != 50 {
		t.Fatal("local write failure entered the loss denominator")
	}
}

func TestGeneration_exitSettlesOnlyExpiredEcho(t *testing.T) {
	for _, exit := range []string{"close", "read-error", "cancel", "stop"} {
		for _, age := range []string{"unfinished", "expired"} {
			t.Run(exit+"/"+age, func(t *testing.T) {
				server := newFakeReferenceServer(t)
				connection, err := net.DialUDP("udp4", nil, server.connection.LocalAddr().(*net.UDPAddr))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = connection.Close() })
				network, err := netstack.New(netstack.Config{
					Address: netip.MustParseAddr("10.20.30.40"), MTU: 1400, QueueSize: 8, Generation: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(network.Close)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				done := make(chan struct{})
				owner := &Supervisor{
					desired: true, nextID: 1, status: Status{State: StateReady, Generation: 1}, cancel: cancel, runDone: done,
				}
				current := generation{
					id: 1, owner: owner, network: network, session: goldenSession(t), connection: connection,
					timings: runtimeTimings{echoInterval: time.Hour, liveness: time.Hour}, readEvents: make(chan datagramEvent),
				}
				// Offset the send instant rather than racing an ECHO timer at teardown.
				sent := time.Now()
				if age == "expired" {
					sent = sent.Add(-time.Hour - time.Second)
				}
				if err := current.sendEcho(sent); err != nil {
					t.Fatal(err)
				}
				server.next(t)
				var runErr error
				go func() {
					runErr = current.runEstablished(ctx)
					current.workers.Wait()
					close(done)
				}()
				t.Cleanup(owner.Stop)
				// An unbuffered event confirms the established loop is live before exit.
				select {
				case current.readEvents <- datagramEvent{}:
				case <-done:
					t.Fatal("established loop exited before the gate")
				}
				wantErr := error(context.Canceled)
				switch exit {
				case "close":
					current.readEvents <- datagramEvent{packet: BuildClose(current.session)}
					wantErr = ErrPeerClosed
				case "read-error":
					current.readEvents <- datagramEvent{err: net.ErrClosed}
					wantErr = ErrSocketFailure
				case "cancel":
					cancel()
				case "stop":
					owner.Stop()
				}
				<-done
				if !errors.Is(runErr, wantErr) || !current.echoSent.IsZero() {
					t.Fatalf("exit = %v, pending ECHO = %v", runErr, current.echoSent)
				}
				history := owner.Status().Quality.History
				if age == "expired" && exit != "stop" {
					if len(history) != 1 || history[0].RTTMillis != nil || history[0].LossPercent != 100 ||
						!history[0].At.Equal(sent.Add(current.timings.echoInterval)) {
						t.Fatalf("expired ECHO was not settled at its deadline: %#v", history)
					}
				} else if len(history) != 0 {
					t.Fatalf("unfinished ECHO or manual Stop produced a sample: %#v", history)
				}
			})
		}
	}
}

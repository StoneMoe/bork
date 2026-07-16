package peer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/media"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"

	"github.com/thesyncim/gopus"
)

func TestClientsDiscoverAuthenticateAndExchangeVoiceOnSameHost(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	firstDevice, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	secondDevice, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate() second error = %v", err)
	}
	room, err := invite.New("runtime test")
	if err != nil {
		t.Fatalf("invite.New() error = %v", err)
	}
	options := endpoint.Options{ListenAddress: "[::]:0", STUNServers: []string{}, STUNRefresh: 0}
	firstClient := NewClient(firstDevice, room, options, logger)
	secondClient := NewClient(secondDevice, room, options, logger)
	firstFlow := media.NewFlow()
	secondFlow := media.NewFlow()
	firstChanges := firstClient.StateChanges()
	secondChanges := secondClient.StateChanges()
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- firstClient.Loop(ctx, firstFlow) }()
	go func() { secondDone <- secondClient.Loop(ctx, secondFlow) }()

	waitForAuthenticatedRemotePeer(t, firstClient, firstChanges, secondDevice.PeerID())
	waitForAuthenticatedRemotePeer(t, secondClient, secondChanges, firstDevice.PeerID())
	encoder, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: 48000, Channels: 1, Application: gopus.ApplicationVoIP})
	if err != nil {
		t.Fatalf("gopus.NewEncoder() error = %v", err)
	}
	if err := encoder.SetFrameSize(480); err != nil {
		t.Fatalf("SetFrameSize() error = %v", err)
	}
	pcm := make([]float32, 480)
	for index := range pcm {
		pcm[index] = float32(math.Sin(float64(index)*0.03)) * 0.1
	}
	voiceBuffer := make([]byte, protocol.MaxVoicePayload)
	voiceLength, err := encoder.Encode(pcm, voiceBuffer)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	voicePayload := voiceBuffer[:voiceLength]
	generation := firstFlow.Reset()
	if !firstFlow.SubmitSend(media.SendFrame{Timestamp: 960, Payload: voicePayload, Deadline: time.Now().Add(time.Second), Generation: generation}) {
		t.Fatal("SubmitSend() rejected voice frame")
	}
	voiceCtx, stopVoice := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopVoice()
	var frame media.ReceivedFrame
	var ok bool
	select {
	case <-voiceCtx.Done():
	case <-secondFlow.ReceivedReady():
		frame, ok = secondFlow.TakeReceived()
	}
	if ok {
		if frame.SourceID != firstDevice.PeerID() || frame.Timestamp != 960 || !bytes.Equal(frame.Payload, voicePayload) {
			t.Fatalf("received voice frame = %#v", frame)
		}
		decoder, err := gopus.NewDecoder(gopus.DecoderConfig{SampleRate: 48000, Channels: 1, MaxPacketSamples: 480, MaxPacketBytes: protocol.MaxVoicePayload})
		if err != nil {
			t.Fatalf("gopus.NewDecoder() error = %v", err)
		}
		decoded := make([]float32, 480)
		if count, err := decoder.Decode(frame.Payload, decoded); err != nil || count != 480 {
			t.Fatalf("Decode() = %d, %v", count, err)
		}
	} else {
		t.Fatal("timed out waiting for voice frame")
	}
	cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first local peer error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second local peer error = %v", err)
	}
}

func TestClientsDiscoverAcrossProcessesOnSameHost(t *testing.T) {
	room, err := invite.New("process discovery test")
	if err != nil {
		t.Fatalf("invite.New() error = %v", err)
	}
	root := t.TempDir()
	firstConnected := filepath.Join(root, "first.connected")
	secondConnected := filepath.Join(root, "second.connected")
	type result struct {
		name   string
		output string
		err    error
	}
	results := make(chan result, 2)
	start := func(name, ownMarker, otherMarker string) {
		command := exec.Command(os.Args[0], "-test.run=^TestClientDiscoveryHelper$")
		command.Env = append(os.Environ(),
			"BORK_NODE_DISCOVERY_HELPER=1",
			"BORK_NODE_DISCOVERY_INVITE="+room.Encode(),
			"BORK_NODE_DISCOVERY_DATA_DIR="+filepath.Join(root, name),
			"BORK_NODE_DISCOVERY_OWN_MARKER="+ownMarker,
			"BORK_NODE_DISCOVERY_OTHER_MARKER="+otherMarker,
		)
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Start(); err != nil {
			results <- result{name: name, err: err}
			return
		}
		go func() {
			err := command.Wait()
			results <- result{name: name, output: output.String(), err: err}
		}()
	}
	start("first", firstConnected, secondConnected)
	start("second", secondConnected, firstConnected)

	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("%s helper error = %v\n%s", result.name, result.err, result.output)
			}
		case <-time.After(12 * time.Second):
			t.Fatal("timed out waiting for same-host peer processes")
		}
	}
	if _, err := os.Stat(firstConnected); err != nil {
		t.Fatalf("first peer did not connect: %v", err)
	}
	if _, err := os.Stat(secondConnected); err != nil {
		t.Fatalf("second peer did not connect: %v", err)
	}
}

func TestClientDiscoveryHelper(t *testing.T) {
	if os.Getenv("BORK_NODE_DISCOVERY_HELPER") != "1" {
		return
	}
	localIdentity, err := identity.LoadOrCreate(os.Getenv("BORK_NODE_DISCOVERY_DATA_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := identity.Acquire(localIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	room, err := invite.Parse(os.Getenv("BORK_NODE_DISCOVERY_INVITE"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewClient(localIdentity, room, endpoint.Options{ListenAddress: "[::]:0", STUNServers: []string{}, STUNRefresh: 0}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Loop(ctx, nil) }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	connected := false
	for {
		select {
		case <-ticker.C:
			if !connected && len(client.Snapshot().RemotePeers) > 0 {
				if err := os.WriteFile(os.Getenv("BORK_NODE_DISCOVERY_OWN_MARKER"), []byte("connected"), 0o600); err != nil {
					t.Fatal(err)
				}
				connected = true
			}
			if connected {
				if _, err := os.Stat(os.Getenv("BORK_NODE_DISCOVERY_OTHER_MARKER")); err == nil {
					cancel()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					return
				}
			}
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			t.Fatalf("client stopped before both processes connected; state = %#v", client.Snapshot())
		case <-ctx.Done():
			<-done
			t.Fatalf("timed out waiting for both processes to connect; state = %#v", client.Snapshot())
		}
	}
}

func waitForAuthenticatedRemotePeer(t *testing.T, client *Client, changes <-chan struct{}, peerID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-changes:
			for _, remotePeer := range client.Snapshot().RemotePeers {
				if remotePeer.PeerID == peerID && remotePeer.RTTMillis > 0 {
					return
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for authenticated peer %s", peerID)
		}
	}
}

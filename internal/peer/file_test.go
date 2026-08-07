package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
)

func TestFileCodecsAreCanonical(t *testing.T) {
	digest := sha256.Sum256([]byte("contents"))
	transfer := &fileTransfer{id: [16]byte{1}, name: "report.txt", size: 8, digest: digest}
	offerPayload, err := encodeFileOffer(transfer)
	if err != nil {
		t.Fatal(err)
	}
	kind, id, value, offer, err := decodeFileControl(offerPayload)
	if err != nil || kind != fileControlOffer || id != transfer.id || value != 0 || offer.name != transfer.name || offer.size != transfer.size || offer.digest != digest {
		t.Fatalf("decoded offer = %d %x %d %#v, %v", kind, id, value, offer, err)
	}
	if _, _, _, _, err := decodeFileControl(append(offerPayload, 0)); err == nil {
		t.Fatal("offer with trailing data was accepted")
	}

	dataPayload := encodeFileData(transfer.id, 32, []byte("chunk"))
	gotID, offset, data, err := decodeFileData(dataPayload)
	if err != nil || gotID != transfer.id || offset != 32 || !bytes.Equal(data, []byte("chunk")) {
		t.Fatalf("decoded data = %x %d %q, %v", gotID, offset, data, err)
	}
	if _, _, _, err := decodeFileData(append(dataPayload, 0)); err == nil {
		t.Fatal("data with trailing byte was accepted")
	}
	if _, _, _, _, err := decodeFileControl([]byte{fileProtocolVersion, fileControlAccept}); err == nil {
		t.Fatal("truncated control was accepted")
	}
}

func TestFileCommandBeforeLoopReturns(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	if _, err := client.OfferFile("peer", "file"); err == nil {
		t.Fatal("OfferFile() before Loop succeeded")
	}
}

func TestClientsTransferAndCancelFiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	firstDevice, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondDevice, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room, err := invite.New("file transfer test")
	if err != nil {
		t.Fatal(err)
	}
	options := endpoint.Options{ListenAddress: "[::]:0", STUNServers: []string{}, STUNRefresh: 0}
	first := NewClient(firstDevice, room, networking.Options{Endpoint: options}, logger)
	second := NewClient(secondDevice, room, networking.Options{Endpoint: options}, logger)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- first.Loop(ctx, nil) }()
	go func() { secondDone <- second.Loop(ctx, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-firstDone; err != nil {
			t.Errorf("first client: %v", err)
		}
		if err := <-secondDone; err != nil {
			t.Errorf("second client: %v", err)
		}
	})
	waitForAuthenticatedRemotePeer(t, first, first.StateChanges(), secondDevice.PeerID())
	waitForAuthenticatedRemotePeer(t, second, second.StateChanges(), firstDevice.PeerID())

	sourceData := make([]byte, 1<<20+17)
	for index := range sourceData {
		sourceData[index] = byte(index * 31)
	}
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, sourceData, 0o600); err != nil {
		t.Fatal(err)
	}
	transferID, err := first.OfferFile(secondDevice.PeerID(), source)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, second, transferID, func(transfer FileTransferSnapshot) bool {
		return transfer.Status == "offered" && transfer.PeerID == firstDevice.PeerID() && transfer.Size == uint64(len(sourceData))
	})
	destination := filepath.Join(t.TempDir(), "received.bin")
	if err := second.AcceptFile(transferID, destination); err != nil {
		t.Fatal(err)
	}
	received := waitForFileTransfer(t, second, transferID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "completed" })
	sent := waitForFileTransfer(t, first, transferID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "completed" })
	if received.Transferred != uint64(len(sourceData)) || sent.Transferred != uint64(len(sourceData)) {
		t.Fatalf("progress received=%d sent=%d want=%d", received.Transferred, sent.Transferred, len(sourceData))
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, sourceData) {
		t.Fatalf("received content length=%d, err=%v", len(got), err)
	}
	existingSource := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(existingSource, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	existingID, err := first.OfferFile(secondDevice.PeerID(), existingSource)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, second, existingID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "offered" })
	existingDestination := filepath.Join(t.TempDir(), "existing.bin")
	if err := os.WriteFile(existingDestination, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := second.AcceptFile(existingID, existingDestination); err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, second, existingID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "failed" })
	if existing, err := os.ReadFile(existingDestination); err != nil || string(existing) != "keep" {
		t.Fatalf("exclusive destination changed existing file to %q: %v", existing, err)
	}
	waitForFileTransfer(t, first, existingID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "canceled" })

	cancelData := bytes.Repeat([]byte("cancel-me"), 512<<10)
	cancelSource := filepath.Join(t.TempDir(), "cancel-source.bin")
	if err := os.WriteFile(cancelSource, cancelData, 0o600); err != nil {
		t.Fatal(err)
	}
	cancelID, err := first.OfferFile(secondDevice.PeerID(), cancelSource)
	if err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, second, cancelID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "offered" })
	cancelDestination := filepath.Join(t.TempDir(), "partial.bin")
	if err := second.AcceptFile(cancelID, cancelDestination); err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, second, cancelID, func(transfer FileTransferSnapshot) bool {
		return transfer.Status == "transferring" && transfer.Transferred > 0
	})
	if err := second.CancelFile(cancelID); err != nil {
		t.Fatal(err)
	}
	waitForFileTransfer(t, first, cancelID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "canceled" })
	waitForFileTransfer(t, second, cancelID, func(transfer FileTransferSnapshot) bool { return transfer.Status == "canceled" })
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(cancelDestination)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial destination was not removed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForFileTransfer(t *testing.T, client *Client, transferID string, condition func(FileTransferSnapshot) bool) FileTransferSnapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := client.StateSnapshot()
		for _, transfer := range snapshot.Transfers {
			if transfer.ID == transferID && condition(transfer) {
				return transfer
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, _ := client.StateSnapshot()
	t.Fatalf("timed out waiting for transfer %s: %#v", transferID, snapshot.Transfers)
	return FileTransferSnapshot{}
}

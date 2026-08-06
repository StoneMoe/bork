package peer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	reliableChannelFileControl = 6
	reliableChannelFileData    = 7
	fileProtocolVersion        = 1
	fileChunkSize              = 32 << 10
	MaxFileTransferBytes       = 1 << 30
	maxFileTransferRecords     = 32
	fileOfferTimeout           = 10 * time.Minute
	fileTransferTimeout        = 2 * time.Minute
	maxFileNameBytes           = 255
)

const (
	fileControlOffer byte = iota + 1
	fileControlAccept
	fileControlReject
	fileControlCancel
	fileControlACK
)

type FileTransferSnapshot struct {
	ID          string
	PeerID      string
	Direction   string
	Name        string
	Path        string
	Status      string
	Size        uint64
	Transferred uint64
	SHA256      string
	Error       string
}

type fileCommandKind byte

const (
	fileCommandOffer fileCommandKind = iota + 1
	fileCommandAccept
	fileCommandReject
	fileCommandCancel
)

type fileCommand struct {
	kind       fileCommandKind
	peerID     string
	path       string
	transferID string
	result     chan fileCommandResult
}

type fileCommandResult struct {
	transferID string
	err        error
}

type fileWorkKind byte

const (
	fileWorkOffer fileWorkKind = iota + 1
	fileWorkCreate
	fileWorkRead
	fileWorkWrite
	fileWorkFinish
)

type fileWorkResult struct {
	kind       fileWorkKind
	transferID string
	size       uint64
	digest     [sha256.Size]byte
	data       []byte
	file       *os.File
	err        error
}

type fileTransfer struct {
	id          [16]byte
	peerID      string
	sessionID   [16]byte
	direction   string
	name        string
	path        string
	status      string
	size        uint64
	transferred uint64
	digest      [sha256.Size]byte
	errorText   string
	updatedAt   time.Time
	file        *os.File
	hasher      hash.Hash
	working     bool
	workContext context.Context
	cancelWork  context.CancelFunc
}

func (c *Client) OfferFile(peerID, path string) (string, error) {
	return c.requestFileCommand(fileCommand{kind: fileCommandOffer, peerID: peerID, path: path, result: make(chan fileCommandResult, 1)})
}

func (c *Client) AcceptFile(transferID, destination string) error {
	_, err := c.requestFileCommand(fileCommand{kind: fileCommandAccept, transferID: transferID, path: destination, result: make(chan fileCommandResult, 1)})
	return err
}

func (c *Client) RejectFile(transferID string) error {
	_, err := c.requestFileCommand(fileCommand{kind: fileCommandReject, transferID: transferID, result: make(chan fileCommandResult, 1)})
	return err
}

func (c *Client) CancelFile(transferID string) error {
	_, err := c.requestFileCommand(fileCommand{kind: fileCommandCancel, transferID: transferID, result: make(chan fileCommandResult, 1)})
	return err
}

func (c *Client) requestFileCommand(command fileCommand) (string, error) {
	if !c.started.Load() {
		return "", errors.New("peer client is not running")
	}
	select {
	case <-c.loopReady:
	case <-c.loopDone:
		return "", errors.New("peer client is not running")
	}
	select {
	case c.fileCommands <- command:
	case <-c.loopDone:
		return "", errors.New("peer client is not running")
	}
	select {
	case result := <-command.result:
		return result.transferID, result.err
	case <-c.loopDone:
		return "", errors.New("peer client stopped")
	}
}

func (c *Client) handleFileCommand(command fileCommand) {
	result := fileCommandResult{transferID: command.transferID}
	switch command.kind {
	case fileCommandOffer:
		result.transferID, result.err = c.startFileOffer(command.peerID, command.path)
	case fileCommandAccept:
		result.err = c.acceptFile(command.transferID, command.path)
	case fileCommandReject:
		result.err = c.rejectFile(command.transferID)
	case fileCommandCancel:
		result.err = c.cancelFile(command.transferID, true, "canceled")
	default:
		result.err = errors.New("file command is invalid")
	}
	command.result <- result
}

func (c *Client) startFileOffer(peerID, path string) (string, error) {
	peer := c.remotePeers[peerID]
	if peer == nil || peer.session == nil || !peer.session.authenticated {
		return "", errors.New("recipient is not authenticated")
	}
	if c.activeFileTransfer(peerID, "outgoing") != nil {
		return "", errors.New("an outgoing file transfer is already active for this peer")
	}
	if c.activeFileTransferCount() >= maxFileTransferRecords {
		return "", errors.New("too many active file transfers")
	}
	if path == "" {
		return "", errors.New("source path is empty")
	}
	id, err := newFileTransferID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	workContext, cancelWork := context.WithCancel(c.fileContext)
	transfer := &fileTransfer{id: id, peerID: peerID, sessionID: peer.session.sessionID, direction: "outgoing", path: path, name: filepath.Base(path), status: "preparing", updatedAt: now, working: true, workContext: workContext, cancelWork: cancelWork}
	c.fileTransfers[id] = transfer
	c.startFileWork(fileWorkResult{kind: fileWorkOffer, transferID: hex.EncodeToString(id[:])}, func(result *fileWorkResult) {
		file, err := os.Open(path)
		if err != nil {
			result.err = err
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxFileTransferBytes {
			result.err = errors.New("source must be a regular file no larger than 1 GiB")
			return
		}
		h := sha256.New()
		buffer := make([]byte, fileChunkSize)
		remaining := info.Size()
		for remaining > 0 {
			if transfer.workContext.Err() != nil {
				result.err = transfer.workContext.Err()
				return
			}
			count, readErr := file.Read(buffer[:min(int64(len(buffer)), remaining)])
			if count > 0 {
				_, _ = h.Write(buffer[:count])
				remaining -= int64(count)
			}
			if readErr != nil {
				result.err = io.ErrUnexpectedEOF
				return
			}
		}
		result.size = uint64(info.Size())
		copy(result.digest[:], h.Sum(nil))
	})
	c.publishStateChange()
	return hex.EncodeToString(id[:]), nil
}

func (c *Client) acceptFile(encodedID, destination string) error {
	transfer := c.fileTransferByString(encodedID)
	if transfer == nil || transfer.direction != "incoming" || transfer.status != "offered" {
		return errors.New("file offer is not pending")
	}
	if c.activeAcceptedFileTransfer(transfer.peerID) != nil {
		return errors.New("an incoming file transfer is already accepted for this peer")
	}
	if destination == "" {
		return errors.New("destination path is empty")
	}
	transfer.path, transfer.status, transfer.updatedAt, transfer.working = destination, "accepting", time.Now(), true
	c.startFileWork(fileWorkResult{kind: fileWorkCreate, transferID: encodedID}, func(result *fileWorkResult) {
		result.file, result.err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	})
	c.publishStateChange()
	return nil
}

func (c *Client) rejectFile(encodedID string) error {
	transfer := c.fileTransferByString(encodedID)
	if transfer == nil || transfer.direction != "incoming" || transfer.status != "offered" {
		return errors.New("file offer is not pending")
	}
	c.queueFileControl(transfer, fileControlReject, 0)
	c.finishFileTransfer(transfer, "rejected", "")
	return nil
}

func (c *Client) cancelFile(encodedID string, notify bool, reason string) error {
	transfer := c.fileTransferByString(encodedID)
	if transfer == nil || fileTerminal(transfer.status) {
		return errors.New("file transfer is not active")
	}
	if notify {
		c.queueFileControl(transfer, fileControlCancel, 0)
	}
	c.finishFileTransfer(transfer, "canceled", reason)
	return nil
}

func (c *Client) startFileWork(result fileWorkResult, work func(*fileWorkResult)) {
	c.fileWorkers.Add(1)
	go func() {
		defer c.fileWorkers.Done()
		work(&result)
		select {
		case c.fileWorkResults <- result:
		case <-c.fileContext.Done():
			if result.file != nil {
				_ = result.file.Close()
			}
		}
	}()
}

func (c *Client) handleFileWorkResult(result fileWorkResult) {
	transfer := c.fileTransferByString(result.transferID)
	if transfer == nil || fileTerminal(transfer.status) {
		if result.file != nil {
			c.closePartialFile(result.file, "")
		}
		return
	}
	transfer.working = false
	if result.err != nil {
		c.queueFileControl(transfer, fileControlCancel, 0)
		c.finishFileTransfer(transfer, "failed", result.err.Error())
		return
	}
	switch result.kind {
	case fileWorkOffer:
		if transfer.status != "preparing" || !validFileName(transfer.name) {
			c.finishFileTransfer(transfer, "failed", "source filename is invalid")
			return
		}
		transfer.size, transfer.digest, transfer.status, transfer.updatedAt = result.size, result.digest, "offered", time.Now()
		payload, err := encodeFileOffer(transfer)
		if err != nil || c.queueFilePayload(transfer, reliableChannelFileControl, true, payload) != nil {
			c.finishFileTransfer(transfer, "failed", "could not queue file offer")
			return
		}
	case fileWorkCreate:
		if transfer.status != "accepting" {
			c.closePartialFile(result.file, transfer.path)
			return
		}
		transfer.file, transfer.hasher, transfer.status, transfer.updatedAt = result.file, sha256.New(), "transferring", time.Now()
		if err := c.queueFileControl(transfer, fileControlAccept, 0); err != nil {
			c.finishFileTransfer(transfer, "failed", err.Error())
		} else if transfer.size == 0 {
			if transfer.digest != sha256.Sum256(nil) {
				c.queueFileControl(transfer, fileControlCancel, 0)
				c.finishFileTransfer(transfer, "failed", "SHA-256 mismatch")
				break
			}
			transfer.working = true
			file := transfer.file
			c.startFileWork(fileWorkResult{kind: fileWorkFinish, transferID: result.transferID}, func(result *fileWorkResult) {
				if err := file.Sync(); err != nil {
					result.err = err
				} else {
					result.err = file.Close()
				}
			})
		}
	case fileWorkRead:
		if transfer.status != "transferring" {
			return
		}
		payload := encodeFileData(transfer.id, transfer.transferred, result.data)
		if err := c.queueFilePayload(transfer, reliableChannelFileData, false, payload); err != nil {
			c.finishFileTransfer(transfer, "failed", err.Error())
			return
		}
		transfer.status, transfer.updatedAt = "waiting", time.Now()
	case fileWorkWrite:
		if transfer.status != "transferring" {
			return
		}
		_, _ = transfer.hasher.Write(result.data)
		transfer.transferred += uint64(len(result.data))
		transfer.updatedAt = time.Now()
		if transfer.transferred == transfer.size {
			if got := transfer.hasher.Sum(nil); !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(transfer.digest[:])) {
				c.queueFileControl(transfer, fileControlCancel, 0)
				c.finishFileTransfer(transfer, "failed", "SHA-256 mismatch")
				return
			}
			transfer.working = true
			file := transfer.file
			c.startFileWork(fileWorkResult{kind: fileWorkFinish, transferID: result.transferID}, func(result *fileWorkResult) {
				if err := file.Sync(); err != nil {
					result.err = err
				} else {
					result.err = file.Close()
				}
			})
			return
		}
		if err := c.queueFileControl(transfer, fileControlACK, transfer.transferred); err != nil {
			c.finishFileTransfer(transfer, "failed", err.Error())
		}
	case fileWorkFinish:
		transfer.file = nil
		if transfer.status != "transferring" {
			return
		}
		if err := c.queueFileControl(transfer, fileControlACK, transfer.transferred); err != nil {
			c.finishFileTransfer(transfer, "failed", err.Error())
			return
		}
		c.finishFileTransfer(transfer, "completed", "")
	}
	c.publishStateChange()
}

func (c *Client) handleFileMessage(sender *RemotePeer, message deliveredReliableMessage) {
	if sender == nil || sender.session == nil {
		return
	}
	if message.channel == reliableChannelFileControl {
		kind, id, value, offer, err := decodeFileControl(message.payload)
		if err != nil {
			return
		}
		if kind == fileControlOffer {
			c.receiveFileOffer(sender, id, offer)
			return
		}
		transfer := c.fileTransfers[id]
		if transfer == nil || transfer.peerID != sender.identity.PeerID() || transfer.sessionID != sender.session.sessionID || fileTerminal(transfer.status) {
			return
		}
		switch kind {
		case fileControlAccept:
			if transfer.direction == "outgoing" && transfer.status == "offered" {
				transfer.status, transfer.updatedAt = "waiting", time.Now()
				if transfer.size != 0 {
					transfer.status = "transferring"
					c.readNextFileChunk(transfer)
				}
			}
		case fileControlReject:
			if transfer.direction == "outgoing" && transfer.status == "offered" {
				c.finishFileTransfer(transfer, "rejected", "")
			}
		case fileControlCancel:
			c.finishFileTransfer(transfer, "canceled", "canceled by peer")
		case fileControlACK:
			if transfer.direction == "outgoing" && transfer.status == "waiting" && value >= transfer.transferred && value <= transfer.size && value-transfer.transferred <= fileChunkSize && (value > transfer.transferred || transfer.size == 0) {
				sender.session.reliable.discardOutboundChannel(reliableChannelFileData)
				transfer.transferred, transfer.updatedAt = value, time.Now()
				if value == transfer.size {
					c.finishFileTransfer(transfer, "completed", "")
				} else {
					transfer.status = "transferring"
					c.readNextFileChunk(transfer)
				}
			}
		}
		c.publishStateChange()
		return
	}
	if message.channel != reliableChannelFileData {
		return
	}
	id, offset, data, err := decodeFileData(message.payload)
	transfer := c.fileTransfers[id]
	if err != nil || transfer == nil || transfer.direction != "incoming" || transfer.status != "transferring" || transfer.working || transfer.peerID != sender.identity.PeerID() || transfer.sessionID != sender.session.sessionID || offset != transfer.transferred || uint64(len(data)) > transfer.size-transfer.transferred {
		return
	}
	transfer.working = true
	copyData := append([]byte(nil), data...)
	file := transfer.file
	c.startFileWork(fileWorkResult{kind: fileWorkWrite, transferID: hex.EncodeToString(id[:]), data: copyData}, func(result *fileWorkResult) {
		_, result.err = file.WriteAt(copyData, int64(offset))
	})
}

type decodedFileOffer struct {
	name   string
	size   uint64
	digest [sha256.Size]byte
}

func (c *Client) receiveFileOffer(sender *RemotePeer, id [16]byte, offer decodedFileOffer) {
	if _, exists := c.fileTransfers[id]; exists || c.activeFileTransferCount() >= maxFileTransferRecords {
		return
	}
	workContext, cancelWork := context.WithCancel(c.fileContext)
	transfer := &fileTransfer{id: id, peerID: sender.identity.PeerID(), sessionID: sender.session.sessionID, direction: "incoming", name: offer.name, status: "offered", size: offer.size, digest: offer.digest, updatedAt: time.Now(), workContext: workContext, cancelWork: cancelWork}
	c.fileTransfers[id] = transfer
	c.pruneFileTransfers()
	c.publishStateChange()
}

func (c *Client) readNextFileChunk(transfer *fileTransfer) {
	if transfer.working || transfer.transferred >= transfer.size {
		return
	}
	transfer.working = true
	offset := transfer.transferred
	size := int(min(uint64(fileChunkSize), transfer.size-offset))
	path := transfer.path
	id := hex.EncodeToString(transfer.id[:])
	c.startFileWork(fileWorkResult{kind: fileWorkRead, transferID: id}, func(result *fileWorkResult) {
		file, err := os.Open(path)
		if err != nil {
			result.err = err
			return
		}
		defer file.Close()
		result.data = make([]byte, size)
		_, result.err = io.ReadFull(io.NewSectionReader(file, int64(offset), int64(size)), result.data)
	})
}

func (c *Client) queueFileControl(transfer *fileTransfer, kind byte, value uint64) error {
	payload := make([]byte, 18)
	payload[0], payload[1] = fileProtocolVersion, kind
	copy(payload[2:18], transfer.id[:])
	if kind == fileControlACK {
		payload = append(payload, make([]byte, 8)...)
		binary.BigEndian.PutUint64(payload[18:], value)
	}
	return c.queueFilePayload(transfer, reliableChannelFileControl, true, payload)
}

func (c *Client) queueFilePayload(transfer *fileTransfer, channel uint16, ordered bool, payload []byte) error {
	peer := c.remotePeers[transfer.peerID]
	if peer == nil || peer.session == nil || !peer.session.authenticated || peer.session.sessionID != transfer.sessionID {
		return errors.New("file transfer session is unavailable")
	}
	return peer.session.reliable.queue(channel, ordered, payload)
}

func encodeFileOffer(transfer *fileTransfer) ([]byte, error) {
	if transfer == nil || !validFileName(transfer.name) || transfer.size > MaxFileTransferBytes {
		return nil, errors.New("file offer is invalid")
	}
	payload := make([]byte, 60+len(transfer.name))
	payload[0], payload[1] = fileProtocolVersion, fileControlOffer
	copy(payload[2:18], transfer.id[:])
	binary.BigEndian.PutUint64(payload[18:26], transfer.size)
	copy(payload[26:58], transfer.digest[:])
	binary.BigEndian.PutUint16(payload[58:60], uint16(len(transfer.name)))
	copy(payload[60:], transfer.name)
	return payload, nil
}

func decodeFileControl(payload []byte) (byte, [16]byte, uint64, decodedFileOffer, error) {
	var id [16]byte
	if len(payload) < 18 || payload[0] != fileProtocolVersion {
		return 0, id, 0, decodedFileOffer{}, errors.New("file control header is invalid")
	}
	kind := payload[1]
	copy(id[:], payload[2:18])
	if id == ([16]byte{}) {
		return 0, id, 0, decodedFileOffer{}, errors.New("file transfer ID is zero")
	}
	if kind == fileControlOffer {
		if len(payload) < 60 {
			return 0, id, 0, decodedFileOffer{}, errors.New("file offer is truncated")
		}
		nameLength := int(binary.BigEndian.Uint16(payload[58:60]))
		if nameLength == 0 || len(payload) != 60+nameLength {
			return 0, id, 0, decodedFileOffer{}, errors.New("file offer length is invalid")
		}
		offer := decodedFileOffer{name: string(payload[60:]), size: binary.BigEndian.Uint64(payload[18:26])}
		copy(offer.digest[:], payload[26:58])
		if offer.size > MaxFileTransferBytes || !validFileName(offer.name) {
			return 0, id, 0, decodedFileOffer{}, errors.New("file offer fields are invalid")
		}
		return kind, id, 0, offer, nil
	}
	if kind == fileControlACK {
		if len(payload) != 26 {
			return 0, id, 0, decodedFileOffer{}, errors.New("file ACK length is invalid")
		}
		return kind, id, binary.BigEndian.Uint64(payload[18:26]), decodedFileOffer{}, nil
	}
	if (kind != fileControlAccept && kind != fileControlReject && kind != fileControlCancel) || len(payload) != 18 {
		return 0, id, 0, decodedFileOffer{}, errors.New("file control encoding is invalid")
	}
	return kind, id, 0, decodedFileOffer{}, nil
}

func encodeFileData(id [16]byte, offset uint64, data []byte) []byte {
	payload := make([]byte, 29+len(data))
	payload[0] = fileProtocolVersion
	copy(payload[1:17], id[:])
	binary.BigEndian.PutUint64(payload[17:25], offset)
	binary.BigEndian.PutUint32(payload[25:29], uint32(len(data)))
	copy(payload[29:], data)
	return payload
}

func decodeFileData(payload []byte) ([16]byte, uint64, []byte, error) {
	var id [16]byte
	if len(payload) < 29 || payload[0] != fileProtocolVersion {
		return id, 0, nil, errors.New("file data header is invalid")
	}
	copy(id[:], payload[1:17])
	size := int(binary.BigEndian.Uint32(payload[25:29]))
	if id == ([16]byte{}) || size == 0 || size > fileChunkSize || len(payload) != 29+size {
		return id, 0, nil, errors.New("file data encoding is invalid")
	}
	return id, binary.BigEndian.Uint64(payload[17:25]), payload[29:], nil
}

func validFileName(name string) bool {
	return name != "" && len(name) <= maxFileNameBytes && utf8.ValidString(name) && name == filepath.Base(name) && name != "." && !strings.ContainsAny(name, "\x00/\\")
}

func newFileTransferID() ([16]byte, error) {
	var id [16]byte
	for id == ([16]byte{}) {
		if _, err := rand.Read(id[:]); err != nil {
			return id, fmt.Errorf("create file transfer ID: %w", err)
		}
	}
	return id, nil
}

func (c *Client) fileTransferByString(encoded string) *fileTransfer {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != 16 {
		return nil
	}
	var id [16]byte
	copy(id[:], decoded)
	return c.fileTransfers[id]
}

func (c *Client) activeFileTransfer(peerID, direction string) *fileTransfer {
	for _, transfer := range c.fileTransfers {
		if transfer.peerID == peerID && transfer.direction == direction && !fileTerminal(transfer.status) {
			return transfer
		}
	}
	return nil
}

func (c *Client) activeAcceptedFileTransfer(peerID string) *fileTransfer {
	for _, transfer := range c.fileTransfers {
		if transfer.peerID == peerID && transfer.direction == "incoming" && transfer.status != "offered" && !fileTerminal(transfer.status) {
			return transfer
		}
	}
	return nil
}

func (c *Client) activeFileTransferCount() int {
	count := 0
	for _, transfer := range c.fileTransfers {
		if !fileTerminal(transfer.status) {
			count++
		}
	}
	return count
}

func fileTerminal(status string) bool {
	return status == "completed" || status == "rejected" || status == "canceled" || status == "failed"
}

func (c *Client) finishFileTransfer(transfer *fileTransfer, status, reason string) {
	if transfer == nil || fileTerminal(transfer.status) {
		return
	}
	transfer.status, transfer.errorText, transfer.updatedAt, transfer.working = status, reason, time.Now(), false
	if transfer.cancelWork != nil {
		transfer.cancelWork()
		transfer.cancelWork = nil
	}
	if peer := c.remotePeers[transfer.peerID]; peer != nil && peer.session != nil && peer.session.sessionID == transfer.sessionID {
		if transfer.direction == "outgoing" {
			peer.session.reliable.discardOutboundChannel(reliableChannelFileData)
		} else {
			peer.session.reliable.discardInboundChannel(reliableChannelFileData)
		}
	}
	if transfer.file != nil {
		c.closePartialFile(transfer.file, transfer.path)
		transfer.file = nil
	}
	c.pruneFileTransfers()
	c.publishStateChange()
}

func (c *Client) closePartialFile(file *os.File, path string) {
	c.fileWorkers.Add(1)
	go func() {
		defer c.fileWorkers.Done()
		_ = file.Close()
		if path != "" {
			_ = os.Remove(path)
		}
	}()
}

func (c *Client) expireFileTransfers(now time.Time) {
	for _, transfer := range c.fileTransfers {
		if fileTerminal(transfer.status) {
			continue
		}
		peer := c.remotePeers[transfer.peerID]
		if peer == nil || peer.session == nil || !peer.session.authenticated || peer.session.sessionID != transfer.sessionID {
			c.finishFileTransfer(transfer, "failed", "peer session ended")
			continue
		}
		if transfer.working {
			continue
		}
		timeout := fileTransferTimeout
		if transfer.status == "offered" {
			timeout = fileOfferTimeout
		}
		if !now.Before(transfer.updatedAt.Add(timeout)) {
			c.queueFileControl(transfer, fileControlCancel, 0)
			c.finishFileTransfer(transfer, "failed", "file transfer timed out")
		}
	}
}

func (c *Client) discardFileWorkResults() {
	for {
		select {
		case result := <-c.fileWorkResults:
			if result.file == nil {
				continue
			}
			_ = result.file.Close()
			if transfer := c.fileTransferByString(result.transferID); transfer != nil && transfer.direction == "incoming" && transfer.path != "" {
				_ = os.Remove(transfer.path)
			}
		default:
			return
		}
	}
}

func (c *Client) stopFileTransfers() {
	for _, transfer := range c.fileTransfers {
		if !fileTerminal(transfer.status) {
			c.finishFileTransfer(transfer, "canceled", "peer client stopped")
		}
	}
}

func (c *Client) pruneFileTransfers() {
	for {
		terminalCount := 0
		var oldestID [16]byte
		var oldest time.Time
		for id, transfer := range c.fileTransfers {
			if !fileTerminal(transfer.status) {
				continue
			}
			terminalCount++
			if oldest.IsZero() || transfer.updatedAt.Before(oldest) {
				oldestID, oldest = id, transfer.updatedAt
			}
		}
		if terminalCount <= maxFileTransferRecords {
			return
		}
		delete(c.fileTransfers, oldestID)
	}
}

func (c *Client) fileTransferSnapshots() []FileTransferSnapshot {
	snapshots := make([]FileTransferSnapshot, 0, len(c.fileTransfers))
	for _, transfer := range c.fileTransfers {
		snapshots = append(snapshots, FileTransferSnapshot{
			ID: hex.EncodeToString(transfer.id[:]), PeerID: transfer.peerID, Direction: transfer.direction,
			Name: transfer.name, Path: transfer.path, Status: transfer.status, Size: transfer.size,
			Transferred: transfer.transferred, SHA256: hex.EncodeToString(transfer.digest[:]), Error: transfer.errorText,
		})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return snapshots
}

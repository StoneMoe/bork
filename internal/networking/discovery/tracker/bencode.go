package tracker

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"time"
)

const (
	maxHTTPTrackerResponseSize = 64 * 1024
	maxBencodeDepth            = 8
	maxBencodeStringLength     = maxHTTPTrackerResponseSize
	maxBencodeKeyLength        = 128
	maxFailureReasonLength     = 1024
	maxHTTPPeerNameLength      = 253
)

type bencodeDecoder struct {
	data []byte
	pos  int
}

func parseHTTPAnnounceResponse(packet []byte) (announceResponse, error) {
	if len(packet) == 0 {
		return announceResponse{}, errors.New("HTTP tracker response is empty")
	}
	if len(packet) > maxHTTPTrackerResponseSize {
		return announceResponse{}, fmt.Errorf("HTTP tracker response exceeds %d bytes", maxHTTPTrackerResponseSize)
	}

	decoder := bencodeDecoder{data: packet}
	if decoder.take() != 'd' {
		return announceResponse{}, errors.New("HTTP tracker response must be a bencoded dictionary")
	}

	var interval int64
	var intervalSet bool
	var compact4, compact6 []byte
	var listedPeers []netip.AddrPort
	var listedPeerNames []httpPeer
	var externalIP []byte
	var failure []byte
	var failureSet bool
	var previousKey []byte
	for {
		if decoder.pos >= len(decoder.data) {
			return announceResponse{}, errors.New("bencoded tracker dictionary is truncated")
		}
		if decoder.data[decoder.pos] == 'e' {
			decoder.pos++
			break
		}
		key, err := decoder.parseString(1)
		if err != nil {
			return announceResponse{}, err
		}
		if len(key) == 0 || len(key) > maxBencodeKeyLength {
			return announceResponse{}, errors.New("bencoded tracker dictionary key is invalid")
		}
		if previousKey != nil && bytes.Compare(previousKey, key) >= 0 {
			return announceResponse{}, errors.New("bencoded tracker dictionary keys are not strictly ordered")
		}
		previousKey = key

		switch string(key) {
		case "external ip":
			externalIP, err = decoder.parseString(1)
		case "failure reason":
			failure, err = decoder.parseString(1)
			if err == nil && len(failure) > maxFailureReasonLength {
				err = errors.New("HTTP tracker failure reason is too long")
			}
			failureSet = true
		case "interval":
			interval, err = decoder.parseInteger(1)
			intervalSet = true
		case "peers":
			compact4, listedPeers, listedPeerNames, err = decoder.parseHTTPPeers(1)
		case "peers6":
			compact6, err = decoder.parseString(1)
		default:
			err = decoder.skipValue(1)
		}
		if err != nil {
			return announceResponse{}, err
		}
	}
	if decoder.pos != len(decoder.data) {
		return announceResponse{}, errors.New("HTTP tracker response has trailing bencode data")
	}
	if len(compact4)%6 != 0 || len(compact6)%18 != 0 {
		return announceResponse{}, errors.New("HTTP tracker response has a malformed compact peer list")
	}
	if failureSet {
		return announceResponse{}, &TrackerError{Message: string(failure)}
	}
	if !intervalSet || interval <= 0 || interval > math.MaxUint32 {
		return announceResponse{}, errors.New("HTTP tracker interval is missing or invalid")
	}

	response := announceResponse{
		interval:  time.Duration(interval) * time.Second,
		peers:     listedPeers,
		peerNames: listedPeerNames,
	}
	if len(externalIP) > 0 {
		response.externalAddress = parseTrackerExternalIP(externalIP)
	}
	return parseHTTPCompactPeers(response, compact4, compact6)
}

func (d *bencodeDecoder) parseHTTPPeers(depth int) ([]byte, []netip.AddrPort, []httpPeer, error) {
	if d.pos >= len(d.data) {
		return nil, nil, nil, errors.New("bencoded tracker peers value is truncated")
	}
	if d.data[d.pos] == 'l' {
		peers, names, err := d.parseHTTPPeerList(depth)
		return nil, peers, names, err
	}
	compact, err := d.parseString(depth)
	return compact, nil, nil, err
}

func (d *bencodeDecoder) parseHTTPPeerList(depth int) ([]netip.AddrPort, []httpPeer, error) {
	if err := d.beginCollection(depth, 'l', "peer list"); err != nil {
		return nil, nil, err
	}
	peers := make([]netip.AddrPort, 0, maxAnnouncePeers)
	names := make([]httpPeer, 0, maxAnnouncePeers)
	seen := make(map[netip.AddrPort]struct{}, maxAnnouncePeers)
	for {
		ended, err := d.endCollection("peer list")
		if err != nil {
			return nil, nil, err
		}
		if ended {
			return peers, names, nil
		}
		peer, err := d.parseHTTPPeer(depth + 1)
		if err != nil {
			return nil, nil, err
		}
		peers, names = appendHTTPPeer(peers, names, seen, peer)
	}
}

func appendHTTPPeer(peers []netip.AddrPort, names []httpPeer, seen map[netip.AddrPort]struct{}, peer httpPeer) ([]netip.AddrPort, []httpPeer) {
	if peer.address.IsValid() && peer.port != 0 {
		return appendUniquePeer(peers, seen, netip.AddrPortFrom(peer.address, peer.port)), names
	}
	if peer.name != "" && peer.port != 0 && len(peers)+len(names) < maxAnnouncePeers {
		names = append(names, peer)
	}
	return peers, names
}

func (d *bencodeDecoder) parseHTTPPeer(depth int) (httpPeer, error) {
	if err := d.beginCollection(depth, 'd', "peer"); err != nil {
		return httpPeer{}, err
	}
	var fields httpPeer
	var previousKey []byte
	for {
		ended, err := d.endCollection("peer dictionary")
		if err != nil {
			return httpPeer{}, err
		}
		if ended {
			return fields, nil
		}
		key, err := d.parseHTTPPeerEntry(&fields, depth+1, previousKey)
		if err != nil {
			return httpPeer{}, err
		}
		previousKey = key
	}
}

func (d *bencodeDecoder) parseHTTPPeerEntry(fields *httpPeer, depth int, previousKey []byte) ([]byte, error) {
	key, err := d.parseString(depth)
	if err != nil {
		return nil, err
	}
	if len(key) == 0 || len(key) > maxBencodeKeyLength {
		return nil, errors.New("bencoded peer dictionary key is invalid")
	}
	if previousKey != nil && bytes.Compare(previousKey, key) >= 0 {
		return nil, errors.New("bencoded peer dictionary keys are not strictly ordered")
	}
	return key, d.parseHTTPPeerField(fields, key, depth)
}

func (d *bencodeDecoder) parseHTTPPeerField(fields *httpPeer, key []byte, depth int) error {
	switch string(key) {
	case "ip":
		return fields.parseAddress(d, depth)
	case "peer id":
		_, err := d.parseString(depth)
		return err
	case "port":
		return fields.parsePort(d, depth)
	default:
		return d.skipValue(depth)
	}
}

func (fields *httpPeer) parseAddress(d *bencodeDecoder, depth int) error {
	encoded, err := d.parseString(depth)
	if err != nil {
		return err
	}
	address, err := netip.ParseAddr(string(encoded))
	if err == nil && address.Zone() == "" {
		fields.address = address.Unmap()
	} else if len(encoded) > 0 && len(encoded) <= maxHTTPPeerNameLength {
		fields.name = string(encoded)
	}
	return nil
}

func (fields *httpPeer) parsePort(d *bencodeDecoder, depth int) error {
	port, err := d.parseInteger(depth)
	if err != nil {
		return err
	}
	if port > 0 && port <= math.MaxUint16 {
		fields.port = uint16(port)
	}
	return nil
}

func (d *bencodeDecoder) beginCollection(depth int, marker byte, name string) error {
	if err := d.enterValue(depth); err != nil {
		return err
	}
	if d.take() != marker {
		return fmt.Errorf("bencoded tracker %s has the wrong value type", name)
	}
	return nil
}

func (d *bencodeDecoder) endCollection(name string) (bool, error) {
	if d.pos >= len(d.data) {
		return false, fmt.Errorf("bencoded tracker %s is truncated", name)
	}
	if d.data[d.pos] != 'e' {
		return false, nil
	}
	d.pos++
	return true, nil
}

func parseTrackerExternalIP(encoded []byte) netip.Addr {
	switch len(encoded) {
	case 4:
		return netip.AddrFrom4([4]byte(encoded))
	case 16:
		return netip.AddrFrom16([16]byte(encoded)).Unmap()
	default:
		address, err := netip.ParseAddr(string(encoded))
		if err == nil {
			return address.Unmap()
		}
		return netip.Addr{}
	}
}

func parseHTTPCompactPeers(response announceResponse, compact4, compact6 []byte) (announceResponse, error) {
	seen := make(map[netip.AddrPort]struct{}, maxAnnouncePeers)
	for _, peer := range response.peers {
		seen[peer] = struct{}{}
	}
	peers, err := appendCompactPeers(response.peers, seen, compact4, false)
	if err != nil {
		return announceResponse{}, fmt.Errorf("HTTP tracker IPv4 peers: %w", err)
	}
	peers, err = appendCompactPeers(peers, seen, compact6, true)
	if err != nil {
		return announceResponse{}, fmt.Errorf("HTTP tracker IPv6 peers: %w", err)
	}
	response.peers = peers
	return response, nil
}

func (d *bencodeDecoder) enterValue(depth int) error {
	if depth > maxBencodeDepth {
		return errors.New("bencoded tracker response is nested too deeply")
	}
	return nil
}

func (d *bencodeDecoder) parseString(depth int) ([]byte, error) {
	if err := d.enterValue(depth); err != nil {
		return nil, err
	}
	return d.parseRawString()
}

func (d *bencodeDecoder) parseRawString() ([]byte, error) {
	if d.pos >= len(d.data) || d.data[d.pos] < '0' || d.data[d.pos] > '9' {
		return nil, errors.New("invalid bencoded byte string")
	}
	separator := bytes.IndexByte(d.data[d.pos:], ':')
	if separator < 0 {
		return nil, errors.New("truncated bencoded byte string length")
	}
	encoded := d.data[d.pos : d.pos+separator]
	if len(encoded) > 1 && encoded[0] == '0' {
		return nil, errors.New("non-canonical bencoded byte string length")
	}
	length, err := strconv.ParseUint(string(encoded), 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, errors.New("bencoded byte string is too long")
		}
		return nil, errors.New("invalid bencoded byte string length")
	}
	if length > maxBencodeStringLength {
		return nil, errors.New("bencoded byte string is too long")
	}
	d.pos += separator + 1
	if length > uint64(len(d.data)-d.pos) {
		return nil, errors.New("truncated bencoded byte string")
	}
	value := d.data[d.pos : d.pos+int(length)]
	d.pos += int(length)
	return value, nil
}

func (d *bencodeDecoder) parseInteger(depth int) (int64, error) {
	if err := d.enterValue(depth); err != nil {
		return 0, err
	}
	return d.parseRawInteger()
}

func (d *bencodeDecoder) parseRawInteger() (int64, error) {
	if d.take() != 'i' {
		return 0, errors.New("invalid bencoded integer")
	}
	separator := bytes.IndexByte(d.data[d.pos:], 'e')
	if separator < 0 {
		return 0, errors.New("truncated bencoded integer")
	}
	encoded := d.data[d.pos : d.pos+separator]
	digits := encoded
	if len(digits) > 0 && digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return 0, errors.New("truncated bencoded integer")
	}
	if encoded[0] == '+' {
		return 0, errors.New("invalid bencoded integer digit")
	}
	if digits[0] == '0' && len(digits) > 1 {
		return 0, errors.New("non-canonical bencoded integer")
	}
	if encoded[0] == '-' && digits[0] == '0' {
		return 0, errors.New("non-canonical negative zero")
	}
	if len(encoded) > 20 {
		return 0, errors.New("bencoded integer is too long")
	}
	value, err := strconv.ParseInt(string(encoded), 10, 64)
	if err != nil {
		if !errors.Is(err, strconv.ErrRange) {
			return 0, errors.New("invalid bencoded integer digit")
		}
		return 0, errors.New("bencoded integer is out of range")
	}
	d.pos += separator + 1
	return value, nil
}

func (d *bencodeDecoder) skipValue(depth int) error {
	if err := d.enterValue(depth); err != nil {
		return err
	}
	if d.pos >= len(d.data) {
		return errors.New("truncated bencoded value")
	}
	switch d.data[d.pos] {
	case 'i':
		_, err := d.parseRawInteger()
		return err
	case 'l':
		d.pos++
		for {
			if d.pos >= len(d.data) {
				return errors.New("truncated bencoded list")
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				return nil
			}
			if err := d.skipValue(depth + 1); err != nil {
				return err
			}
		}
	case 'd':
		d.pos++
		var previousKey []byte
		for {
			if d.pos >= len(d.data) {
				return errors.New("truncated bencoded dictionary")
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				return nil
			}
			key, err := d.parseString(depth + 1)
			if err != nil {
				return err
			}
			if len(key) == 0 || len(key) > maxBencodeKeyLength || (previousKey != nil && bytes.Compare(previousKey, key) >= 0) {
				return errors.New("invalid bencoded dictionary key ordering")
			}
			previousKey = key
			if err := d.skipValue(depth + 1); err != nil {
				return err
			}
		}
	default:
		if d.data[d.pos] < '0' || d.data[d.pos] > '9' {
			return errors.New("invalid bencoded value type")
		}
		_, err := d.parseRawString()
		return err
	}
}

func (d *bencodeDecoder) take() byte {
	if d.pos >= len(d.data) {
		return 0
	}
	value := d.data[d.pos]
	d.pos++
	return value
}

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
	maxBencodeItems            = 1024
	maxBencodeStringLength     = maxHTTPTrackerResponseSize
	maxBencodeKeyLength        = 128
	maxFailureReasonLength     = 1024
)

type bencodeDecoder struct {
	data  []byte
	pos   int
	items int
}

func parseHTTPAnnounceResponse(packet []byte) (announceResponse, error) {
	if len(packet) == 0 {
		return announceResponse{}, errors.New("HTTP tracker response is empty")
	}
	if len(packet) > maxHTTPTrackerResponseSize {
		return announceResponse{}, fmt.Errorf("HTTP tracker response exceeds %d bytes", maxHTTPTrackerResponseSize)
	}

	decoder := bencodeDecoder{data: packet}
	if err := decoder.enterValue(0); err != nil {
		return announceResponse{}, err
	}
	if decoder.take() != 'd' {
		return announceResponse{}, errors.New("HTTP tracker response must be a bencoded dictionary")
	}

	var interval int64
	var intervalSet bool
	var compact4, compact6 []byte
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
			compact4, err = decoder.parseString(1)
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
	if !intervalSet || interval < 0 || interval > math.MaxUint32 {
		return announceResponse{}, errors.New("HTTP tracker interval is missing or invalid")
	}

	response := announceResponse{interval: time.Duration(interval) * time.Second}
	if len(externalIP) > 0 {
		response.externalAddress = parseTrackerExternalIP(externalIP)
	}
	return parseHTTPCompactPeers(response, compact4, compact6)
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
	d.items++
	if d.items > maxBencodeItems {
		return errors.New("bencoded tracker response has too many items")
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

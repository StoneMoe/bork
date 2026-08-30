package iwan

import (
	"crypto/aes"
	"crypto/md5"
	"crypto/subtle"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxUsernameBytes = 253
	maxPasswordBytes = aes.BlockSize
)

type Credentials struct {
	username      string
	passwordBlock [aes.BlockSize]byte
	xorKey        [8]byte
	valid         bool
}

func NewCredentials(username, password string) (Credentials, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > maxUsernameBytes || !utf8.ValidString(username) {
		return Credentials{}, fmt.Errorf("username: %w", ErrMalformedPacket)
	}
	if password == "" || len(password) > maxPasswordBytes || !utf8.ValidString(password) {
		return Credentials{}, fmt.Errorf("password: %w", ErrMalformedPacket)
	}
	digest := md5.Sum([]byte(username + password))
	var key [8]byte
	copy(key[:], digest[:8])
	passwordBlock, err := encryptPassword(username, password)
	if err != nil {
		return Credentials{}, fmt.Errorf("password transform: %w", err)
	}
	return Credentials{username: username, passwordBlock: passwordBlock, xorKey: key, valid: true}, nil
}

type OpenRequest struct {
	Username      string
	PasswordBlock [aes.BlockSize]byte
	MTU           uint16
	XOR           bool
}

func BuildOpen(credentials Credentials, mtu uint16) ([]byte, error) {
	if !credentials.valid {
		return nil, fmt.Errorf("credentials: %w", ErrMalformedPacket)
	}
	if mtu == 0 {
		mtu = DefaultMTU
	}
	if mtu < MinMTU || mtu > MaxMTU {
		return nil, fmt.Errorf("MTU %d: %w", mtu, ErrMalformedPacket)
	}
	packet := make([]byte, signedHeaderSize, 64)
	writeHeader(packet, wireHeader{typ: TypeOpen, flags: 1})
	signControl(packet)
	packet = appendTLV(packet, 3, byte(mtu>>8), byte(mtu))
	packet = appendTLV(packet, 1, []byte(credentials.username)...)
	packet = appendTLV(packet, 2, credentials.passwordBlock[:]...)
	return appendTLV(packet, 8, 1), nil
}

func ParseOpen(packet []byte) (OpenRequest, error) {
	header, err := parseHeader(packet)
	if err != nil {
		return OpenRequest{}, err
	}
	if header.typ != TypeOpen || header.flags != 1 || !allZero(header.token[:]) || !allZero(header.session[:]) {
		return OpenRequest{}, ErrMalformedPacket
	}
	if err := validateSignedControl(packet); err != nil {
		return OpenRequest{}, err
	}
	items, err := parseTLVs(packet[signedHeaderSize:])
	if err != nil {
		return OpenRequest{}, err
	}
	request := OpenRequest{}
	var usernameSeen, passwordSeen, mtuSeen, xorSeen bool
	for _, item := range items {
		switch item.typ {
		case 1:
			if usernameSeen || len(item.value) == 0 || len(item.value) > maxUsernameBytes || !utf8.Valid(item.value) {
				return OpenRequest{}, ErrMalformedPacket
			}
			request.Username = string(item.value)
			usernameSeen = true
		case 2:
			if passwordSeen || len(item.value) != aes.BlockSize {
				return OpenRequest{}, ErrMalformedPacket
			}
			copy(request.PasswordBlock[:], item.value)
			passwordSeen = true
		case 3:
			if mtuSeen || len(item.value) != 2 {
				return OpenRequest{}, ErrMalformedPacket
			}
			request.MTU = uint16(item.value[0])<<8 | uint16(item.value[1])
			mtuSeen = true
		case 8:
			if xorSeen || len(item.value) != 1 || item.value[0] != 1 {
				return OpenRequest{}, ErrMalformedPacket
			}
			request.XOR = true
			xorSeen = true
		default:
			if isCriticalTLV(item.typ) {
				return OpenRequest{}, ErrMalformedPacket
			}
		}
	}
	if !usernameSeen || !passwordSeen || !mtuSeen || !xorSeen || strings.TrimSpace(request.Username) != request.Username || request.MTU < MinMTU || request.MTU > MaxMTU {
		return OpenRequest{}, ErrMalformedPacket
	}
	return request, nil
}

func (request OpenRequest) Authenticate(credentials Credentials) bool {
	if !credentials.valid || !request.XOR || request.Username != credentials.username {
		return false
	}
	return subtle.ConstantTimeCompare(request.PasswordBlock[:], credentials.passwordBlock[:]) == 1
}

func encryptPassword(username, password string) ([aes.BlockSize]byte, error) {
	key := md5.Sum([]byte("mw" + username))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return [aes.BlockSize]byte{}, err
	}
	var plaintext [aes.BlockSize]byte
	copy(plaintext[:], password)
	var encrypted [aes.BlockSize]byte
	block.Encrypt(encrypted[:], plaintext[:])
	return encrypted, nil
}

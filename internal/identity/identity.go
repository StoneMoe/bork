package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	identityFilename    = "identity.key"
	identityMagic       = "BORKID01"
	maxIdentityFileSize = 4096
	loadRetryAttempts   = 50
	loadRetryDelay      = 10 * time.Millisecond
)

type Identity struct {
	publicKey ed25519.PublicKey
	peerID    string
}

type LocalIdentity struct {
	Identity
	dataDir    string
	privateKey ed25519.PrivateKey
}

func LoadOrCreate(dataDir string) (*LocalIdentity, error) {
	resolved, err := resolveDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(resolved, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(resolved, 0o700); err != nil {
			return nil, fmt.Errorf("secure data directory: %w", err)
		}
	}

	path := filepath.Join(resolved, identityFilename)
	identity, err := load(path, resolved)
	if err == nil {
		return identity, nil
	}
	if isRetryableAccessError(err) {
		return loadWithRetry(path, resolved)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate user identity: %w", err)
	}
	if err := store(path, resolved, seed); err != nil {
		if isPublishConflict(err) {
			return loadWithRetry(path, resolved)
		}
		return nil, err
	}
	return fromSeed(resolved, seed), nil
}

func store(path, dataDir string, seed []byte) error {
	return writeIdentity(path, dataDir, seed, publishFile)
}

func writeIdentity(path, dataDir string, seed []byte, publish func(string, string) error) error {
	protected, err := protectSeed(seed)
	if err != nil {
		return fmt.Errorf("protect user identity: %w", err)
	}
	contents := append([]byte(identityMagic), protected...)
	file, err := os.CreateTemp(dataDir, ".identity-key-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary user identity: %w", err)
	}
	temporaryPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary user identity: %w", err)
	}
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write user identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync user identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close user identity: %w", err)
	}
	if err := publish(temporaryPath, path); err != nil {
		return fmt.Errorf("publish user identity: %w", err)
	}
	return nil
}

func (i Identity) PeerID() string {
	return i.peerID
}

func (i Identity) ShortID() string {
	if len(i.peerID) <= 14 {
		return i.peerID
	}
	return i.peerID[:14]
}

func (i Identity) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), i.publicKey...)
}

func (i Identity) Verify(message, signature []byte) bool {
	return ed25519.Verify(i.publicKey, message, signature)
}

func (i *LocalIdentity) Public() crypto.PublicKey {
	return i.PublicKey()
}

func (i *LocalIdentity) Sign(_ io.Reader, message []byte, options crypto.SignerOpts) ([]byte, error) {
	if options.HashFunc() != crypto.Hash(0) {
		return nil, errors.New("Ed25519 messages must not be pre-hashed")
	}
	return ed25519.Sign(i.privateKey, message), nil
}

func (i *LocalIdentity) DataDir() string {
	return i.dataDir
}

func FromPublicKey(publicKey ed25519.PublicKey) (Identity, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return Identity{}, errors.New("invalid Ed25519 public key")
	}
	publicKey = append(ed25519.PublicKey(nil), publicKey...)
	digest := sha256.Sum256(publicKey)
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
	return Identity{publicKey: publicKey, peerID: "b1" + encoded}, nil
}

func resolveDataDir(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate user config directory: %w", err)
		}
		dataDir = filepath.Join(base, "bork")
	}
	resolved, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	return resolved, nil
}

func load(path, dataDir string) (*LocalIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect user identity: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("user identity is not a regular file")
	}
	if info.Size() > maxIdentityFileSize {
		return nil, errors.New("user identity is too large")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := file.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("secure user identity: %w", err)
		}
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxIdentityFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read user identity: %w", err)
	}
	if len(contents) > maxIdentityFileSize {
		return nil, errors.New("user identity is too large")
	}
	seed, err := decodeStoredSeed(contents)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("user identity is corrupt or unavailable to this account")
	}
	return fromSeed(dataDir, seed), nil
}

func loadWithRetry(path, dataDir string) (*LocalIdentity, error) {
	var lastErr error
	for range loadRetryAttempts {
		identity, err := load(path, dataDir)
		if err == nil {
			return identity, nil
		}
		lastErr = err
		time.Sleep(loadRetryDelay)
	}
	return nil, lastErr
}

func fromSeed(dataDir string, seed []byte) *LocalIdentity {
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := append(ed25519.PublicKey(nil), privateKey.Public().(ed25519.PublicKey)...)
	publicIdentity, err := FromPublicKey(publicKey)
	if err != nil {
		panic(err)
	}
	return &LocalIdentity{
		Identity:   publicIdentity,
		dataDir:    dataDir,
		privateKey: privateKey,
	}
}

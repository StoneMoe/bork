package identity

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreatePersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate() second error = %v", err)
	}
	if first.PeerID() != second.PeerID() {
		t.Fatalf("PeerID changed: %q != %q", first.PeerID(), second.PeerID())
	}
	message := []byte("bork identity test")
	signature, err := first.Sign(rand.Reader, message, crypto.Hash(0))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if !ed25519.Verify(first.PublicKey(), message, signature) {
		t.Fatal("signature did not verify")
	}

	if runtime.GOOS != "windows" {
		path := filepath.Join(dir, identityFilename)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("identity permissions = %o, want 600", info.Mode().Perm())
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if len(contents) < len(identityMagic) || string(contents[:len(identityMagic)]) != "BORKID01" {
			t.Fatalf("identity magic = %q, want BORKID01", contents[:min(len(contents), len(identityMagic))])
		}
		seed, err := decodeStoredSeed(contents)
		if err != nil {
			t.Fatalf("decodeStoredSeed() error = %v", err)
		}
		if !bytes.Equal(seed, first.privateKey.Seed()) {
			t.Fatal("stored identity seed does not match loaded identity")
		}
	}
}

func TestLoadOrCreateConcurrent(t *testing.T) {
	dir := t.TempDir()
	const workers = 8
	identities := make(chan *LocalIdentity, workers)
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			identity, err := LoadOrCreate(dir)
			if err != nil {
				errors <- err
				return
			}
			identities <- identity
		}()
	}
	wait.Wait()
	close(errors)
	close(identities)
	for err := range errors {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	var peerID string
	for identity := range identities {
		if peerID == "" {
			peerID = identity.PeerID()
		}
		if identity.PeerID() != peerID {
			t.Fatalf("concurrent identity mismatch: %q != %q", identity.PeerID(), peerID)
		}
	}
}

func TestLoadOrCreateRejectsIdentitySymlinkWithoutChmodTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.key")
	if err := os.WriteFile(target, []byte("not an identity"), 0o666); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0o666); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, identityFilename)); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	if _, err := LoadOrCreate(dir); err == nil || err.Error() != "user identity is not a regular file" {
		t.Fatalf("LoadOrCreate() error = %v, want non-regular identity error", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat() after load error = %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("symlink target permissions = %o, want unchanged %o", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestLoadOrCreateRejectsDataDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	dataDir := filepath.Join(root, "data")
	if err := os.Symlink(target, dataDir); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	if _, err := LoadOrCreate(dataDir); err == nil || err.Error() != "data directory must not be a symbolic link" {
		t.Fatalf("LoadOrCreate() error = %v, want data-directory symlink error", err)
	}
	if _, err := os.Stat(filepath.Join(target, identityFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity in symlink target Stat() error = %v, want not exist", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat() after load error = %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("data-directory symlink target permissions = %o, want unchanged %o", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestLoadRejectsReplacementBetweenInspectionAndOpen(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	path := filepath.Join(dir, identityFilename)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	replacedPath := filepath.Join(dir, "original.key")

	_, err = loadFile(path, func(path string) (*os.File, error) {
		if err := os.Rename(path, replacedPath); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, contents, 0o666); err != nil {
			return nil, err
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(path, 0o666); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	})
	if !errors.Is(err, errIdentityChanged) {
		t.Fatalf("loadFile() error = %v, want identity-changed error", err)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat() error = %v", statErr)
		}
		if info.Mode().Perm()&0o077 == 0 {
			t.Fatalf("replacement permissions = %o, want unchanged permissive mode", info.Mode().Perm())
		}
	}
}

func TestStoredIdentityDoesNotExposeSeedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI is Windows-specific")
	}
	dir := t.TempDir()
	identity, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, identityFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	privateKey := identity.privateKey
	seed := privateKey.Seed()
	if len(contents) == len(identityMagic)+len(seed) && string(contents[len(identityMagic):]) == string(seed) {
		t.Fatal("stored identity contains the plaintext Ed25519 seed")
	}
}

func TestLoadOrCreateRejectsCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identityFilename), []byte("broken"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate() error = nil, want corrupt identity error")
	}
}

func TestLoadOrCreateRejectsOversizedIdentity(t *testing.T) {
	dir := t.TempDir()
	contents := make([]byte, maxIdentityFileSize+1)
	if err := os.WriteFile(filepath.Join(dir, identityFilename), contents, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate() error = nil, want oversized identity error")
	}
}

func TestLoadOrCreateRejectsSameLengthSeedCorruptionOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows identity format test")
	}
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	path := filepath.Join(dir, identityFilename)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(contents) != len(identityMagic)+ed25519.SeedSize+ed25519.PublicKeySize {
		t.Fatalf("identity length = %d, want %d", len(contents), len(identityMagic)+ed25519.SeedSize+ed25519.PublicKeySize)
	}
	contents[len(identityMagic)] ^= 1
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate() error = nil, want seed corruption error")
	}
}

//go:build windows && wintun_embed

package peer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEmbeddedWintunAssets(t *testing.T) {
	if err := validateEmbeddedWintun(embeddedWintunDLL, embeddedWintunDLLSize, embeddedWintunDLLSHA256, embeddedWintunLicense); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedWintunMaterialization(t *testing.T) {
	descriptor := currentUserSecurityDescriptor(t)
	directory := t.TempDir()
	dllPath, err := materializeWintun(context.Background(), directory, descriptor, embeddedWintunDLL, embeddedWintunDLLSHA256, embeddedWintunLicense)
	if err != nil {
		t.Fatal(err)
	}
	licensePath := filepath.Join(directory, "LICENSE.txt")
	assertFileBytes(t, dllPath, embeddedWintunDLL)
	assertFileBytes(t, licensePath, embeddedWintunLicense)

	if err := os.WriteFile(licensePath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockedDLL, _, err := openLockedRuntimeFile(dllPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializeWintun(context.Background(), directory, descriptor, embeddedWintunDLL, embeddedWintunDLLSHA256, embeddedWintunLicense); err != nil {
		lockedDLL.Close()
		t.Fatal(err)
	}
	if err := lockedDLL.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, licensePath, embeddedWintunLicense)

	if err := os.WriteFile(dllPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeWintun(context.Background(), directory, descriptor, embeddedWintunDLL, embeddedWintunDLLSHA256, embeddedWintunLicense); err != nil {
		t.Fatal(err)
	}
	assertFileBytes(t, dllPath, embeddedWintunDLL)
}

func TestEmbeddedWintunPreparationRejectsNonElevatedProcess(t *testing.T) {
	elevated, err := processIsElevated()
	if err != nil {
		t.Fatal(err)
	}
	if elevated {
		t.Skip("process is elevated")
	}
	if err := prepareWintun(context.Background()); err == nil || !strings.Contains(err.Error(), "Administrator") {
		t.Fatalf("prepareWintun() error = %v", err)
	}
}

func currentUserSecurityDescriptor(t *testing.T) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sG:%sD:P(A;;FA;;;%s)", sid, sid, sid))
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func assertFileBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("%s content mismatch", filepath.Base(path))
	}
}

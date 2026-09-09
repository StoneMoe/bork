package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesAndAppendsLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "bork.log")
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "first\nsecond\n" {
		t.Fatalf("log contents = %q", contents)
	}
}

func TestOpenRotatesOversizedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bork.log")
	contents := make([]byte, maxLogSize)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != maxLogSize {
		t.Fatalf("rotated log: info=%v err=%v", info, err)
	}
}

package gameproxy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestScanExecutableRulesDiscoversRegularExecutablesRecursively(t *testing.T) {
	root := t.TempDir()
	first := writeTestFile(t, filepath.Join(root, "alpha.exe"))
	second := writeTestFile(t, filepath.Join(root, "nested", "Bravo.ExE"))
	writeTestFile(t, filepath.Join(root, "nested", "ignored.txt"))
	if err := os.Mkdir(filepath.Join(root, "directory.exe"), 0o700); err != nil {
		t.Fatal(err)
	}

	rules, err := scanExecutableRules(root, canonicalizeTestPath)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{canonicalizeTestPathRequired(t, first), canonicalizeTestPathRequired(t, second)}
	sort.Strings(want)
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %q, want %q", got, want)
	}
}

func TestScanExecutableRulesReturnsStableDeduplicatedPaths(t *testing.T) {
	root := t.TempDir()
	first := writeTestFile(t, filepath.Join(root, "z", "first.exe"))
	second := writeTestFile(t, filepath.Join(root, "a", "second.exe"))
	third := writeTestFile(t, filepath.Join(root, "m", "third.exe"))
	canonicalRoot := canonicalizeTestPathRequired(t, root)
	shared := filepath.Join(canonicalRoot, "canonical", "shared.exe")
	unique := filepath.Join(canonicalRoot, "canonical", "unique.exe")
	canonicalize := func(path string) (string, error) {
		switch path {
		case first, second:
			return shared, nil
		case third:
			return unique, nil
		default:
			return canonicalizeTestPath(path)
		}
	}

	rules, err := scanExecutableRules(root, canonicalize)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{shared, unique}
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %q, want %q", got, want)
	}
}

func TestScanExecutableRulesRejectsCanonicalPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	executable := writeTestFile(t, filepath.Join(root, "game.exe"))
	outside := filepath.Join(filepath.Dir(root), "outside.exe")
	canonicalize := func(path string) (string, error) {
		if path == executable {
			return outside, nil
		}
		return canonicalizeTestPath(path)
	}

	_, err := scanExecutableRules(root, canonicalize)

	if !errors.Is(err, ErrExecutableOutsideRoot) {
		t.Fatalf("error = %v, want ErrExecutableOutsideRoot", err)
	}
}

func TestScanExecutableRulesDoesNotTraverseDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	inside := writeTestFile(t, filepath.Join(root, "inside.exe"))
	externalRoot := t.TempDir()
	writeTestFile(t, filepath.Join(externalRoot, "outside.exe"))
	if err := os.Symlink(externalRoot, filepath.Join(root, "linked")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	rules, err := scanExecutableRules(root, canonicalizeTestPath)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{canonicalizeTestPathRequired(t, inside)}
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %q, want %q", got, want)
	}
}

func TestScanExecutableRulesReturnsWalkErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	canonicalize := func(path string) (string, error) {
		return filepath.Abs(path)
	}

	_, err := scanExecutableRules(root, canonicalize)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestScanExecutableRulesReturnsCanonicalizationPermissionErrors(t *testing.T) {
	root := t.TempDir()
	executable := writeTestFile(t, filepath.Join(root, "game.exe"))
	canonicalize := func(path string) (string, error) {
		if path == executable {
			return "", fs.ErrPermission
		}
		return canonicalizeTestPath(path)
	}

	_, err := scanExecutableRules(root, canonicalize)

	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("error = %v, want fs.ErrPermission", err)
	}
}

func TestExecutableRulesMatchUsesCanonicalFullPathOnly(t *testing.T) {
	workspace := t.TempDir()
	allowed := writeTestFile(t, filepath.Join(workspace, "allowed", "game.exe"))
	other := writeTestFile(t, filepath.Join(workspace, "other", "game.exe"))

	rules, err := ScanExecutableRules(filepath.Dir(allowed))
	if err != nil {
		t.Fatal(err)
	}
	matchedAllowed, err := rules.Match(allowed)
	if err != nil {
		t.Fatal(err)
	}
	matchedOther, err := rules.Match(other)
	if err != nil {
		t.Fatal(err)
	}

	if !matchedAllowed {
		t.Fatal("allowed full path did not match")
	}
	if matchedOther {
		t.Fatal("same basename at a different full path matched")
	}
}

func writeTestFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalizeTestPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absPath)
}

func canonicalizeTestPathRequired(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalizeTestPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

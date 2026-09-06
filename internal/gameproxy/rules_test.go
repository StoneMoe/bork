//go:build game_proxy

package gameproxy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

	rules, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalizeTestPath)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{canonicalizeTestPathRequired(t, first), canonicalizeTestPathRequired(t, second)}
	sort.Strings(want)
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %q, want %q", got, want)
	}
}

func TestScanExecutableRulesMergesMultipleRootsWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	first := writeTestFile(t, filepath.Join(root, "first.exe"))
	second := writeTestFile(t, filepath.Join(nested, "second.exe"))

	rules, err := ScanExecutableRules(t.Context(), []string{root, nested})
	if err != nil {
		t.Fatal(err)
	}
	firstCanonical, err := canonicalPath(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCanonical, err := canonicalPath(second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{firstCanonical, secondCanonical}
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

	rules, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalize)

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

	_, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalize)

	if !errors.Is(err, ErrExecutableOutsideRoot) {
		t.Fatalf("error = %v, want ErrExecutableOutsideRoot", err)
	}
}

func TestScanExecutableRulesSkipsDirectoryLinksWithoutSkippingSiblings(t *testing.T) {
	root := t.TempDir()
	inside := writeTestFile(t, filepath.Join(root, "game.exe"))
	externalRoot := t.TempDir()
	writeTestFile(t, filepath.Join(externalRoot, "outside.exe"))
	for _, name := range []string{"00-linked", "01-linked.exe"} {
		link := filepath.Join(root, name)
		if runtime.GOOS == "windows" {
			// Junctions exercise directory reparse points without symlink privileges.
			if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, externalRoot).CombinedOutput(); err != nil {
				t.Fatalf("create directory junction: %v: %s", err, output)
			}
		} else if err := os.Symlink(externalRoot, link); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
	}

	rules, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalizeTestPath)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{canonicalizeTestPathRequired(t, inside)}
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() = %q, want %q", got, want)
	}

	// An explicitly configured descendant through a skipped junction still
	// owns its own scan; it must not be discarded as a redundant nested root.
	explicit := writeTestFile(t, filepath.Join(externalRoot, "nested", "allowed.exe"))
	rules, err = ScanExecutableRules(t.Context(), []string{root, filepath.Join(root, "00-linked", "nested")})
	if err != nil {
		t.Fatal(err)
	}
	want = nil
	for _, path := range []string{inside, explicit} {
		canonical, err := canonicalPath(path)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, canonical)
	}
	sort.Strings(want)
	if got := rules.Paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Paths() with explicit linked descendant = %q, want %q", got, want)
	}
}

func TestScanExecutableRulesReturnsWalkErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	canonicalize := func(path string) (string, error) {
		return filepath.Abs(path)
	}

	_, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalize)

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

	_, err := scanExecutableRulesFromRoots(t.Context(), []string{root}, canonicalize)

	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("error = %v, want fs.ErrPermission", err)
	}
}

func TestExecutableRulesMatchUsesCanonicalFullPathOnly(t *testing.T) {
	workspace := t.TempDir()
	allowed := writeTestFile(t, filepath.Join(workspace, "allowed", "game.exe"))
	other := writeTestFile(t, filepath.Join(workspace, "other", "game.exe"))

	rules, err := ScanExecutableRules(t.Context(), []string{filepath.Dir(allowed)})
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

func TestScanExecutableRulesCancellation(t *testing.T) {
	for _, stage := range []string{"before root", "during walk", "between roots", "before result", "empty roots"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			first := writeTestFile(t, filepath.Join(root, "first.exe"))
			roots := []string{root}
			if stage == "during walk" {
				writeTestFile(t, filepath.Join(root, "second.exe"))
			}
			if stage == "between roots" {
				roots = append(roots, t.TempDir())
			}
			if stage == "empty roots" {
				roots = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantCalls := 2 // The first root and its first executable.
			if stage == "before root" || stage == "empty roots" {
				cancel()
				wantCalls = 0
			}
			calls := 0
			canonicalize := func(path string) (string, error) {
				calls++
				if path == first {
					cancel()
				}
				return canonicalizeTestPath(path)
			}

			rules, err := scanExecutableRulesFromRoots(ctx, roots, canonicalize)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if calls != wantCalls {
				t.Fatalf("canonicalization calls = %d, want %d", calls, wantCalls)
			}
			if rules.pathSet != nil || rules.paths != nil {
				t.Fatalf("canceled scan returned partial rules: %#v", rules)
			}
		})
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

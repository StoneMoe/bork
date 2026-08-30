package gameproxy

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

var ErrExecutableOutsideRoot = errors.New("gameproxy: executable outside scan root")

type pathCanonicalizer func(string) (string, error)

type ExecutableRules struct {
	paths        []string
	pathSet      map[string]struct{}
	canonicalize pathCanonicalizer
}

func ScanExecutableRules(root string) (ExecutableRules, error) {
	return scanExecutableRules(root, canonicalPath)
}

func (rules ExecutableRules) Paths() []string {
	return slices.Clone(rules.paths)
}

func (rules ExecutableRules) Match(executablePath string) (bool, error) {
	canonicalPath, err := rules.canonicalize(executablePath)
	if err != nil {
		return false, fmt.Errorf("canonicalize executable %q: %w", executablePath, err)
	}
	_, matched := rules.pathSet[canonicalPath]
	return matched, nil
}

func scanExecutableRules(root string, canonicalize pathCanonicalizer) (ExecutableRules, error) {
	canonicalRoot, err := canonicalize(root)
	if err != nil {
		return ExecutableRules{}, fmt.Errorf("canonicalize scan root %q: %w", root, err)
	}

	pathSet := make(map[string]struct{})
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("scan path %q: %w", path, walkErr)
		}
		reparseDirectory, err := isDirectoryReparsePoint(path, entry)
		if err != nil {
			return fmt.Errorf("inspect path %q: %w", path, err)
		}
		if reparseDirectory {
			return fs.SkipDir
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".exe") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect executable %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		canonicalExecutable, err := canonicalize(path)
		if err != nil {
			return fmt.Errorf("canonicalize executable %q: %w", path, err)
		}
		contained, err := pathWithinRoot(canonicalRoot, canonicalExecutable)
		if err != nil {
			return fmt.Errorf("compare executable %q with scan root: %w", canonicalExecutable, err)
		}
		if !contained {
			return fmt.Errorf("%q: %w", canonicalExecutable, ErrExecutableOutsideRoot)
		}
		pathSet[canonicalExecutable] = struct{}{}
		return nil
	})
	if err != nil {
		return ExecutableRules{}, fmt.Errorf("scan executable root %q: %w", root, err)
	}

	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return ExecutableRules{paths: paths, pathSet: pathSet, canonicalize: canonicalize}, nil
}

func pathWithinRoot(root, path string) (bool, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false, err
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative), nil
}

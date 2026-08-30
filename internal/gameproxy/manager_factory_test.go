package gameproxy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"bork/internal/gameproxy/iwan"
)

func TestManager_Start_passes_canonical_paths_to_bridge_in_stable_order(t *testing.T) {
	// Given
	root := t.TempDir()
	zeta := writeTestFile(t, filepath.Join(root, "zeta.exe"))
	alpha := writeTestFile(t, filepath.Join(root, "alpha.exe"))
	want := make([]string, 0, 2)
	for _, path := range []string{alpha, zeta} {
		canonical, err := canonicalPath(path)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, canonical)
	}
	slices.Sort(want)
	log := &eventLog{}
	bridge := newFakeBridge(log)
	bridgeFactory := &fakeBridgeFactory{log: log, supported: true, bridge: bridge}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 1})
	manager := newManager(managerDependencies{
		bridge:        bridgeFactory,
		newSupervisor: func(iwan.Options) (supervisorRuntime, error) { return supervisor, nil },
	})
	input := validStartInput()
	input.Directory = root

	// When
	err := manager.Start(context.Background(), input)

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if got := bridgeFactory.receivedPaths(); !slices.Equal(got, want) {
		t.Fatalf("BridgeFactory paths = %q, want %q", got, want)
	}
	manager.Stop()
}

func TestManager_Start_keeps_rule_paths_immutable_from_bridge_factory(t *testing.T) {
	// Given
	want := []string{"C:/Games/Alpha.exe", "C:/Games/Zeta.exe"}
	managerPaths := slices.Clone(want)
	matcher := &ownedPathMatcher{paths: managerPaths}
	log := &eventLog{}
	bridge := newFakeBridge(log)
	bridgeFactory := &fakeBridgeFactory{log: log, supported: true, bridge: bridge, mutatePaths: true}
	supervisor := newFakeSupervisor(log, iwan.Status{State: iwan.StateReady, Generation: 2})
	manager := newManager(managerDependencies{
		bridge: bridgeFactory,
		scanRules: func(string) (ruleSet, error) {
			return ruleSet{matcher: matcher, paths: managerPaths, executableCount: len(managerPaths)}, nil
		},
		newSupervisor: func(iwan.Options) (supervisorRuntime, error) { return supervisor, nil },
	})

	// When
	err := manager.Start(context.Background(), validStartInput())

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managerPaths, want) {
		t.Fatalf("manager rule paths = %q, want immutable %q", managerPaths, want)
	}
	if got := bridgeFactory.receivedPaths(); !slices.Equal(got, want) {
		t.Fatalf("BridgeFactory paths = %q, want %q", got, want)
	}
	matched, matchErr := matcher.Match(want[0])
	if matchErr != nil || !matched {
		t.Fatalf("manager matcher after factory mutation = %v, %v", matched, matchErr)
	}
	manager.Stop()
}

type ownedPathMatcher struct{ paths []string }

func (matcher *ownedPathMatcher) Match(path string) (bool, error) {
	return slices.Contains(matcher.paths, path), nil
}

package gameproxy

import (
	"context"
	"time"

	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

type BridgeFactory interface {
	Supported() bool
	EnsureAvailable(context.Context) error
	New(context.Context, []string) (intercept.Bridge, error)
}

type supervisorRuntime interface {
	intercept.Dialer
	Start(context.Context) error
	Stop()
	WaitReady(context.Context) error
	Status() iwan.Status
	Changes() <-chan struct{}
}

type ruleSet struct {
	matcher         intercept.ExecutableMatcher
	paths           []string
	executableCount int
}

type managerDependencies struct {
	bridge        BridgeFactory
	scanRules     func(string) (ruleSet, error)
	newSupervisor func(iwan.Options) (supervisorRuntime, error)
}

func defaultDependencies(bridge BridgeFactory) managerDependencies {
	return managerDependencies{
		bridge: bridge,
		scanRules: func(directory string) (ruleSet, error) {
			rules, err := ScanExecutableRules(directory)
			if err != nil {
				return ruleSet{}, err
			}
			paths := rules.Paths()
			return ruleSet{matcher: rules, paths: paths, executableCount: len(paths)}, nil
		},
		newSupervisor: func(options iwan.Options) (supervisorRuntime, error) {
			return iwan.NewSupervisor(options)
		},
	}
}

func (dependencies managerDependencies) withDefaults() managerDependencies {
	defaults := defaultDependencies(dependencies.bridge)
	if dependencies.scanRules == nil {
		dependencies.scanRules = defaults.scanRules
	}
	if dependencies.newSupervisor == nil {
		dependencies.newSupervisor = defaults.newSupervisor
	}
	return dependencies
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

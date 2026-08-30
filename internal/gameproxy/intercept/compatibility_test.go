package intercept_test

import (
	"bork/internal/gameproxy"
	"bork/internal/gameproxy/intercept"
	"bork/internal/gameproxy/iwan"
)

var _ intercept.Dialer = (*iwan.Supervisor)(nil)
var _ intercept.ExecutableMatcher = gameproxy.ExecutableRules{}

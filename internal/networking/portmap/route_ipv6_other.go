//go:build !windows

package portmap

import routegateway "github.com/jackpal/gateway"

// Non-Windows platforms keep the gateway package path. Windows uses the
// native route API because parsing localized command output is unreliable.
func discoverIPv6Route() (defaultRoute, error) {
	return resolveDefaultRoute(routegateway.DiscoverInterfaceIPv6, routegateway.DiscoverGatewayIPv6, true)
}

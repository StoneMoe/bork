package portmap

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	routegateway "github.com/jackpal/gateway"
)

const defaultRouteCacheTTL = 30 * time.Second

type defaultRoute struct {
	gateway net.IP
	local   net.IP
	zone    string
}

type routeAddressFunc func() (net.IP, error)
type routeDiscoverFunc func() (defaultRoute, error)

type defaultRouteCache struct {
	slot  chan struct{}
	mu    sync.Mutex
	route defaultRoute
	until time.Time
}

var (
	ipv4RouteCache = newDefaultRouteCache()
	ipv6RouteCache = newDefaultRouteCache()

	discoverInterface = routegateway.DiscoverInterface
	discoverGateway   = routegateway.DiscoverGateway
)

func newDefaultRouteCache() *defaultRouteCache {
	return &defaultRouteCache{slot: make(chan struct{}, 1)}
}

func discoverDefaultRoute(ctx context.Context) (defaultRoute, error) {
	return ipv4RouteCache.discover(ctx, discoverIPv4Route)
}

func discoverDefaultIPv6Route(ctx context.Context) (defaultRoute, error) {
	return ipv6RouteCache.discover(ctx, discoverIPv6Route)
}

func discoverIPv4Route() (defaultRoute, error) {
	return resolveDefaultRoute(discoverInterface, discoverGateway, false)
}

func (cache *defaultRouteCache) discover(ctx context.Context, discover routeDiscoverFunc) (defaultRoute, error) {
	if ctx == nil {
		return defaultRoute{}, errors.New("default route context is required")
	}
	if route, ok := cache.current(); ok {
		return route, nil
	}
	select {
	case cache.slot <- struct{}{}:
	case <-ctx.Done():
		return defaultRoute{}, ctx.Err()
	}
	if route, ok := cache.current(); ok {
		<-cache.slot
		return route, nil
	}
	return cache.resolve(ctx, discover)
}

func (cache *defaultRouteCache) current() (defaultRoute, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !time.Now().Before(cache.until) {
		return defaultRoute{}, false
	}
	return cloneDefaultRoute(cache.route), true
}

type routeResult struct {
	route defaultRoute
	err   error
}

func (cache *defaultRouteCache) resolve(ctx context.Context, discover routeDiscoverFunc) (defaultRoute, error) {
	result := make(chan routeResult, 1)
	go func() {
		defer func() { <-cache.slot }()
		route, err := discover()
		resolved := routeResult{route: route, err: err}
		if resolved.err == nil {
			cache.store(resolved.route)
		}
		result <- resolved
	}()
	select {
	case resolved := <-result:
		return resolved.route, resolved.err
	case <-ctx.Done():
		return defaultRoute{}, ctx.Err()
	}
}

func resolveDefaultRoute(localAddress, gatewayAddress routeAddressFunc, needsZone bool) (defaultRoute, error) {
	local, err := localAddress()
	if err != nil {
		return defaultRoute{}, err
	}
	gateway, err := gatewayAddress()
	if err != nil {
		return defaultRoute{}, err
	}
	route := defaultRoute{gateway: append(net.IP(nil), gateway...), local: append(net.IP(nil), local...)}
	if needsZone && gateway.IsLinkLocalUnicast() {
		route.zone, err = interfaceNameForIP(local)
	}
	return route, err
}

func (cache *defaultRouteCache) store(route defaultRoute) {
	cache.mu.Lock()
	cache.route = cloneDefaultRoute(route)
	cache.until = time.Now().Add(defaultRouteCacheTTL)
	cache.mu.Unlock()
}

// IPv6 link-local gateways require an interface zone when used in a socket
// address. The gateway package returns the address without that zone, so use
// the interface that owns the selected source address.
func interfaceNameForIP(target net.IP) (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, networkInterface := range interfaces {
		if interfaceHasIP(networkInterface, target) {
			return networkInterface.Name, nil
		}
	}
	return "", errors.New("default route interface was not found")
}

func interfaceHasIP(networkInterface net.Interface, target net.IP) bool {
	addresses, err := networkInterface.Addrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if addressIP(address).Equal(target) {
			return true
		}
	}
	return false
}

func addressIP(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		return nil
	}
}

func cloneDefaultRoute(route defaultRoute) defaultRoute {
	return defaultRoute{gateway: append(net.IP(nil), route.gateway...), local: append(net.IP(nil), route.local...), zone: route.zone}
}

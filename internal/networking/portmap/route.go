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
}

var (
	defaultRouteSlot    = make(chan struct{}, 1)
	defaultRouteMu      sync.Mutex
	defaultRouteWorkers sync.WaitGroup
	cachedRoute         defaultRoute
	cachedRouteUntil    time.Time
	discoverInterface   = routegateway.DiscoverInterface
	discoverGateway     = routegateway.DiscoverGateway
)

func discoverDefaultRoute(ctx context.Context) (defaultRoute, error) {
	if ctx == nil {
		return defaultRoute{}, errors.New("default route context is required")
	}
	defaultRouteMu.Lock()
	if time.Now().Before(cachedRouteUntil) {
		route := cloneDefaultRoute(cachedRoute)
		defaultRouteMu.Unlock()
		return route, nil
	}
	defaultRouteMu.Unlock()

	select {
	case defaultRouteSlot <- struct{}{}:
	case <-ctx.Done():
		return defaultRoute{}, ctx.Err()
	}
	defaultRouteMu.Lock()
	if time.Now().Before(cachedRouteUntil) {
		route := cloneDefaultRoute(cachedRoute)
		defaultRouteMu.Unlock()
		<-defaultRouteSlot
		return route, nil
	}
	defaultRouteMu.Unlock()
	result := make(chan struct {
		route defaultRoute
		err   error
	}, 1)
	defaultRouteWorkers.Add(1)
	go func() {
		defer defaultRouteWorkers.Done()
		defer func() { <-defaultRouteSlot }()
		local, err := discoverInterface()
		if err == nil {
			var gateway net.IP
			gateway, err = discoverGateway()
			if err == nil {
				route := defaultRoute{gateway: append(net.IP(nil), gateway...), local: append(net.IP(nil), local...)}
				defaultRouteMu.Lock()
				cachedRoute = route
				cachedRouteUntil = time.Now().Add(defaultRouteCacheTTL)
				defaultRouteMu.Unlock()
				result <- struct {
					route defaultRoute
					err   error
				}{route: cloneDefaultRoute(route)}
				return
			}
		}
		result <- struct {
			route defaultRoute
			err   error
		}{err: err}
	}()
	select {
	case resolved := <-result:
		return resolved.route, resolved.err
	case <-ctx.Done():
		return defaultRoute{}, ctx.Err()
	}
}

func cloneDefaultRoute(route defaultRoute) defaultRoute {
	return defaultRoute{gateway: append(net.IP(nil), route.gateway...), local: append(net.IP(nil), route.local...)}
}

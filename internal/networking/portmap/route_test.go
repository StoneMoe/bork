package portmap

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultRouteDiscoveryCancellationLeavesAtMostOneWorker(t *testing.T) {
	oldInterface, oldGateway := discoverInterface, discoverGateway
	defer func() {
		discoverInterface, discoverGateway = oldInterface, oldGateway
		defaultRouteMu.Lock()
		cachedRouteUntil = time.Time{}
		defaultRouteMu.Unlock()
	}()
	defaultRouteMu.Lock()
	cachedRouteUntil = time.Time{}
	defaultRouteMu.Unlock()
	release := make(chan struct{})
	var calls atomic.Int32
	discoverInterface = func() (net.IP, error) {
		calls.Add(1)
		<-release
		return net.IPv4(192, 168, 1, 2), nil
	}
	discoverGateway = func() (net.IP, error) { return net.IPv4(192, 168, 1, 1), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := discoverDefaultRoute(ctx); err == nil {
		t.Fatal("blocked route discovery ignored cancellation")
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer secondCancel()
	if _, err := discoverDefaultRoute(secondCtx); err == nil {
		t.Fatal("second blocked route discovery ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("route discovery workers = %d, want 1", calls.Load())
	}
	close(release)
	defaultRouteWorkers.Wait()
}

func TestConcurrentDefaultRouteDiscoverySharesResult(t *testing.T) {
	oldInterface, oldGateway := discoverInterface, discoverGateway
	defer func() {
		discoverInterface, discoverGateway = oldInterface, oldGateway
		defaultRouteMu.Lock()
		cachedRouteUntil = time.Time{}
		defaultRouteMu.Unlock()
	}()
	defaultRouteMu.Lock()
	cachedRouteUntil = time.Time{}
	defaultRouteMu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	discoverInterface = func() (net.IP, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return net.IPv4(192, 168, 1, 2), nil
	}
	discoverGateway = func() (net.IP, error) { return net.IPv4(192, 168, 1, 1), nil }

	results := make(chan error, 2)
	go func() {
		_, err := discoverDefaultRoute(context.Background())
		results <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first route discovery did not start")
	}
	go func() {
		_, err := discoverDefaultRoute(context.Background())
		results <- err
	}()
	close(release)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent route discovery did not finish")
		}
	}
	defaultRouteWorkers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("route discovery workers = %d, want shared result from one worker", calls.Load())
	}
}

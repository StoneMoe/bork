//go:build windows

package portmap

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetBestRoute2 only reads the route table; it does not send traffic to this
// address. Use an ordinary global address so a PCP anycast route cannot change
// which gateway and source address Windows selects.
const ipv6RouteProbe = "2606:4700:4700::1111"

var getBestRoute2 = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetBestRoute2")

func discoverIPv6Route() (defaultRoute, error) {
	row, source, err := bestIPv6Route()
	if err != nil {
		return defaultRoute{}, err
	}
	if row.DestinationPrefix.PrefixLength != 0 {
		return defaultRoute{}, errors.New("GetBestRoute2 did not select an IPv6 default route")
	}
	if row.InterfaceIndex == 0 {
		return defaultRoute{}, errors.New("GetBestRoute2 returned no interface")
	}
	gateway, err := ipv6Address(&row.NextHop)
	if err != nil {
		return defaultRoute{}, err
	}
	local, err := ipv6Address(&source)
	if err != nil {
		return defaultRoute{}, err
	}

	// A numeric interface index is a valid IPv6 zone and comes from the same
	// route result as the gateway and source address.
	zone := strconv.FormatUint(uint64(row.InterfaceIndex), 10)
	return defaultRoute{gateway: gateway, local: local, zone: zone}, nil
}

func bestIPv6Route() (windows.MibIpForwardRow2, windows.RawSockaddrInet, error) {
	probe := netip.MustParseAddr(ipv6RouteProbe)
	var interfaceIndex uint32
	if err := windows.GetBestInterfaceEx(&windows.SockaddrInet6{Addr: probe.As16()}, &interfaceIndex); err != nil {
		return windows.MibIpForwardRow2{}, windows.RawSockaddrInet{}, err
	}
	if interfaceIndex == 0 {
		return windows.MibIpForwardRow2{}, windows.RawSockaddrInet{}, errors.New("Windows returned no IPv6 route interface")
	}

	destination := ipv6Sockaddr(probe)
	var row windows.MibIpForwardRow2
	var source windows.RawSockaddrInet

	// GetBestRoute2 requires a route interface. Leave the source unset so the
	// returned route and source address come from the same native lookup.
	status, _, _ := getBestRoute2.Call(
		0,
		uintptr(interfaceIndex),
		0,
		uintptr(unsafe.Pointer(&destination)),
		0,
		uintptr(unsafe.Pointer(&row)),
		uintptr(unsafe.Pointer(&source)),
	)
	if status != 0 {
		return windows.MibIpForwardRow2{}, windows.RawSockaddrInet{}, syscall.Errno(status)
	}
	return row, source, nil
}

// RawSockaddrInet is a C union. The family field tells Windows and this code
// that the remaining bytes use the IPv6 layout.
func ipv6Sockaddr(address netip.Addr) windows.RawSockaddrInet {
	var raw windows.RawSockaddrInet
	raw6 := (*windows.RawSockaddrInet6)(unsafe.Pointer(&raw))
	raw6.Family = windows.AF_INET6
	raw6.Addr = address.As16()
	return raw
}

func ipv6Address(raw *windows.RawSockaddrInet) (net.IP, error) {
	if raw.Family != windows.AF_INET6 {
		return nil, errors.New("GetBestRoute2 returned a non-IPv6 address")
	}
	raw6 := (*windows.RawSockaddrInet6)(unsafe.Pointer(raw))
	return append(net.IP(nil), raw6.Addr[:]...), nil
}

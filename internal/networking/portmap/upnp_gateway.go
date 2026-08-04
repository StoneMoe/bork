package portmap

import (
	"context"
	"net"
	"sort"
	"sync"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

const (
	providerWANIP2           = "UPnP IGD WANIPConnection2"
	providerWANIP1           = "UPnP IGD WANIPConnection1"
	providerWANPPP           = "UPnP IGD WANPPPConnection1"
	maxUPnPGatewayCandidates = 16
)

type portMappingClient interface {
	AddPortMappingCtx(context.Context, string, uint16, string, uint16, string, bool, string, uint32) error
	DeletePortMappingCtx(context.Context, string, uint16, string) error
	GetExternalIPAddressCtx(context.Context) (string, error)
	GetSpecificPortMappingEntryCtx(context.Context, string, uint16, string) (uint16, string, bool, string, uint32, error)
}

type igdGateway struct {
	key    string
	name   string
	local  net.IP
	remote net.IP
	client portMappingClient
}

func (g *igdGateway) id() string       { return g.key }
func (g *igdGateway) provider() string { return g.name }
func (g *igdGateway) localAddr() net.IP {
	return append(net.IP(nil), g.local...)
}

func (g *igdGateway) addPortMapping(ctx context.Context, externalPort, internalPort uint16, internalIP, description string, lease uint32) error {
	return g.client.AddPortMappingCtx(ctx, "", externalPort, mappingProtocol, internalPort, internalIP, true, description, lease)
}

func (g *igdGateway) getSpecificPortMapping(ctx context.Context, externalPort uint16) (portMappingEntry, error) {
	internalPort, internalIP, enabled, description, lease, err := g.client.GetSpecificPortMappingEntryCtx(ctx, "", externalPort, mappingProtocol)
	return portMappingEntry{
		internalPort:  internalPort,
		internalIP:    internalIP,
		enabled:       enabled,
		description:   description,
		leaseDuration: lease,
	}, err
}

func (g *igdGateway) externalIPAddress(ctx context.Context) (string, error) {
	return g.client.GetExternalIPAddressCtx(ctx)
}

func (g *igdGateway) deletePortMapping(ctx context.Context, externalPort uint16) error {
	return g.client.DeletePortMappingCtx(ctx, "", externalPort, mappingProtocol)
}

type wanIP2Discovery struct {
	clients []*internetgateway2.WANIPConnection2
	errors  []error
	err     error
}

type wanIP1Discovery struct {
	clients []*internetgateway2.WANIPConnection1
	errors  []error
	err     error
}

type wanPPPDiscovery struct {
	clients []*internetgateway2.WANPPPConnection1
	errors  []error
	err     error
}

func discoverGateways(ctx context.Context) ([]gateway, error) {
	var ip2 wanIP2Discovery
	var ip1 wanIP1Discovery
	var ppp wanPPPDiscovery
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		ip2.clients, ip2.errors, ip2.err = internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	}()
	go func() {
		defer workers.Done()
		ip1.clients, ip1.errors, ip1.err = internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	}()
	go func() {
		defer workers.Done()
		ppp.clients, ppp.errors, ppp.err = internetgateway2.NewWANPPPConnection1ClientsCtx(ctx)
	}()
	workers.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	gateways := make([]gateway, 0, min(maxUPnPGatewayCandidates, len(ip2.clients)+len(ip1.clients)+len(ppp.clients)))
	seen := make(map[string]struct{}, maxUPnPGatewayCandidates)
	for _, client := range ip2.clients {
		if len(gateways) == maxUPnPGatewayCandidates {
			break
		}
		if client != nil {
			gateways = appendUniqueGateway(gateways, seen, newIGDGateway(&client.ServiceClient, client, providerWANIP2))
		}
	}
	for _, client := range ip1.clients {
		if len(gateways) == maxUPnPGatewayCandidates {
			break
		}
		if client != nil {
			gateways = appendUniqueGateway(gateways, seen, newIGDGateway(&client.ServiceClient, client, providerWANIP1))
		}
	}
	for _, client := range ppp.clients {
		if len(gateways) == maxUPnPGatewayCandidates {
			break
		}
		if client != nil {
			gateways = appendUniqueGateway(gateways, seen, newIGDGateway(&client.ServiceClient, client, providerWANPPP))
		}
	}
	if len(gateways) > 1 {
		if route, err := discoverDefaultRoute(ctx); err == nil {
			sort.SliceStable(gateways, func(i, j int) bool {
				return upnpGatewayRank(gateways[i], route) < upnpGatewayRank(gateways[j], route)
			})
		}
	}

	var failures errorSummary
	failures.add("WANIPConnection2 discovery", ip2.err)
	for _, err := range ip2.errors {
		failures.add("WANIPConnection2 device", err)
	}
	failures.add("WANIPConnection1 discovery", ip1.err)
	for _, err := range ip1.errors {
		failures.add("WANIPConnection1 device", err)
	}
	failures.add("WANPPPConnection1 discovery", ppp.err)
	for _, err := range ppp.errors {
		failures.add("WANPPPConnection1 device", err)
	}
	if len(failures.parts) == 0 {
		return gateways, nil
	}
	return gateways, failures.err("UPnP discovery")
}

func appendUniqueGateway(gateways []gateway, seen map[string]struct{}, candidate gateway) []gateway {
	if candidate == nil || len(gateways) >= maxUPnPGatewayCandidates {
		return gateways
	}
	if id := candidate.id(); id != "" {
		if _, exists := seen[id]; exists {
			return gateways
		}
		seen[id] = struct{}{}
	}
	return append(gateways, candidate)
}

func newIGDGateway(service *goupnp.ServiceClient, client portMappingClient, provider string) *igdGateway {
	if service.SOAPClient != nil {
		service.SOAPClient.HTTPClient = newUPnPSOAPHTTPClient()
	}
	var remote net.IP
	if service.Location != nil {
		remote = net.ParseIP(service.Location.Hostname())
	}
	return &igdGateway{
		key:    serviceClientKey(service, provider),
		name:   provider,
		local:  append(net.IP(nil), service.LocalAddr()...),
		remote: append(net.IP(nil), remote...),
		client: client,
	}
}

func upnpGatewayRank(candidate gateway, route defaultRoute) int {
	if igd, ok := candidate.(*igdGateway); ok && igd.remote != nil && igd.remote.Equal(route.gateway) {
		return 0
	}
	if local := candidate.localAddr(); local != nil && local.Equal(route.local) {
		return 1
	}
	return 2
}

func serviceClientKey(client *goupnp.ServiceClient, provider string) string {
	if client == nil {
		return ""
	}
	location := ""
	if client.Location != nil {
		location = client.Location.String()
	}
	controlURL := ""
	serviceID := ""
	if client.Service != nil {
		serviceID = client.Service.ServiceId
		if client.Service.ControlURL.Ok {
			controlURL = client.Service.ControlURL.URL.String()
		}
	}
	deviceID := ""
	if client.RootDevice != nil {
		deviceID = client.RootDevice.Device.UDN
	}
	localAddress := client.LocalAddr().String()
	if location == "" && controlURL == "" && serviceID == "" && deviceID == "" {
		return ""
	}
	if deviceID != "" && serviceID != "" {
		return provider + "\x00" + deviceID + "\x00" + serviceID + "\x00" + localAddress
	}
	return provider + "\x00" + deviceID + "\x00" + location + "\x00" + serviceID + "\x00" + controlURL + "\x00" + localAddress
}

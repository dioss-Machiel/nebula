package nebula

import (
	"net/netip"
	"testing"

	"github.com/slackhq/nebula/firewall"
	"github.com/slackhq/nebula/util"
	"github.com/stretchr/testify/assert"
)

func TestPacketsAreBalancedEqually(t *testing.T) {

	gateways := []util.Gateway{}

	gw1Ip := netip.MustParseAddr("1.0.0.1")
	gw2Ip := netip.MustParseAddr("1.0.0.2")
	gw3Ip := netip.MustParseAddr("1.0.0.3")

	gateways = append(gateways, util.NewGateway(gw1Ip, 1))
	gateways = append(gateways, util.NewGateway(gw2Ip, 1))
	gateways = append(gateways, util.NewGateway(gw3Ip, 1))

	util.RebalanceGateways(gateways)

	gw1count := 0
	gw2count := 0
	gw3count := 0

	i := uint16(0)
	for ; i < 65535; i++ {
		packet := firewall.Packet{
			LocalIP:    netip.MustParseAddr("192.168.1.1"),
			RemoteIP:   netip.MustParseAddr("10.0.0.1"),
			LocalPort:  i,
			RemotePort: 65535 - i,
			Protocol:   6, // TCP
			Fragment:   false,
		}

		selectedGw := selectRoute(&packet, gateways)

		switch selectedGw {
		case gw1Ip:
			gw1count += 1
		case gw2Ip:
			gw2count += 1
		case gw3Ip:
			gw3count += 1
		}

	}

	assert.Equal(t, 21930, gw1count)
	assert.Equal(t, 21937, gw2count)
	assert.Equal(t, 21668, gw3count)

}

func TestPacketsAreBalancedByPriority(t *testing.T) {

	gateways := []util.Gateway{}

	gw1Ip := netip.MustParseAddr("1.0.0.1")
	gw2Ip := netip.MustParseAddr("1.0.0.2")

	gateways = append(gateways, util.NewGateway(gw1Ip, 3))
	gateways = append(gateways, util.NewGateway(gw2Ip, 2))

	util.RebalanceGateways(gateways)

	gw1count := 0
	gw2count := 0

	i := uint16(0)
	for ; i < 65535; i++ {
		packet := firewall.Packet{
			LocalIP:    netip.MustParseAddr("192.168.1.1"),
			RemoteIP:   netip.MustParseAddr("10.0.0.1"),
			LocalPort:  i,
			RemotePort: 65535 - i,
			Protocol:   6, // TCP
			Fragment:   false,
		}

		selectedGw := selectRoute(&packet, gateways)

		switch selectedGw {
		case gw1Ip:
			gw1count += 1
		case gw2Ip:
			gw2count += 1
		}

	}

	assert.Equal(t, 39515, gw1count)
	assert.Equal(t, 26020, gw2count)

}

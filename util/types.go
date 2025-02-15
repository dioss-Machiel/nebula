package util

import "net/netip"

type EE_NewRouteType []Gateway

type Gateway struct {
	ip         netip.Addr
	weight     int
	upperBound int
}

func (g *Gateway) UpperBound() int {
	return g.upperBound
}

func NewGateway(ip netip.Addr, weight int) Gateway {
	return Gateway{ip: ip, weight: weight}
}

func (g *Gateway) SetUpperBound(i int) {
	g.upperBound = i
}

func (g *Gateway) Ip() netip.Addr {
	return g.ip
}

func (g *Gateway) Weight() int {
	return g.weight
}

// Divide and round to nearest integer
func divAndRound(v uint64, d uint64) uint64 {
	var tmp uint64 = v + d/2
	return tmp / d
}

// Implements Hash-Threshold mapping
// Follows the same algorithm as in the linux kernel.
func RebalanceGateways(gateways []Gateway) {

	var totalWeight int = 0
	for i := range gateways {
		totalWeight += gateways[i].weight
	}

	var loopWeight int = 0
	for i := range gateways {
		loopWeight += gateways[i].weight
		gateways[i].SetUpperBound(int(divAndRound(uint64(loopWeight)<<31, uint64(totalWeight))) - 1)
	}

}

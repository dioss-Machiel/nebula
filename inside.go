package nebula

import (
	"net/netip"

	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula/firewall"
	"github.com/slackhq/nebula/header"
	"github.com/slackhq/nebula/iputil"
	"github.com/slackhq/nebula/noiseutil"
	"github.com/slackhq/nebula/util"
	"github.com/zeebo/xxh3"
)

func (f *Interface) consumeInsidePacket(packet []byte, fwPacket *firewall.Packet, nb, out []byte, q int, localCache firewall.ConntrackCache) {
	err := newPacket(packet, false, fwPacket)
	if err != nil {
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("packet", packet).Debugf("Error while validating outbound packet: %s", err)
		}
		return
	}

	// Ignore local broadcast packets
	if f.dropLocalBroadcast && fwPacket.RemoteIP == f.myBroadcastAddr {
		return
	}

	if fwPacket.RemoteIP == f.myVpnNet.Addr() {
		// Immediately forward packets from self to self.
		// This should only happen on Darwin-based and FreeBSD hosts, which
		// routes packets from the Nebula IP to the Nebula IP through the Nebula
		// TUN device.
		if immediatelyForwardToSelf {
			_, err := f.readers[q].Write(packet)
			if err != nil {
				f.l.WithError(err).Error("Failed to forward to tun")
			}
		}
		// Otherwise, drop. On linux, we should never see these packets - Linux
		// routes packets from the nebula IP to the nebula IP through the loopback device.
		return
	}

	// Ignore multicast packets
	if f.dropMulticast && fwPacket.RemoteIP.IsMulticast() {
		return
	}

	hostinfo, ready := f.getOrHandshakeConsiderRouting(fwPacket, func(hh *HandshakeHostInfo) {
		hh.cachePacket(f.l, header.Message, 0, packet, f.sendMessageNow, f.cachedPacketMetrics)
	})

	if hostinfo == nil {
		f.rejectInside(packet, out, q)
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("vpnIp", fwPacket.RemoteIP).
				WithField("fwPacket", fwPacket).
				Debugln("dropping outbound packet, vpnIp not in our CIDR or in unsafe routes")
		}
		return
	}

	if !ready {
		return
	}

	dropReason := f.firewall.Drop(*fwPacket, false, hostinfo, f.pki.GetCAPool(), localCache)
	if dropReason == nil {
		f.sendNoMetrics(header.Message, 0, hostinfo.ConnectionState, hostinfo, netip.AddrPort{}, packet, nb, out, q)

	} else {
		f.rejectInside(packet, out, q)
		if f.l.Level >= logrus.DebugLevel {
			hostinfo.logger(f.l).
				WithField("fwPacket", fwPacket).
				WithField("reason", dropReason).
				Debugln("dropping outbound packet")
		}
	}
}

func (f *Interface) rejectInside(packet []byte, out []byte, q int) {
	if !f.firewall.InSendReject {
		return
	}

	out = iputil.CreateRejectPacket(packet, out)
	if len(out) == 0 {
		return
	}

	_, err := f.readers[q].Write(out)
	if err != nil {
		f.l.WithError(err).Error("Failed to write to tun")
	}
}

func (f *Interface) rejectOutside(packet []byte, ci *ConnectionState, hostinfo *HostInfo, nb, out []byte, q int) {
	if !f.firewall.OutSendReject {
		return
	}

	out = iputil.CreateRejectPacket(packet, out)
	if len(out) == 0 {
		return
	}

	if len(out) > iputil.MaxRejectPacketSize {
		if f.l.GetLevel() >= logrus.InfoLevel {
			f.l.
				WithField("packet", packet).
				WithField("outPacket", out).
				Info("rejectOutside: packet too big, not sending")
		}
		return
	}

	f.sendNoMetrics(header.Message, 0, ci, hostinfo, netip.AddrPort{}, out, nb, packet, q)
}

// This should only called for internal nebula traffic
func (f *Interface) Handshake(vpnIp netip.Addr) {
	f.getOrHandshakeNoRouting(vpnIp, nil)
}

// getOrHandshakeNoRouting returns nil if the vpnIp is not in the VPN net
// If the 2nd return var is false then the hostinfo is not ready to be used in a tunnel
func (f *Interface) getOrHandshakeNoRouting(vpnIp netip.Addr, cacheCallback func(*HandshakeHostInfo)) (*HostInfo, bool) {
	if !f.myVpnNet.Contains(vpnIp) {
		return nil, false
	}

	return f.handshakeManager.GetOrHandshake(vpnIp, cacheCallback)
}

func hashPacket(p *firewall.Packet) int {
	hasher := xxh3.Hasher{}

	hasher.Write(p.LocalIP.AsSlice())
	hasher.Write(p.RemoteIP.AsSlice())
	hasher.Write([]byte{
		byte(p.LocalPort & 0xFF),
		byte((p.LocalPort >> 8) & 0xFF),
		byte(p.RemotePort & 0xFF),
		byte((p.RemotePort >> 8) & 0xFF),
		byte(p.Protocol),
	})

	// Use xxh3 as it is a fast hash with good distribution
	return int(hasher.Sum64() & 0x7FFFFFFF)
}

func balancePacket(fwPacket *firewall.Packet, gateways util.EE_NewRouteType) netip.Addr {
	hash := hashPacket(fwPacket)

	selectedGateway := netip.Addr{}

	for i := range gateways {
		if hash <= gateways[i].UpperBound() {
			selectedGateway = gateways[i].Ip()
			break
		}
	}

	// This should never happen
	if !selectedGateway.IsValid() {
		panic("The packet hash value should always fall inside a gateway bucket")
	}

	return selectedGateway
}

// This is called for external traffic
// getOrHandshake returns nil if the vpnIp is not routable.
// If the 2nd return var is false then the hostinfo is not ready to be used in a tunnel
func (f *Interface) getOrHandshakeConsiderRouting(fwPacket *firewall.Packet, cacheCallback func(*HandshakeHostInfo)) (*HostInfo, bool) {

	destinationIp := fwPacket.RemoteIP

	// Host is inside the mesh, no routing
	if f.myVpnNet.Contains(destinationIp) {
		return f.handshakeManager.GetOrHandshake(destinationIp, cacheCallback)
	}

	gateways := f.inside.RoutesFor(destinationIp)
	if len(gateways) == 0 {
		return nil, false
	} else if len(gateways) == 1 {
		// Single gateway route
		return f.handshakeManager.GetOrHandshake(gateways[0].Ip(), cacheCallback)
	} else {
		// Multi gateway route, perform ECMP categorization
		gatewayIp := balancePacket(fwPacket, gateways)
		if hostInfo, ok := f.handshakeManager.GetOrHandshake(gatewayIp, cacheCallback); ok {
			return hostInfo, ok
		}

		// It appears the selected gateway cannot be reached, attempt the others as a fallback
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("destination", destinationIp).
				WithField("originalGateway", gatewayIp).
				Debugln("Calculated gateway for ECMP not available, attempting other gateways")
		}

		var hostInfo *HostInfo
		var ok bool

		for i := range gateways {
			// Skip the gateway that failed previously
			if gateways[i].Ip() == gatewayIp {
				continue
			}
			// Find another gateway to fallback on (this breaks ECMP but that seems better than no connectivity)
			if hostInfo, ok = f.handshakeManager.GetOrHandshake(gateways[i].Ip(), cacheCallback); ok {
				break
			}
		}

		return hostInfo, ok
	}
}

func (f *Interface) sendMessageNow(t header.MessageType, st header.MessageSubType, hostinfo *HostInfo, p, nb, out []byte) {
	fp := &firewall.Packet{}
	err := newPacket(p, false, fp)
	if err != nil {
		f.l.Warnf("error while parsing outgoing packet for firewall check; %v", err)
		return
	}

	// check if packet is in outbound fw rules
	dropReason := f.firewall.Drop(*fp, false, hostinfo, f.pki.GetCAPool(), nil)
	if dropReason != nil {
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("fwPacket", fp).
				WithField("reason", dropReason).
				Debugln("dropping cached packet")
		}
		return
	}

	f.sendNoMetrics(header.Message, st, hostinfo.ConnectionState, hostinfo, netip.AddrPort{}, p, nb, out, 0)
}

// SendMessageToVpnIp handles real ip:port lookup and sends to the current best known address for vpnIp
func (f *Interface) SendMessageToVpnIp(t header.MessageType, st header.MessageSubType, vpnIp netip.Addr, p, nb, out []byte) {
	hostInfo, ready := f.getOrHandshakeNoRouting(vpnIp, func(hh *HandshakeHostInfo) {
		hh.cachePacket(f.l, t, st, p, f.SendMessageToHostInfo, f.cachedPacketMetrics)
	})

	if hostInfo == nil {
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("vpnIp", vpnIp).
				Debugln("dropping SendMessageToVpnIp, vpnIp not in our CIDR or in unsafe routes")
		}
		return
	}

	if !ready {
		return
	}

	f.SendMessageToHostInfo(t, st, hostInfo, p, nb, out)
}

func (f *Interface) SendMessageToHostInfo(t header.MessageType, st header.MessageSubType, hi *HostInfo, p, nb, out []byte) {
	f.send(t, st, hi.ConnectionState, hi, p, nb, out)
}

func (f *Interface) send(t header.MessageType, st header.MessageSubType, ci *ConnectionState, hostinfo *HostInfo, p, nb, out []byte) {
	f.messageMetrics.Tx(t, st, 1)
	f.sendNoMetrics(t, st, ci, hostinfo, netip.AddrPort{}, p, nb, out, 0)
}

func (f *Interface) sendTo(t header.MessageType, st header.MessageSubType, ci *ConnectionState, hostinfo *HostInfo, remote netip.AddrPort, p, nb, out []byte) {
	f.messageMetrics.Tx(t, st, 1)
	f.sendNoMetrics(t, st, ci, hostinfo, remote, p, nb, out, 0)
}

// SendVia sends a payload through a Relay tunnel. No authentication or encryption is done
// to the payload for the ultimate target host, making this a useful method for sending
// handshake messages to peers through relay tunnels.
// via is the HostInfo through which the message is relayed.
// ad is the plaintext data to authenticate, but not encrypt
// nb is a buffer used to store the nonce value, re-used for performance reasons.
// out is a buffer used to store the result of the Encrypt operation
// q indicates which writer to use to send the packet.
func (f *Interface) SendVia(via *HostInfo,
	relay *Relay,
	ad,
	nb,
	out []byte,
	nocopy bool,
) {
	if noiseutil.EncryptLockNeeded {
		// NOTE: for goboring AESGCMTLS we need to lock because of the nonce check
		via.ConnectionState.writeLock.Lock()
	}
	c := via.ConnectionState.messageCounter.Add(1)

	out = header.Encode(out, header.Version, header.Message, header.MessageRelay, relay.RemoteIndex, c)
	f.connectionManager.Out(via.localIndexId)

	// Authenticate the header and payload, but do not encrypt for this message type.
	// The payload consists of the inner, unencrypted Nebula header, as well as the end-to-end encrypted payload.
	if len(out)+len(ad)+via.ConnectionState.eKey.Overhead() > cap(out) {
		if noiseutil.EncryptLockNeeded {
			via.ConnectionState.writeLock.Unlock()
		}
		via.logger(f.l).
			WithField("outCap", cap(out)).
			WithField("payloadLen", len(ad)).
			WithField("headerLen", len(out)).
			WithField("cipherOverhead", via.ConnectionState.eKey.Overhead()).
			Error("SendVia out buffer not large enough for relay")
		return
	}

	// The header bytes are written to the 'out' slice; Grow the slice to hold the header and associated data payload.
	offset := len(out)
	out = out[:offset+len(ad)]

	// In one call path, the associated data _is_ already stored in out. In other call paths, the associated data must
	// be copied into 'out'.
	if !nocopy {
		copy(out[offset:], ad)
	}

	var err error
	out, err = via.ConnectionState.eKey.EncryptDanger(out, out, nil, c, nb)
	if noiseutil.EncryptLockNeeded {
		via.ConnectionState.writeLock.Unlock()
	}
	if err != nil {
		via.logger(f.l).WithError(err).Info("Failed to EncryptDanger in sendVia")
		return
	}
	err = f.writers[0].WriteTo(out, via.remote)
	if err != nil {
		via.logger(f.l).WithError(err).Info("Failed to WriteTo in sendVia")
	}
	f.connectionManager.RelayUsed(relay.LocalIndex)
}

func (f *Interface) sendNoMetrics(t header.MessageType, st header.MessageSubType, ci *ConnectionState, hostinfo *HostInfo, remote netip.AddrPort, p, nb, out []byte, q int) {
	if ci.eKey == nil {
		//TODO: log warning
		return
	}
	useRelay := !remote.IsValid() && !hostinfo.remote.IsValid()
	fullOut := out

	if useRelay {
		if len(out) < header.Len {
			// out always has a capacity of mtu, but not always a length greater than the header.Len.
			// Grow it to make sure the next operation works.
			out = out[:header.Len]
		}
		// Save a header's worth of data at the front of the 'out' buffer.
		out = out[header.Len:]
	}

	if noiseutil.EncryptLockNeeded {
		// NOTE: for goboring AESGCMTLS we need to lock because of the nonce check
		ci.writeLock.Lock()
	}
	c := ci.messageCounter.Add(1)

	//l.WithField("trace", string(debug.Stack())).Error("out Header ", &Header{Version, t, st, 0, hostinfo.remoteIndexId, c}, p)
	out = header.Encode(out, header.Version, t, st, hostinfo.remoteIndexId, c)
	f.connectionManager.Out(hostinfo.localIndexId)

	// Query our LH if we haven't since the last time we've been rebound, this will cause the remote to punch against
	// all our IPs and enable a faster roaming.
	if t != header.CloseTunnel && hostinfo.lastRebindCount != f.rebindCount {
		//NOTE: there is an update hole if a tunnel isn't used and exactly 256 rebinds occur before the tunnel is
		// finally used again. This tunnel would eventually be torn down and recreated if this action didn't help.
		f.lightHouse.QueryServer(hostinfo.vpnIp)
		hostinfo.lastRebindCount = f.rebindCount
		if f.l.Level >= logrus.DebugLevel {
			f.l.WithField("vpnIp", hostinfo.vpnIp).Debug("Lighthouse update triggered for punch due to rebind counter")
		}
	}

	var err error
	out, err = ci.eKey.EncryptDanger(out, out, p, c, nb)
	if noiseutil.EncryptLockNeeded {
		ci.writeLock.Unlock()
	}
	if err != nil {
		hostinfo.logger(f.l).WithError(err).
			WithField("udpAddr", remote).WithField("counter", c).
			WithField("attemptedCounter", c).
			Error("Failed to encrypt outgoing packet")
		return
	}

	if remote.IsValid() {
		err = f.writers[q].WriteTo(out, remote)
		if err != nil {
			hostinfo.logger(f.l).WithError(err).
				WithField("udpAddr", remote).Error("Failed to write outgoing packet")
		}
	} else if hostinfo.remote.IsValid() {
		err = f.writers[q].WriteTo(out, hostinfo.remote)
		if err != nil {
			hostinfo.logger(f.l).WithError(err).
				WithField("udpAddr", remote).Error("Failed to write outgoing packet")
		}
	} else {
		// Try to send via a relay
		for _, relayIP := range hostinfo.relayState.CopyRelayIps() {
			relayHostInfo, relay, err := f.hostMap.QueryVpnIpRelayFor(hostinfo.vpnIp, relayIP)
			if err != nil {
				hostinfo.relayState.DeleteRelay(relayIP)
				hostinfo.logger(f.l).WithField("relay", relayIP).WithError(err).Info("sendNoMetrics failed to find HostInfo")
				continue
			}
			f.SendVia(relayHostInfo, relay, out, nb, fullOut[:header.Len+len(out)], true)
			break
		}
	}
}

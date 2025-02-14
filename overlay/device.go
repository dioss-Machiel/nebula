package overlay

import (
	"io"
	"net/netip"

	"github.com/slackhq/nebula/util"
)

type Device interface {
	io.ReadWriteCloser
	Activate() error
	Cidr() netip.Prefix
	Name() string
	RoutesFor(netip.Addr) util.EE_NewRouteType
	NewMultiQueueReader() (io.ReadWriteCloser, error)
}

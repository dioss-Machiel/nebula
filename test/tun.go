package test

import (
	"errors"
	"io"
	"net/netip"

	"github.com/slackhq/nebula/util"
)

type NoopTun struct{}

func (NoopTun) RoutesFor(addr netip.Addr) util.EE_NewRouteType {
	return util.EE_NewRouteType{}
}

func (NoopTun) Activate() error {
	return nil
}

func (NoopTun) Cidr() netip.Prefix {
	return netip.Prefix{}
}

func (NoopTun) Name() string {
	return "noop"
}

func (NoopTun) Read([]byte) (int, error) {
	return 0, nil
}

func (NoopTun) Write([]byte) (int, error) {
	return 0, nil
}

func (NoopTun) NewMultiQueueReader() (io.ReadWriteCloser, error) {
	return nil, errors.New("unsupported")
}

func (NoopTun) Close() error {
	return nil
}

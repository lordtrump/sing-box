package inbound

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/udpnat"
)

var _ adapter.Inbound = (*Direct)(nil)

type Direct struct {
	myInboundAdapter
	tlsConfig           tls.ServerConfig
	udpNat              *udpnat.Service[netip.AddrPort]
	overrideOption      int
	overrideDestination M.Socksaddr
}

func NewDirect(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.DirectInboundOptions) (*Direct, error) {
	options.UDPFragmentDefault = true
	inbound := &Direct{
		myInboundAdapter: myInboundAdapter{
			protocol:      C.TypeDirect,
			network:       options.Network.Build(),
			ctx:           ctx,
			router:        router,
			logger:        logger,
			tag:           tag,
			listenOptions: options.ListenOptions,
		},
	}
	if options.OverrideAddress != "" && options.OverridePort != 0 {
		inbound.overrideOption = 1
		inbound.overrideDestination = M.ParseSocksaddrHostPort(options.OverrideAddress, options.OverridePort)
	} else if options.OverrideAddress != "" {
		inbound.overrideOption = 2
		inbound.overrideDestination = M.ParseSocksaddrHostPort(options.OverrideAddress, options.OverridePort)
	} else if options.OverridePort != 0 {
		inbound.overrideOption = 3
		inbound.overrideDestination = M.Socksaddr{Port: options.OverridePort}
	}
	if options.TLS != nil {
		tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
		if err != nil {
			return nil, err
		}
		inbound.tlsConfig = tlsConfig
	}
	var udpTimeout time.Duration
	if options.UDPTimeout != 0 {
		udpTimeout = time.Duration(options.UDPTimeout)
	} else {
		udpTimeout = C.UDPTimeout
	}
	inbound.udpNat = udpnat.New[netip.AddrPort](int64(udpTimeout.Seconds()), adapter.NewUpstreamContextHandler(inbound.newConnection, inbound.newPacketConnection, inbound))
	inbound.connHandler = inbound
	inbound.packetHandler = inbound
	inbound.packetUpstream = inbound.udpNat
	return inbound, nil
}

func (d *Direct) Start() error {
	if d.tlsConfig != nil {
		err := d.tlsConfig.Start()
		if err != nil {
			return E.Cause(err, "create TLS config")
		}
	}
	return d.myInboundAdapter.Start()
}

func (d *Direct) Close() error {
	return common.Close(
		&d.myInboundAdapter,
		d.tlsConfig,
	)
}

func (d *Direct) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	var err error
	if d.tlsConfig != nil {
		conn, err = tls.ServerHandshake(ctx, conn, d.tlsConfig)
		if err != nil {
			return err
		}
	}
	switch d.overrideOption {
	case 1:
		metadata.Destination = d.overrideDestination
	case 2:
		destination := d.overrideDestination
		destination.Port = metadata.Destination.Port
		metadata.Destination = destination
	case 3:
		metadata.Destination.Port = d.overrideDestination.Port
	}
	d.logger.InfoContext(ctx, "inbound connection to ", metadata.Destination)
	return d.router.RouteConnection(ctx, conn, metadata)
}

func (d *Direct) NewPacket(ctx context.Context, conn N.PacketConn, buffer *buf.Buffer, metadata adapter.InboundContext) error {
	switch d.overrideOption {
	case 1:
		metadata.Destination = d.overrideDestination
	case 2:
		destination := d.overrideDestination
		destination.Port = metadata.Destination.Port
		metadata.Destination = destination
	case 3:
		metadata.Destination.Port = d.overrideDestination.Port
	}
	d.udpNat.NewContextPacket(ctx, metadata.Source.AddrPort(), buffer, adapter.UpstreamMetadata(metadata), func(natConn N.PacketConn) (context.Context, N.PacketWriter) {
		return adapter.WithContext(log.ContextWithNewID(ctx), &metadata), &udpnat.DirectBackWriter{Source: conn, Nat: natConn}
	})
	return nil
}

func (d *Direct) newConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return d.router.RouteConnection(ctx, conn, metadata)
}

func (d *Direct) newPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	ctx = log.ContextWithNewID(ctx)
	d.logger.InfoContext(ctx, "inbound packet connection from ", metadata.Source)
	return d.router.RoutePacketConnection(ctx, conn, metadata)
}

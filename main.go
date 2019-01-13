package main

//go:generate errorgen

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/golang/protobuf/proto"

	"v2ray.com/core"

	"v2ray.com/core/app/dispatcher"
	"v2ray.com/core/app/proxyman"
	_ "v2ray.com/core/app/proxyman/inbound"
	_ "v2ray.com/core/app/proxyman/outbound"

	"v2ray.com/core/common/net"
	"v2ray.com/core/common/protocol"
	"v2ray.com/core/common/serial"

	"v2ray.com/core/proxy/dokodemo"
	"v2ray.com/core/proxy/freedom"

	"v2ray.com/core/transport/internet"
	"v2ray.com/core/transport/internet/quic"
	"v2ray.com/core/transport/internet/tls"
	"v2ray.com/core/transport/internet/websocket"

	"v2ray.com/ext/sysio"
	"v2ray.com/ext/tools/conf"
)

var (
	vpn        = flag.Bool("V", false, "Run in VPN mode.")
	fastOpen   = flag.Bool("fast-open", false, "Enable TCP fast open.")
	localAddr  = flag.String("localAddr", "127.0.0.1", "local address to listen on.")
	localPort  = flag.String("localPort", "1984", "local port to listen on.")
	remoteAddr = flag.String("remoteAddr", "127.0.0.1", "remote address to forward.")
	remotePort = flag.String("remotePort", "1080", "remote port to forward.")
	path       = flag.String("path", "/", "URL path for websocket.")
	host       = flag.String("host", "cloudfront.com", "Host header for websocket.")
	tlsEnabled = flag.Bool("tls", false, "Enable TLS.")
	cert       = flag.String("cert", "", "Path to TLS certificate file. Overrides certRaw.")
	certRaw    = flag.String("certRaw", "", "Raw TLS certificate content. Intended only for Android.")
	key        = flag.String("key", "", "(server) Path to TLS key file.")
	mode       = flag.String("mode", "websocket", "Transport mode: websocket, quic (enforced tls).")
	server     = flag.Bool("server", false, "Run in server mode")
	logLevel   = flag.String("loglevel", "", "loglevel for v2ray: debug, info, warning (default), error, none.")
)

func readCertificate() ([]byte, error) {
	if *cert != "" {
		return sysio.ReadFile(*cert)
	}
	if *certRaw != "" {
		return []byte(*certRaw), nil
	}
	return nil, newError("missing")
}

func generateConfig() (*core.Config, error) {
	lport, err := net.PortFromString(*localPort)
	if err != nil {
		return nil, newError("invalid localPort:", *localPort).Base(err)
	}
	rport, err := strconv.ParseUint(*remotePort, 10, 32)
	if err != nil {
		return nil, newError("invalid remotePort:", *remotePort).Base(err)
	}
	outboundProxy := serial.ToTypedMessage(&freedom.Config{
		DestinationOverride: &freedom.DestinationOverride{
			Server: &protocol.ServerEndpoint{
				Address: net.NewIPOrDomain(net.ParseAddress(*remoteAddr)),
				Port: uint32(rport),
			},
		},
	})

	var transportSettings proto.Message
	switch *mode {
	case "websocket":
		transportSettings = &websocket.Config{
			Path: *path,
			Header: []*websocket.Header{
				{Key: "Host", Value: *host},
			},
		}
	case "quic":
		transportSettings = &quic.Config{
			Security: &protocol.SecurityConfig{Type: protocol.SecurityType_NONE},
		}
		*tlsEnabled = true
	default:
		return nil, newError("unsupported mode:", *mode)
	}

	streamConfig := internet.StreamConfig{
		ProtocolName: *mode,
		TransportSettings: []*internet.TransportConfig{{
			ProtocolName: *mode,
			Settings: serial.ToTypedMessage(transportSettings),
		}},
	}
	if *fastOpen {
		streamConfig.SocketSettings = &internet.SocketConfig{Tfo: internet.SocketConfig_Enable}
	}
	if *tlsEnabled {
		tlsConfig := tls.Config{ServerName: *host}
		if *server {
			if *key == "" {
				return nil, newError("no certificates configured")
			}
			certificate := tls.Certificate{}
			certificate.Certificate, err = readCertificate()
			if err != nil {
				return nil, newError("failed to read cert").Base(err)
			}
			certificate.Key, err = sysio.ReadFile(*key)
			if err != nil {
				return nil, newError("failed to read key file").Base(err)
			}
			tlsConfig.Certificate = []*tls.Certificate{&certificate}
		} else if *cert != "" || *certRaw != "" {
			certificate := tls.Certificate{Usage: tls.Certificate_AUTHORITY_VERIFY}
			certificate.Certificate, err = readCertificate()
			if err != nil {
				return nil, newError("failed to read cert").Base(err)
			}
			tlsConfig.Certificate = []*tls.Certificate{&certificate}
		}
		streamConfig.SecurityType = serial.GetMessageType(&tlsConfig)
		streamConfig.SecuritySettings = []*serial.TypedMessage{serial.ToTypedMessage(&tlsConfig)}
	}

	apps := []*serial.TypedMessage{
		serial.ToTypedMessage(&dispatcher.Config{}),
		serial.ToTypedMessage(&proxyman.InboundConfig{}),
		serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		serial.ToTypedMessage((&conf.LogConfig{LogLevel: *logLevel}).Build()),
	}
	if *server {
		return &core.Config{
			Inbound: []*core.InboundHandlerConfig{{
				ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
					PortRange: net.SinglePortRange(lport),
					Listen:	net.NewIPOrDomain(net.ParseAddress(*localAddr)),
					StreamSettings: &streamConfig,
				}),
				ProxySettings: serial.ToTypedMessage(&dokodemo.Config{
					// This address is required when mux is used on client.
					// dokodemo is not aware of mux connections by itself.
					// Change this value to net.LocalHostIP if mux is disabled.
					Address: net.NewIPOrDomain(net.ParseAddress("v1.mux.cool")),
					Networks: []net.Network{net.Network_TCP},
				}),
			}},
			Outbound: []*core.OutboundHandlerConfig{{
				ProxySettings: outboundProxy,
			}},
			App: apps,
		}, nil
	} else {
		return &core.Config{
			Inbound: []*core.InboundHandlerConfig{{
				ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
					PortRange: net.SinglePortRange(lport),
					Listen:	net.NewIPOrDomain(net.ParseAddress(*localAddr)),
				}),
				ProxySettings: serial.ToTypedMessage(&dokodemo.Config{
					Address: net.NewIPOrDomain(net.LocalHostIP),
					Networks: []net.Network{net.Network_TCP},
				}),
			}},
			Outbound: []*core.OutboundHandlerConfig{{
				SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{
					StreamSettings: &streamConfig,
					MultiplexSettings: &proxyman.MultiplexingConfig{
						Enabled: true,
						Concurrency: 8,
					},
				}),
				ProxySettings: outboundProxy,
			}},
			App: apps,
		}, nil
	}
}

func startV2Ray() (core.Server, error) {

	if *vpn {
		registerControlFunc()
	}

	opts, err := parseEnv()

	if err == nil {
		if c, b := opts.Get("mode"); b {
			*mode = c
		}
		if _, b := opts.Get("tls"); b {
			*tlsEnabled = true
		}
		if c, b := opts.Get("host"); b {
			*host = c
		}
		if c, b := opts.Get("path"); b {
			*path = c
		}
		if _, b := opts.Get("server"); b {
			*server = true
		}
		if c, b := opts.Get("localAddr"); b {
			if *server {
				*remoteAddr = c
			} else {
				*localAddr = c
			}
		}
		if c, b := opts.Get("localPort"); b {
			if *server {
				*remotePort = c
			} else {
				*localPort = c
			}
		}
		if c, b := opts.Get("remoteAddr"); b {
			if *server {
				*localAddr = c
			} else {
				*remoteAddr = c
			}
		}
		if c, b := opts.Get("remotePort"); b {
			if *server {
				*localPort = c
			} else {
				*remotePort = c
			}
		}
	}

	config, err := generateConfig()
	if err != nil {
		return nil, newError("failed to parse config").Base(err)
	}
	instance, err := core.New(config)
	if err != nil {
		return nil, newError("failed to create v2ray instance").Base(err)
	}
	return instance, nil
}

func printVersion() {
	version := core.VersionStatement()
	for _, s := range version {
		log.Println(s)
	}
}

func main() {
	flag.Parse()

	logInit()

	printVersion()

	server, err := startV2Ray()
	if err != nil {
		log.Println(err.Error())
		// Configuration error. Exit with a special value to prevent systemd from restarting.
		os.Exit(23)
	}
	if err := server.Start(); err != nil {
		log.Fatalln("failed to start server:", err.Error())
	}

	defer server.Close()

	{
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, os.Interrupt, os.Kill, syscall.SIGTERM)
		<-osSignals
	}
}

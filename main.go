package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/wlynxg/anet"
	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

var (
	globalDocUrl string
	maxToken     string
	maxUid       string
)

func main() {
	//os.Setenv("GODEBUG", "netdns=go")
	fmt.Print("written by p1neappleXpress\n")

	exitNode := flag.Bool("exit-node", false, "Run as exit node (needs root)")
	client := flag.Bool("client", false, "Run as client")
	debug := flag.Bool("debug", false, "Enable verbose debug logging")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, vyandex-v6, oneme)")
	yandexQueueSize := flag.Int("yandex-queue-size", 1024, "Yandex transport write queue size")
	compressionEnabled := flag.Bool("compression", true, "Enable transport LZ4 wrapper")
	tcpBufferDefault := flag.Int("tcp-buffer-default", 262144, "gVisor TCP default send/receive buffer bytes")
	tcpBufferMax := flag.Int("tcp-buffer-max", 1048576, "gVisor TCP max send/receive buffer bytes")
	batchPackets := flag.Int("batch-packets", 1, "IP packets per Yandex transport batch; 1 disables batching")
	batchDelayUs := flag.Int("batch-delay-us", 1000, "Maximum batch flush delay in microseconds")
	volgaURLSecondary := flag.String("volga-url-secondary", "", "Secondary Yandex document URL for experimental vyandex-v6 carrier recycling")
	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL. If u use Yandex.Docs transport")
	flag.StringVar(&maxToken, "maxToken", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX Web token. If u use MAX transport")
	flag.Parse()

	if !*exitNode && !*client {
		flag.Usage()
		os.Exit(1)
	}

	if *debug {
		utils.EnableDebug()
	}

	log.Printf("=== Universal Bypass Tool ===")
	log.Printf("Mode: %s", map[bool]string{true: "EXIT NODE", false: "CLIENT"}[*exitNode])
	log.Printf("Transport: %s", *transportType)

	config := transport.DefaultConfig()
	if *yandexQueueSize < 1 {
		log.Fatalf("Invalid --yandex-queue-size: %d", *yandexQueueSize)
	}
	config.MaxQueueSize = *yandexQueueSize
	if *tcpBufferDefault < 65536 {
		log.Fatalf("Invalid --tcp-buffer-default: %d", *tcpBufferDefault)
	}
	if *tcpBufferMax < *tcpBufferDefault {
		log.Fatalf("Invalid TCP buffers: max %d < default %d", *tcpBufferMax, *tcpBufferDefault)
	}
	if *batchPackets < 1 || *batchPackets > 64 {
		log.Fatalf("Invalid --batch-packets: %d", *batchPackets)
	}
	if *batchDelayUs < 50 || *batchDelayUs > 100000 {
		log.Fatalf("Invalid --batch-delay-us: %d", *batchDelayUs)
	}
	var trans transport.Transport

	switch *transportType {
	case "vyandex":
		volgaTransport := yandex.NewYandexVolgaTransport(globalDocUrl, config)
		if *compressionEnabled {
			trans = transport.NewCompressedTransport(volgaTransport)
		} else {
			trans = volgaTransport
		}
		log.Printf("Volga transport: upstream defaults (internal batch=20 timeout=2ms)")
		log.Printf("Compression: %t", *compressionEnabled)
	case "vyandex-v6":
		documents := []string{globalDocUrl}
		if secondary := strings.TrimSpace(*volgaURLSecondary); secondary != "" {
			documents = append(documents, secondary)
		}
		volgaV6Transport := yandex.NewYandexVolgaV6Transport(documents, config)
		if *compressionEnabled {
			trans = transport.NewCompressedTransport(volgaV6Transport)
		} else {
			trans = volgaV6Transport
		}
		log.Printf("Volga V6 experimental: docs=%d logical-reliability=shared physical-carriers=recyclable", len(documents))
		log.Printf("Volga V6 defaults: batch=20/5000B/2ms send-workers=32 progress-based-recycle=true")
		log.Printf("Compression: %t", *compressionEnabled)
	case "yandex":
		yandexTransport := yandex.NewYandexDocsTransport(globalDocUrl, config)
		if *compressionEnabled {
			trans = transport.NewCompressedTransport(yandexTransport)
		} else {
			trans = yandexTransport
		}
		log.Printf("Yandex queue size: %d", config.MaxQueueSize)
		log.Printf("Compression: %t", *compressionEnabled)
	case "oneme":
		uidint, _ := strconv.ParseInt(maxUid, 10, 64)
		trans = transport.NewCompressedTransport(oneme.NewOneMeTransport(*exitNode, maxToken, uidint, config))
	default:
		log.Fatalf("Unknown transport type: %s", *transportType)
	}

	// Keep the proven private 4-packet/1ms batching wrapper scoped to the legacy
	// Yandex transport only. Both Volga implementations own their batching.
	if *transportType == "yandex" && *batchPackets > 1 {
		trans = transport.NewBatchingTransport(
			trans,
			*batchPackets,
			32768,
			time.Duration(*batchDelayUs)*time.Microsecond,
		)
		log.Printf("Batching: packets=%d max_bytes=32768 delay=%dus", *batchPackets, *batchDelayUs)
	} else if *transportType == "vyandex" || *transportType == "vyandex-v6" {
		log.Printf("External batching: disabled (Volga uses internal batching)")
	} else {
		log.Printf("Batching: disabled")
	}

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	log.Printf("gVisor TCP buffers: default=%d max=%d", *tcpBufferDefault, *tcpBufferMax)
	tun := tunnel.NewTCPTunnel(trans, *exitNode, *tcpBufferDefault, *tcpBufferMax)

	if *exitNode {
		log.Printf("Running as EXIT NODE (needs root for raw socket)")
		log.Printf("! Run: sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP")
		select {}
	} else {
		log.Printf("Running as CLIENT (SOCKS5 on %s)", *socksAddr)
		socks5Server := socks5.NewSOCKS5Server(*socksAddr, tun)
		log.Fatal(socks5Server.Start())
	}
}

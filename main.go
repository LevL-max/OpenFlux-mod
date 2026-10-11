package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/wlynxg/anet"
	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/mailru"
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
	tracePackets := flag.Bool("trace-packets", false, "With --debug, also log every tunnel packet")
	statusEvents := flag.Bool("status-events", true, "Emit safe authentication and connection status events")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, mailru, oneme)")
	mailruEditorActivity := flag.Bool("mailru-editor-activity", false, "Mail.ru: send fake saveChanges edits like a live editor (changes the document)")
	yandexQueueSize := flag.Int("yandex-queue-size", 1024, "Yandex transport write queue size")
	yandexSafeMode := flag.Bool("yandex-safe-mode", false, "Fragment Legacy cursor payloads below 2600 bytes and pace all writes at 10/s (enable on both peers)")
	yandexLanes := flag.Int("yandex-lanes", 1, "Legacy participants: 1..32 (multiple participants require safe mode on both peers)")
	yandexCookieFile := flag.String("yandex-cookie-file", "", "Optional one-time raw browser Cookie header seed")
	yandexCookieStore := flag.String("yandex-cookie-store", "", "Persistent Yandex cookie store JSON path")
	yandexUserAgent := flag.String("yandex-user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0", "User-Agent used for Yandex HTTP/WebSocket bootstrap")
	compressionEnabled := flag.Bool("compression", true, "Enable transport LZ4 wrapper")
	tcpBufferDefault := flag.Int("tcp-buffer-default", 262144, "gVisor TCP default send/receive buffer bytes")
	tcpBufferMax := flag.Int("tcp-buffer-max", 1048576, "gVisor TCP max send/receive buffer bytes")
	batchPackets := flag.Int("batch-packets", 1, "IP packets per Yandex transport batch; 1 disables batching")
	batchDelayUs := flag.Int("batch-delay-us", 1000, "Maximum batch flush delay in microseconds")
	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL. If u use Yandex.Docs transport")
	flag.StringVar(&maxToken, "maxToken", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX Web token. If u use MAX transport")
	flag.Parse()
	utils.EnableStatusEvents(*statusEvents)

	if !*exitNode && !*client {
		flag.Usage()
		os.Exit(1)
	}

	if *debug {
		utils.EnableDebug()
	}
	if *tracePackets {
		utils.EnablePacketTrace()
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
	if *yandexLanes < 1 || *yandexLanes > 32 || (*yandexLanes > 1 && (*transportType != "yandex" || !*yandexSafeMode)) {
		log.Fatal("--yandex-lanes must be 1, or 2..32 with --transport=yandex --yandex-safe-mode=true")
	}

	switch *transportType {
	case "yandex":
		yandexTransport := yandex.NewYandexDocsTransport(globalDocUrl, config)
		var cursorTransport transport.Transport = yandexTransport
		if *yandexCookieStore != "" {
			if err := yandexTransport.SetCookieStore(*yandexCookieStore); err != nil {
				log.Fatalf("Failed to configure Yandex cookie store: %v", err)
			}
			log.Printf("Yandex persistent cookie store: %s", *yandexCookieStore)
		}
		if *yandexCookieFile != "" {
			if err := yandexTransport.LoadBrowserCookies(*yandexCookieFile, *yandexUserAgent); err != nil {
				log.Fatalf("Failed to import Yandex browser cookies: %v", err)
			}
			log.Printf("Yandex browser cookie seed imported")
		}
		if *yandexSafeMode {
			*batchPackets = 1 // large aggregate fragments amplified loss in live cursor tests
			yandexTransport.SetCursorSafety(2600, 100*time.Millisecond)
			if *yandexLanes > 1 {
				participants := []transport.Transport{yandexTransport}
				for i := 1; i < *yandexLanes; i++ {
					participants = append(participants, yandexTransport.NewParticipant())
				}
				striped, err := transport.NewStripedTransport(participants)
				if err != nil {
					log.Fatal(err)
				}
				cursorTransport = striped
			}
			fragmented, err := transport.NewFragmentingTransport(cursorTransport, 1800)
			if err != nil {
				log.Fatal(err)
			}
			cursorTransport = fragmented
			log.Printf("Legacy safe mode: cursor limit 2600 bytes, at most 10 writes/s/participant, lanes=%d; both peers must match safe mode, lanes and batching", *yandexLanes)
		}
		if *compressionEnabled {
			trans = transport.NewCompressedTransport(cursorTransport)
		} else {
			trans = cursorTransport
		}
		log.Printf("Yandex queue size: %d", config.MaxQueueSize)
		log.Printf("Compression: %t", *compressionEnabled)
	case "mailru":
		mailruTransport := mailru.NewMailruDocsTransport(globalDocUrl, config)
		mailruTransport.SetEditorActivity(*mailruEditorActivity)
		if *compressionEnabled {
			trans = transport.NewCompressedTransport(mailruTransport)
		} else {
			trans = mailruTransport
		}
		log.Printf("Mail.ru queue size: %d | editor activity: %t", config.MaxQueueSize, *mailruEditorActivity)
		log.Printf("Compression: %t", *compressionEnabled)
	case "oneme":
		uidint, _ := strconv.ParseInt(maxUid, 10, 64)
		trans = transport.NewCompressedTransport(oneme.NewOneMeTransport(*exitNode, maxToken, uidint, config))
	default:
		log.Fatalf("Unknown transport type: %s", *transportType)
	}

	if (*transportType == "yandex" || *transportType == "mailru") && *batchPackets > 1 {
		trans = transport.NewBatchingTransport(
			trans,
			*batchPackets,
			32768,
			time.Duration(*batchDelayUs)*time.Microsecond,
		)
		log.Printf("Batching: packets=%d max_bytes=32768 delay=%dus", *batchPackets, *batchDelayUs)
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

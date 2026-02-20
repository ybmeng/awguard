package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"awguard/capture"
	"awguard/detect"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func main() {
	iface := flag.String("i", "", "network interface to monitor (default: auto-detect)")
	flag.StringVar(iface, "interface", "", "network interface to monitor (default: auto-detect)")
	verbose := flag.Bool("v", false, "log every inspected packet")
	flag.BoolVar(verbose, "verbose", false, "log every inspected packet")
	dryRun := flag.Bool("n", false, "detect and log but don't kill")
	flag.BoolVar(dryRun, "dry-run", false, "detect and log but don't kill")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: sudo awguard [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Passively monitors network traffic for plaintext BitTorrent signatures.\n")
		fmt.Fprintf(os.Stderr, "If detected, kills the anywatch process immediately.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fmt.Fprintf(os.Stderr, "  -i, --interface string   Network interface to monitor (default: auto-detect)\n")
		fmt.Fprintf(os.Stderr, "  -v, --verbose            Log every inspected packet\n")
		fmt.Fprintf(os.Stderr, "  -n, --dry-run            Detect and log but don't kill\n")
	}

	flag.Parse()

	if os.Geteuid() != 0 {
		log.Fatal("awguard requires root privileges for packet capture. Run with sudo.")
	}

	ifaceName := *iface
	if ifaceName == "" {
		var err error
		ifaceName, err = capture.DetectPrimaryInterface()
		if err != nil {
			log.Fatalf("Failed to auto-detect network interface: %v", err)
		}
	}

	log.Printf("awguard: monitoring interface %s", ifaceName)
	if *dryRun {
		log.Printf("awguard: dry-run mode — will detect but not kill")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cap, err := capture.NewCapture(ifaceName, capture.DefaultSnapLen)
	if err != nil {
		log.Fatalf("Failed to start capture: %v", err)
	}
	defer cap.Close()

	// Graceful shutdown on SIGINT/SIGTERM
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("awguard: shutting down")
		cancel()
		cap.Close()
	}()

	packets := cap.Packets(ctx)
	log.Printf("awguard: watching for plaintext BitTorrent traffic...")

	for pkt := range packets {
		d := inspect(pkt, *verbose)
		if d == nil {
			continue
		}
		log.Printf("awguard: *** BT TRAFFIC DETECTED *** %s", d)
		notify(d)
		if *dryRun {
			continue
		}
		killAnyWatch()
	}

	log.Printf("awguard: stopped")
}

func killAnyWatch() {
	out, err := exec.Command("pkill", "-9", "-f", "anywatch").CombinedOutput()
	if err != nil {
		log.Printf("awguard: pkill anywatch: %v (%s)", err, string(out))
	} else {
		log.Printf("awguard: killed anywatch")
	}
}

func notify(d *detect.Detection) {
	title := "AWGuard: BitTorrent Traffic Detected"
	msg := fmt.Sprintf("%s — %s:%d → %s:%d. AnyWatch killed.",
		d.Type, d.SrcIP, d.SrcPort, d.DstIP, d.DstPort)
	script := fmt.Sprintf(`display notification %q with title %q sound name "Basso"`, msg, title)
	_ = exec.Command("osascript", "-e", script).Run()
}

func inspect(pkt gopacket.Packet, verbose bool) *detect.Detection {
	netLayer := pkt.NetworkLayer()
	if netLayer == nil {
		return nil
	}

	var srcIP, dstIP net.IP
	if ipv4, ok := netLayer.(*layers.IPv4); ok {
		srcIP = ipv4.SrcIP
		dstIP = ipv4.DstIP
	} else if ipv6, ok := netLayer.(*layers.IPv6); ok {
		srcIP = ipv6.SrcIP
		dstIP = ipv6.DstIP
	} else {
		return nil
	}

	if tcpLayer := pkt.Layer(layers.LayerTypeTCP); tcpLayer != nil {
		tcp := tcpLayer.(*layers.TCP)
		payload := tcp.Payload
		if len(payload) == 0 {
			return nil
		}
		if verbose {
			log.Printf("awguard: TCP %s:%d → %s:%d (%d bytes payload)",
				srcIP, tcp.SrcPort, dstIP, tcp.DstPort, len(payload))
		}
		return detect.CheckTCPPayload(payload, srcIP, dstIP, uint16(tcp.SrcPort), uint16(tcp.DstPort))
	}

	if udpLayer := pkt.Layer(layers.LayerTypeUDP); udpLayer != nil {
		udp := udpLayer.(*layers.UDP)
		payload := udp.Payload
		if len(payload) == 0 {
			return nil
		}
		if verbose {
			log.Printf("awguard: UDP %s:%d → %s:%d (%d bytes payload)",
				srcIP, udp.SrcPort, dstIP, udp.DstPort, len(payload))
		}
		return detect.CheckUDPPayload(payload, srcIP, dstIP, uint16(udp.SrcPort), uint16(udp.DstPort))
	}

	return nil
}

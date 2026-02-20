package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"awguard/capture"
	"awguard/detect"
)

var (
	packetCount  atomic.Int64
	lastDetected atomic.Value // stores string
)

func main() {
	if os.Geteuid() != 0 {
		log.Fatal("awguard requires root privileges for packet capture. Run with sudo.")
	}
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetTemplateIcon(iconPNG, iconPNG)
	systray.SetTitle("")
	systray.SetTooltip("AWGuard — BitTorrent killswitch")

	mStatus := systray.AddMenuItem("Monitoring...", "")
	mStatus.Disable()
	mPackets := systray.AddMenuItem("Packets: 0", "")
	mPackets.Disable()
	systray.AddSeparator()
	mLastEvent := systray.AddMenuItem("No violations", "")
	mLastEvent.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit AWGuard", "")

	// Auto-detect interface
	ifaceName, err := capture.DetectPrimaryInterface()
	if err != nil {
		mStatus.SetTitle("Error: no interface found")
		log.Printf("awguard: failed to detect interface: %v", err)
		return
	}

	mStatus.SetTitle(fmt.Sprintf("Monitoring %s", ifaceName))
	log.Printf("awguard: monitoring interface %s", ifaceName)

	// Start capture
	ctx, cancel := context.WithCancel(context.Background())

	cap, err := capture.NewCapture(ifaceName, capture.DefaultSnapLen)
	if err != nil {
		mStatus.SetTitle(fmt.Sprintf("Error: %v", err))
		log.Printf("awguard: capture failed: %v", err)
		return
	}

	packets := cap.Packets(ctx)

	// Packet inspection goroutine
	go func() {
		for pkt := range packets {
			d := inspect(pkt)
			if d == nil {
				continue
			}
			log.Printf("awguard: *** BT TRAFFIC DETECTED *** %s", d)
			notify(d)
			killAnyWatch()

			ts := time.Now().Format("15:04:05")
			lastDetected.Store(fmt.Sprintf("VIOLATION %s: %s", ts, d.Type))
		}
	}()

	// UI update ticker
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mPackets.SetTitle(fmt.Sprintf("Packets: %d", packetCount.Load()))
			if v := lastDetected.Load(); v != nil {
				mLastEvent.SetTitle(v.(string))
			}
		}
	}()

	// Quit handler
	go func() {
		<-mQuit.ClickedCh
		cancel()
		cap.Close()
		systray.Quit()
	}()
}

func onExit() {}

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

func inspect(pkt gopacket.Packet) *detect.Detection {
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
		packetCount.Add(1)
		return detect.CheckTCPPayload(payload, srcIP, dstIP, uint16(tcp.SrcPort), uint16(tcp.DstPort))
	}

	if udpLayer := pkt.Layer(layers.LayerTypeUDP); udpLayer != nil {
		udp := udpLayer.(*layers.UDP)
		payload := udp.Payload
		if len(payload) == 0 {
			return nil
		}
		packetCount.Add(1)
		return detect.CheckUDPPayload(payload, srcIP, dstIP, uint16(udp.SrcPort), uint16(udp.DstPort))
	}

	return nil
}

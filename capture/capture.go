package capture

import (
	"context"
	"fmt"
	"net"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

const DefaultSnapLen int32 = 128

// Capture reads packets from a network interface via libpcap.
type Capture struct {
	handle *pcap.Handle
}

// NewCapture opens a live packet capture on the given interface.
// snapLen controls how many bytes of each packet to capture (128 is enough for headers + BT signature).
// Requires root/BPF privileges on macOS.
func NewCapture(iface string, snapLen int32) (*Capture, error) {
	handle, err := pcap.OpenLive(iface, snapLen, false, pcap.BlockForever)
	if err != nil {
		return nil, fmt.Errorf("pcap.OpenLive(%s): %w", iface, err)
	}
	// Only capture TCP and UDP — skip ARP, ICMP, etc.
	if err := handle.SetBPFFilter("tcp or udp"); err != nil {
		handle.Close()
		return nil, fmt.Errorf("SetBPFFilter: %w", err)
	}
	return &Capture{handle: handle}, nil
}

// Packets returns a channel of captured packets. The channel is closed when
// the context is cancelled or the capture handle is closed.
func (c *Capture) Packets(ctx context.Context) <-chan gopacket.Packet {
	src := gopacket.NewPacketSource(c.handle, c.handle.LinkType())
	src.NoCopy = true
	ch := make(chan gopacket.Packet, 256)
	go func() {
		defer close(ch)
		for {
			pkt, err := src.NextPacket()
			if err != nil {
				// Handle closed or context cancelled
				select {
				case <-ctx.Done():
					return
				default:
				}
				// pcap handle closed — normal shutdown
				return
			}
			select {
			case ch <- pkt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Close stops the packet capture.
func (c *Capture) Close() {
	c.handle.Close()
}

// DetectPrimaryInterface returns the first non-loopback, non-tunnel, up+running
// interface that has an IPv4 address. This is the interface most likely carrying
// internet traffic.
func DetectPrimaryInterface() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("net.Interfaces: %w", err)
	}
	for _, iface := range ifaces {
		// Skip loopback, down, or point-to-point (tunnel) interfaces
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagRunning == 0 {
			continue
		}
		if iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		// Check for IPv4 address
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				return iface.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no suitable network interface found")
}

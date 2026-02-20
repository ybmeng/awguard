package detect

import (
	"bytes"
	"fmt"
	"net"
)

// Type identifies the kind of BitTorrent traffic detected.
type Type int

const (
	BTHandshake     Type = iota // TCP: \x13BitTorrent protocol
	TrackerAnnounce             // TCP: GET /announce?info_hash=
	DHTMessage                  // UDP: bencoded DHT query/response
)

func (t Type) String() string {
	switch t {
	case BTHandshake:
		return "BT Handshake"
	case TrackerAnnounce:
		return "Tracker Announce"
	case DHTMessage:
		return "DHT Message"
	default:
		return "Unknown"
	}
}

// Detection represents a detected BitTorrent protocol signature.
type Detection struct {
	Type   Type
	Detail string
	SrcIP  net.IP
	DstIP  net.IP
	SrcPort uint16
	DstPort uint16
}

func (d *Detection) String() string {
	return fmt.Sprintf("[%s] %s:%d → %s:%d — %s",
		d.Type, d.SrcIP, d.SrcPort, d.DstIP, d.DstPort, d.Detail)
}

var (
	btHandshakePrefix = []byte("\x13BitTorrent protocol")
	announcePath      = []byte("/announce")
	infoHashParam     = []byte("info_hash=")
	httpGet           = []byte("GET ")
)

// CheckTCPPayload inspects a TCP payload for BitTorrent signatures.
// Returns nil if no BT traffic detected.
func CheckTCPPayload(payload []byte, srcIP, dstIP net.IP, srcPort, dstPort uint16) *Detection {
	if len(payload) == 0 {
		return nil
	}

	// BT Handshake: first byte is 19 (\x13), followed by "BitTorrent protocol"
	if len(payload) >= 20 && bytes.Equal(payload[:20], btHandshakePrefix) {
		return &Detection{
			Type:    BTHandshake,
			Detail:  "plaintext BitTorrent handshake detected",
			SrcIP:   srcIP,
			DstIP:   dstIP,
			SrcPort: srcPort,
			DstPort: dstPort,
		}
	}

	// Tracker Announce: HTTP GET with /announce and info_hash=
	if len(payload) >= 4 && bytes.HasPrefix(payload, httpGet) {
		// Only inspect the first line (up to 2048 bytes to bound work)
		end := len(payload)
		if end > 2048 {
			end = 2048
		}
		line := payload[:end]
		if bytes.Contains(line, announcePath) && bytes.Contains(line, infoHashParam) {
			return &Detection{
				Type:    TrackerAnnounce,
				Detail:  "plaintext tracker announce request detected",
				SrcIP:   srcIP,
				DstIP:   dstIP,
				SrcPort: srcPort,
				DstPort: dstPort,
			}
		}
	}

	return nil
}

// DHT bencoded patterns:
// Queries contain "1:y1:q" (y=q means query)
// Responses contain "1:y1:r" (y=r means response)
// Errors contain "1:y1:e" (y=e means error)
var (
	dhtQuery    = []byte("1:y1:q")
	dhtResponse = []byte("1:y1:r")
	dhtError    = []byte("1:y1:e")
)

// CheckUDPPayload inspects a UDP payload for BitTorrent DHT signatures.
// Returns nil if no BT traffic detected.
func CheckUDPPayload(payload []byte, srcIP, dstIP net.IP, srcPort, dstPort uint16) *Detection {
	if len(payload) < 10 {
		return nil
	}

	// DHT messages are bencoded dictionaries starting with 'd'
	if payload[0] != 'd' {
		return nil
	}

	var detail string
	switch {
	case bytes.Contains(payload, dhtQuery):
		detail = "plaintext DHT query detected"
	case bytes.Contains(payload, dhtResponse):
		detail = "plaintext DHT response detected"
	case bytes.Contains(payload, dhtError):
		detail = "plaintext DHT error detected"
	default:
		return nil
	}

	return &Detection{
		Type:    DHTMessage,
		Detail:  detail,
		SrcIP:   srcIP,
		DstIP:   dstIP,
		SrcPort: srcPort,
		DstPort: dstPort,
	}
}

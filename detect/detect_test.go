package detect

import (
	"net"
	"testing"
)

var (
	srcIP = net.IPv4(192, 168, 1, 100)
	dstIP = net.IPv4(10, 0, 0, 1)
)

// --- TCP: BT Handshake ---

func TestBTHandshake(t *testing.T) {
	// Real BT handshake: \x13 + "BitTorrent protocol" + 8 reserved + 20 info_hash + 20 peer_id
	payload := make([]byte, 68)
	copy(payload, "\x13BitTorrent protocol")
	// rest is zeros (reserved bytes, info_hash, peer_id)

	d := CheckTCPPayload(payload, srcIP, dstIP, 51234, 6881)
	if d == nil {
		t.Fatal("expected detection for BT handshake")
	}
	if d.Type != BTHandshake {
		t.Errorf("expected BTHandshake, got %v", d.Type)
	}
	if d.SrcPort != 51234 || d.DstPort != 6881 {
		t.Errorf("wrong ports: %d → %d", d.SrcPort, d.DstPort)
	}
}

func TestBTHandshakeExactly20Bytes(t *testing.T) {
	payload := []byte("\x13BitTorrent protocol")
	d := CheckTCPPayload(payload, srcIP, dstIP, 1000, 2000)
	if d == nil {
		t.Fatal("expected detection for exact 20-byte handshake")
	}
	if d.Type != BTHandshake {
		t.Errorf("expected BTHandshake, got %v", d.Type)
	}
}

func TestBTHandshakeTruncated(t *testing.T) {
	payload := []byte("\x13BitTorrent proto") // 19 bytes, missing last char
	d := CheckTCPPayload(payload, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for truncated handshake, got %v", d)
	}
}

func TestBTHandshakeWrongPrefix(t *testing.T) {
	payload := []byte("\x14BitTorrent protocol") // wrong length byte
	d := CheckTCPPayload(payload, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for wrong prefix byte, got %v", d)
	}
}

// --- TCP: Tracker Announce ---

func TestTrackerAnnounce(t *testing.T) {
	payload := []byte("GET /announce?info_hash=%ab%cd%ef&peer_id=xyz HTTP/1.1\r\nHost: tracker.example.com\r\n\r\n")
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 80)
	if d == nil {
		t.Fatal("expected detection for tracker announce")
	}
	if d.Type != TrackerAnnounce {
		t.Errorf("expected TrackerAnnounce, got %v", d.Type)
	}
}

func TestTrackerAnnounceWithPath(t *testing.T) {
	payload := []byte("GET /some/path/announce?info_hash=%00%01%02&port=6881 HTTP/1.1\r\n\r\n")
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 80)
	if d == nil {
		t.Fatal("expected detection for announce with path prefix")
	}
	if d.Type != TrackerAnnounce {
		t.Errorf("expected TrackerAnnounce, got %v", d.Type)
	}
}

func TestTrackerAnnounceNoInfoHash(t *testing.T) {
	// Has /announce but no info_hash — not a BT tracker request
	payload := []byte("GET /announce?event=started HTTP/1.1\r\n\r\n")
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 80)
	if d != nil {
		t.Errorf("expected nil for announce without info_hash, got %v", d)
	}
}

func TestTrackerNoAnnounce(t *testing.T) {
	payload := []byte("GET /index.html?info_hash=abc HTTP/1.1\r\n\r\n")
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 80)
	if d != nil {
		t.Errorf("expected nil for non-announce GET with info_hash, got %v", d)
	}
}

// --- UDP: DHT ---

func TestDHTQuery(t *testing.T) {
	// Minimal bencoded DHT ping query: d1:ad2:id20:...e1:q4:ping1:t2:aa1:y1:qe
	payload := []byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe")
	d := CheckUDPPayload(payload, srcIP, dstIP, 6881, 6882)
	if d == nil {
		t.Fatal("expected detection for DHT query")
	}
	if d.Type != DHTMessage {
		t.Errorf("expected DHTMessage, got %v", d.Type)
	}
}

func TestDHTResponse(t *testing.T) {
	payload := []byte("d1:rd2:id20:abcdefghij0123456789e1:t2:aa1:y1:re")
	d := CheckUDPPayload(payload, srcIP, dstIP, 6882, 6881)
	if d == nil {
		t.Fatal("expected detection for DHT response")
	}
	if d.Type != DHTMessage {
		t.Errorf("expected DHTMessage, got %v", d.Type)
	}
}

func TestDHTError(t *testing.T) {
	payload := []byte("d1:eli201e23:A Generic Error Ocurrede1:t2:aa1:y1:ee")
	d := CheckUDPPayload(payload, srcIP, dstIP, 6882, 6881)
	if d == nil {
		t.Fatal("expected detection for DHT error")
	}
	if d.Type != DHTMessage {
		t.Errorf("expected DHTMessage, got %v", d.Type)
	}
}

func TestDHTNotBencoded(t *testing.T) {
	// Doesn't start with 'd'
	payload := []byte("l1:y1:qe")
	d := CheckUDPPayload(payload, srcIP, dstIP, 6881, 6882)
	if d != nil {
		t.Errorf("expected nil for non-dict bencoded data, got %v", d)
	}
}

func TestDHTNoTypeField(t *testing.T) {
	// Starts with 'd' but no y field
	payload := []byte("d1:q4:ping1:t2:aae")
	d := CheckUDPPayload(payload, srcIP, dstIP, 6881, 6882)
	if d != nil {
		t.Errorf("expected nil for bencoded dict without y field, got %v", d)
	}
}

// --- Negative cases ---

func TestTCPEmptyPayload(t *testing.T) {
	d := CheckTCPPayload(nil, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for empty TCP payload, got %v", d)
	}
	d = CheckTCPPayload([]byte{}, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for zero-length TCP payload, got %v", d)
	}
}

func TestUDPEmptyPayload(t *testing.T) {
	d := CheckUDPPayload(nil, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for empty UDP payload, got %v", d)
	}
	d = CheckUDPPayload([]byte{}, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for zero-length UDP payload, got %v", d)
	}
}

func TestUDPTooShort(t *testing.T) {
	d := CheckUDPPayload([]byte("d1:y1:q"), srcIP, dstIP, 1000, 2000) // 7 bytes, need 10
	if d != nil {
		t.Errorf("expected nil for short UDP payload, got %v", d)
	}
}

func TestNormalHTTP(t *testing.T) {
	payload := []byte("GET /index.html HTTP/1.1\r\nHost: example.com\r\n\r\n")
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 80)
	if d != nil {
		t.Errorf("expected nil for normal HTTP, got %v", d)
	}
}

func TestTLSClientHello(t *testing.T) {
	// TLS record: ContentType=22 (handshake), version 0x0303, length
	payload := []byte{0x16, 0x03, 0x03, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00}
	d := CheckTCPPayload(payload, srcIP, dstIP, 45000, 443)
	if d != nil {
		t.Errorf("expected nil for TLS handshake, got %v", d)
	}
}

func TestDNSQuery(t *testing.T) {
	// Fake DNS-ish UDP payload
	payload := []byte{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	d := CheckUDPPayload(payload, srcIP, dstIP, 45000, 53)
	if d != nil {
		t.Errorf("expected nil for DNS query, got %v", d)
	}
}

func TestRandomData(t *testing.T) {
	payload := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55,
		0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		0x01, 0x02, 0x03, 0x04, 0x05}
	d := CheckTCPPayload(payload, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for random TCP data, got %v", d)
	}
	d = CheckUDPPayload(payload, srcIP, dstIP, 1000, 2000)
	if d != nil {
		t.Errorf("expected nil for random UDP data, got %v", d)
	}
}

// --- Detection.String() ---

func TestDetectionString(t *testing.T) {
	d := &Detection{
		Type:    BTHandshake,
		Detail:  "test detail",
		SrcIP:   srcIP,
		DstIP:   dstIP,
		SrcPort: 1234,
		DstPort: 5678,
	}
	s := d.String()
	if s == "" {
		t.Fatal("expected non-empty string")
	}
}

func TestTypeString(t *testing.T) {
	cases := []struct {
		t    Type
		want string
	}{
		{BTHandshake, "BT Handshake"},
		{TrackerAnnounce, "Tracker Announce"},
		{DHTMessage, "DHT Message"},
		{Type(99), "Unknown"},
	}
	for _, c := range cases {
		if got := c.t.String(); got != c.want {
			t.Errorf("Type(%d).String() = %q, want %q", c.t, got, c.want)
		}
	}
}

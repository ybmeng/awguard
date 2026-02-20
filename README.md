# AWGuard

BitTorrent traffic killswitch for macOS. Passively sniffs network traffic for plaintext BitTorrent protocol signatures. If detected, kills the target application immediately and pops a macOS notification.

## Why

If you route torrent traffic through an HTTPS CONNECT proxy, the BitTorrent protocol is hidden inside TLS — invisible on the wire. But if the proxy fails, misconfigures, or a library update changes behavior, BT traffic appears in plaintext, exposing your IP.

AWGuard is the safety net. It sits on the network interface via BPF (Berkeley Packet Filter) and watches for protocol signatures that should never appear if the proxy is working correctly.

## Detection signatures

| Signature | Protocol | Pattern |
|-----------|----------|---------|
| BT Handshake | TCP | `\x13BitTorrent protocol` (first 20 bytes) |
| Tracker Announce | TCP (HTTP) | `GET` + `/announce` + `info_hash=` |
| DHT Message | UDP | Bencoded dict with `1:y1:q` / `1:y1:r` / `1:y1:e` |

If traffic is inside TLS (proxy working), none of these are visible. AWGuard only fires on plaintext BT traffic = a leak.

## Install

```bash
# Requires Go 1.24+ and libpcap (included in macOS SDK)
go build -o awguard .

# Copy to a stable location
sudo cp awguard /usr/local/bin/
```

## Usage

```bash
# Run (requires root for BPF packet capture)
sudo awguard

# Specify interface
sudo awguard -i en0

# Verbose mode (logs every inspected packet)
sudo awguard -v

# Dry run (detect and log, don't kill)
sudo awguard -n
```

## Testing

The simplest way to test detection is with **qBittorrent**:

1. Start awguard in dry-run mode: `sudo awguard -v -n`
2. Open qBittorrent and add any torrent
3. Watch the awguard logs — you should see BT Handshake and/or DHT detections
4. Remove the `-n` flag to test the kill behavior (qBittorrent will be terminated)

Unit tests (no root required):

```bash
go test ./detect/ -v
```

## Run at boot (LaunchDaemon)

```bash
sudo tee /Library/LaunchDaemons/com.awguard.plist << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.awguard</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/awguard</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF

sudo launchctl load /Library/LaunchDaemons/com.awguard.plist
```

## Known limitations

- **MSE/PE**: BitTorrent Message Stream Encryption uses a DH handshake without the standard BT prefix. AWGuard won't detect MSE-encrypted direct connections. Acceptable if your application doesn't use MSE.
- **macOS only**: Uses BPF via libpcap. Linux would need `AF_PACKET` or similar.
- **Requires root**: BPF device access requires root privileges on macOS.

## License

MIT

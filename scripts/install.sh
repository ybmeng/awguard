#!/bin/bash
set -e

cd "$(dirname "${BASH_SOURCE[0]}")/.."

APP_NAME="AWGuard"
BUNDLE_PATH="$HOME/Applications/$APP_NAME.app"
BPF_DAEMON="/Library/LaunchDaemons/com.awguard.bpf.plist"
AGENT_PLIST="$HOME/Library/LaunchAgents/com.awguard.plist"

echo "Building AWGuard..."
go build -o awguard .

echo "Creating app bundle at $BUNDLE_PATH..."
rm -rf "$BUNDLE_PATH"
mkdir -p "$BUNDLE_PATH/Contents/MacOS"
mkdir -p "$BUNDLE_PATH/Contents/Resources"

cp awguard "$BUNDLE_PATH/Contents/MacOS/"

cat > "$BUNDLE_PATH/Contents/Info.plist" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>awguard</string>
    <key>CFBundleIdentifier</key>
    <string>com.awguard</string>
    <key>CFBundleName</key>
    <string>AWGuard</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>LSMinimumSystemVersion</key>
    <string>10.13</string>
    <key>LSUIElement</key>
    <true/>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
EOF

# 1. LaunchDaemon: grant BPF read access to staff group at boot
#    /dev/bpf* are recreated each boot, so permissions must be reset
echo "Installing BPF permissions daemon (requires sudo)..."
sudo tee "$BPF_DAEMON" > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.awguard.bpf</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>-c</string>
        <string>chgrp staff /dev/bpf* &amp;&amp; chmod g+r /dev/bpf*</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
EOF

# Apply BPF permissions now
echo "Granting BPF access to staff group..."
sudo chgrp staff /dev/bpf* && sudo chmod g+r /dev/bpf*

# Unload old daemon if present
sudo launchctl bootout system/com.awguard 2>/dev/null || true
sudo launchctl bootout system/com.awguard.bpf 2>/dev/null || true
sudo rm -f /Library/LaunchDaemons/com.awguard.plist 2>/dev/null

# Load BPF daemon
sudo launchctl bootstrap system "$BPF_DAEMON"

# 2. LaunchAgent: run awguard in user session (has GUI for menu bar icon)
echo "Installing LaunchAgent..."
launchctl bootout "gui/$(id -u)/com.awguard" 2>/dev/null || true
mkdir -p "$HOME/Library/LaunchAgents"

cat > "$AGENT_PLIST" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.awguard</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BUNDLE_PATH/Contents/MacOS/awguard</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
EOF

launchctl bootstrap "gui/$(id -u)" "$AGENT_PLIST"

echo ""
echo "Done! AWGuard is running."
echo "  Shield icon should appear in the menu bar."
echo "  BPF permissions restored at every boot (LaunchDaemon)."
echo "  AWGuard starts at login (LaunchAgent)."
echo ""
echo "  To stop: launchctl bootout gui/$(id -u)/com.awguard"
echo "  To uninstall: sudo rm $BPF_DAEMON && rm $AGENT_PLIST"

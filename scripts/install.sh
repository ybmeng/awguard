#!/bin/bash
set -e

cd "$(dirname "${BASH_SOURCE[0]}")/.."

APP_NAME="AWGuard"
BUNDLE_PATH="$HOME/Applications/$APP_NAME.app"
DAEMON_PLIST="/Library/LaunchDaemons/com.awguard.plist"

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

echo "Installing LaunchDaemon (requires sudo)..."
sudo tee "$DAEMON_PLIST" > /dev/null << EOF
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

echo "Loading LaunchDaemon..."
sudo launchctl load "$DAEMON_PLIST"

echo ""
echo "Done! AWGuard is running."
echo "  Shield icon should appear in the menu bar."
echo "  Runs at boot as a LaunchDaemon (root)."
echo "  To stop: sudo launchctl unload $DAEMON_PLIST"

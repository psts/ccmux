#!/bin/bash
set -e

APP_NAME="ccmux"
BUNDLE_ID="com.ccmux.app"
VERSION="1.0.0"
BUILD_DIR=".build/release"
APP_DIR="$BUILD_DIR/$APP_NAME.app"

echo "Building release for arm64 (Apple Silicon)..."
swift build -c release --arch arm64

echo "Creating app bundle..."
mkdir -p "$BUILD_DIR"
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS"
mkdir -p "$APP_DIR/Contents/Resources"

# Copy the arm64 release binary straight from SwiftPM's build product. We build a
# single architecture (Apple Silicon only), so there's no lipo step. This keeps
# rebuilds incremental: the old universal build alternated --arch arm64/x86_64 each
# run, and switching arch forces a full whole-module recompile every time. It also
# avoids lipo clobbering SwiftPM's own build product via the .build/release symlink.
cp ".build/arm64-apple-macosx/release/$APP_NAME" "$APP_DIR/Contents/MacOS/$APP_NAME"

# Copy icon
if [ -f "AppIcon.icns" ]; then
    cp AppIcon.icns "$APP_DIR/Contents/Resources/AppIcon.icns"
    echo "Icon added."
fi

# Copy scripting definition
if [ -f "ccmux.sdef" ]; then
    cp ccmux.sdef "$APP_DIR/Contents/Resources/ccmux.sdef"
    echo "Scripting definition added."
fi

# Copy Claude Code hook scripts (referenced from ~/.claude/settings.json via
# hooks/install-hooks.sh; bundled for reference/distribution).
if [ -d "hooks" ]; then
    cp -r hooks "$APP_DIR/Contents/Resources/hooks"
    chmod +x "$APP_DIR/Contents/Resources/hooks/"*.sh 2>/dev/null || true
    echo "Hook scripts added."
fi

# Create Info.plist
cat > "$APP_DIR/Contents/Info.plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key>
    <string>$APP_NAME</string>
    <key>CFBundleDisplayName</key>
    <string>ccmux</string>
    <key>CFBundleIdentifier</key>
    <string>$BUNDLE_ID</string>
    <key>CFBundleVersion</key>
    <string>$VERSION</string>
    <key>CFBundleShortVersionString</key>
    <string>$VERSION</string>
    <key>CFBundleExecutable</key>
    <string>$APP_NAME</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>LSMinimumSystemVersion</key>
    <string>14.0</string>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>NSSupportsAutomaticTermination</key>
    <false/>
    <key>CFBundleIconFile</key>
    <string>AppIcon</string>
    <key>NSAppleScriptEnabled</key>
    <true/>
    <key>OSAScriptingDefinition</key>
    <string>ccmux.sdef</string>
    <key>LSUIElement</key>
    <false/>
    <key>CFBundleURLTypes</key>
    <array>
        <dict>
            <key>CFBundleURLName</key>
            <string>$BUNDLE_ID.spawn</string>
            <key>CFBundleURLSchemes</key>
            <array>
                <string>ccmux</string>
            </array>
        </dict>
    </array>
</dict>
</plist>
EOF

echo "Code signing..."
SIGN_ID="Apple Development: Patric Sandelin (H5287KRN8S)"
# No hardened runtime (--options runtime): it's only needed for notarization,
# which an Apple Development cert can't do anyway, so there's nothing to gain.
# NB: signing has no bearing on glyph rendering. The ".notdef tofu" bug was a
# cold-launch CoreText font race (terminals resolved Monaco before the
# LaunchServices session's font connection was ready), fixed by deferring window
# creation in AppDelegate — not hardened runtime or library validation, despite
# what an earlier version of this comment claimed.
codesign --force --sign "$SIGN_ID" "$APP_DIR"
codesign --verify --verbose=2 "$APP_DIR"

echo ""
echo "✅ Built: $APP_DIR"
echo ""
echo "To install:  cp -r $APP_DIR /Applications/"
echo "To run:      open $APP_DIR"

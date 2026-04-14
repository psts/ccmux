#!/bin/bash
set -e

APP_NAME="ccmux"
BUNDLE_ID="com.ccmux.app"
VERSION="1.0.0"
BUILD_DIR=".build/release"
APP_DIR="$BUILD_DIR/$APP_NAME.app"

echo "Building release for arm64..."
swift build -c release --arch arm64

echo "Building release for x86_64..."
swift build -c release --arch x86_64

echo "Creating universal binary..."
mkdir -p "$BUILD_DIR"
lipo -create \
    ".build/arm64-apple-macosx/release/$APP_NAME" \
    ".build/x86_64-apple-macosx/release/$APP_NAME" \
    -output "$BUILD_DIR/$APP_NAME"

echo "Creating app bundle..."
rm -rf "$APP_DIR"
mkdir -p "$APP_DIR/Contents/MacOS"
mkdir -p "$APP_DIR/Contents/Resources"

# Copy universal binary
cp "$BUILD_DIR/$APP_NAME" "$APP_DIR/Contents/MacOS/$APP_NAME"

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
</dict>
</plist>
EOF

echo ""
echo "✅ Built: $APP_DIR"
echo ""
echo "To install:  cp -r $APP_DIR /Applications/"
echo "To run:      open $APP_DIR"

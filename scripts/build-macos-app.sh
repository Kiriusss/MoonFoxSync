#!/bin/sh
set -eu
root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
target=${1:?请提供应用包输出目录}
mkdir -p "$target"
target=$(cd "$target" && pwd)
app="$target/心狐同步.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$app/Contents/Library/LaunchAgents" "$app/Contents/Helpers"
cd "$root"
"${GO:-go}" build -trimpath -o "$app/Contents/Resources/kdrive" ./cmd/kdrive
"${GO:-go}" build -trimpath -o "$app/Contents/Resources/kdrive-agent" ./cmd/kdrive-agent
xcrun swiftc -parse-as-library -O -target arm64-apple-macos14.0 client/macos/*.swift -o "$app/Contents/MacOS/Xinhu"
xcrun swiftc -parse-as-library -O -target arm64-apple-macos14.0 client/macos/Login/XinhuLogin.swift -o "$app/Contents/Helpers/XinhuLogin"
cp client/macos/Login/local.kirius.xinhu.login.plist "$app/Contents/Library/LaunchAgents/"
cp assets/brand/xinhu.png "$app/Contents/Resources/Avatar.png"
cp assets/brand/tray/TrayFox*.png "$app/Contents/Resources/"
iconbuild=$(mktemp -d)
trap 'rm -rf "$iconbuild"' EXIT
iconset="$iconbuild/Avatar.iconset"
mkdir "$iconset"
for size in 16 32 128 256 512; do
    sips -z "$size" "$size" assets/brand/xinhu.png --out "$iconset/icon_${size}x${size}.png" >/dev/null
    double=$((size * 2))
    sips -z "$double" "$double" assets/brand/xinhu.png --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/Avatar.icns"
cp client/macos/Info.plist "$app/Contents/Info.plist"
codesign --force --sign - "$app/Contents/Helpers/XinhuLogin"
codesign --force --sign - "$app/Contents/Resources/kdrive"
codesign --force --sign - "$app/Contents/Resources/kdrive-agent"
codesign --force --sign - "$app"
codesign --verify --strict "$app"

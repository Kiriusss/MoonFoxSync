#!/bin/sh
set -eu
root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
target=${1:?请提供应用包输出目录}
mkdir -p "$target"
target=$(cd "$target" && pwd)
app="$target/心狐同步.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$app/Contents/Library/LaunchAgents" "$app/Contents/Helpers"
badge="$app/Contents/PlugIns/MoonFoxFinderBadges.appex"
mkdir -p "$badge/Contents/MacOS"
cd "$root"
CGO_ENABLED=1 "${GO:-go}" build -trimpath -o "$app/Contents/Resources/kdrive" ./cmd/kdrive
CGO_ENABLED=1 "${GO:-go}" build -trimpath -o "$app/Contents/Resources/kdrive-agent" ./cmd/kdrive-agent
xcrun swiftc -parse-as-library -O -target arm64-apple-macos14.0 client/macos/*.swift -o "$app/Contents/MacOS/Xinhu"
xcrun swiftc -parse-as-library -O -target arm64-apple-macos14.0 client/macos/Login/XinhuLogin.swift -o "$app/Contents/Helpers/XinhuLogin"
xcrun swiftc -parse-as-library -O -target arm64-apple-macos14.0 client/macos/WorkspaceMountPaths.swift client/macos/WorkspaceMount/XinhuWorkspace.swift -o "$app/Contents/Helpers/XinhuWorkspace"
xcrun swiftc -parse-as-library -O -application-extension -target arm64-apple-macos14.0 -Xlinker -e -Xlinker _NSExtensionMain client/macos/FinderBadges/*.swift -o "$badge/Contents/MacOS/MoonFoxFinderBadges"
cp client/macos/FinderBadges/Info.plist "$badge/Contents/Info.plist"
cp client/macos/WorkspaceMount/local.kirius.xinhu.workspace.plist "$app/Contents/Library/LaunchAgents/"
cp client/macos/Login/local.kirius.xinhu.login.plist "$app/Contents/Library/LaunchAgents/"
cp -R client/macos/*.lproj "$app/Contents/Resources/"
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
identity=${MOONFOX_SIGN_IDENTITY:--}
group=${MOONFOX_APP_GROUP:-group.local.kirius.xinhu}
host_ent="$iconbuild/Workspace.entitlements"
badge_ent="$iconbuild/FinderBadges.entitlements"
cp client/macos/Workspace.entitlements "$host_ent"
cp client/macos/FinderBadges/Extension.entitlements "$badge_ent"
for info in "$app/Contents/Info.plist" "$badge/Contents/Info.plist"; do
    plutil -replace MoonFoxAppGroup -string "$group" "$info"
done
for ent in "$host_ent" "$badge_ent"; do
    /usr/libexec/PlistBuddy -c "Set :com.apple.security.application-groups:0 $group" "$ent"
done
for key in CFBundleVersion CFBundleShortVersionString; do
    version=$(plutil -extract "$key" raw "$app/Contents/Info.plist")
    plutil -replace "$key" -string "$version" "$badge/Contents/Info.plist"
done
if [ -n "${MOONFOX_HOST_PROFILE:-}" ]; then cp "$MOONFOX_HOST_PROFILE" "$app/Contents/embedded.provisionprofile"; fi
if [ -n "${MOONFOX_BADGE_PROFILE:-}" ]; then cp "$MOONFOX_BADGE_PROFILE" "$badge/Contents/embedded.provisionprofile"; fi
for bin in "$app/Contents/Helpers/XinhuLogin" "$app/Contents/Helpers/XinhuWorkspace" "$app/Contents/Resources/kdrive" "$app/Contents/Resources/kdrive-agent"; do
    codesign --force --sign "$identity" "$bin"
done
codesign --force --sign "$identity" --entitlements "$badge_ent" "$badge"
codesign --force --sign "$identity" --entitlements "$host_ent" "$app"
codesign --verify --deep --strict "$app"

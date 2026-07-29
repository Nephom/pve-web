#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
MOUNT=${PVE_WEB_MOUNT:-/mnt/pve-web}
VERSION=${PVE_WEB_VERSION:-dev}
SOURCE=${1:-freebsd}
COMMIT=${PVE_WEB_COMMIT:-unknown}
BUILD_TIME=${PVE_WEB_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

if ! command -v npm >/dev/null 2>&1; then
    echo "npm is required to build the React frontend" >&2
    exit 1
fi

cd "$ROOT/frontend"
if [ ! -d node_modules ]; then npm install; fi
npm run build
cd "$ROOT"
rm -rf dist
mkdir -p dist
cp -R frontend/dist dist/frontend

build_one() {
    os=$1
    arch=amd64
    out="dist/${os}-${arch}"
    mkdir -p "$out"
    suffix=
    [ "$os" = windows ] && suffix=.exe
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME" \
        -o "$out/pve-web${suffix}" ./cmd/pve-web
    tar -C dist/frontend -czf "$out/frontend.tar.gz" .
    (cd "$out" && sha256sum "pve-web${suffix}" frontend.tar.gz > checksums.txt)
}

case "$SOURCE" in
    freebsd|linux|windows) build_one "$SOURCE" ;;
    all) build_one freebsd; build_one linux; build_one windows ;;
    *) echo "usage: $0 [freebsd|linux|windows|all]" >&2; exit 2 ;;
esac

release="dist/release/$VERSION"
mkdir -p "$release"
for platform in freebsd linux windows; do
    if [ -d "dist/${platform}-amd64" ]; then
        mkdir -p "$release/$platform-amd64"
        cp "dist/${platform}-amd64"/* "$release/$platform-amd64/"
    fi
done
printf '{\n  "name": "pve-web",\n  "version": "%s",\n  "artifacts": {\n    "freebsd-amd64": "freebsd-amd64",\n    "linux-amd64": "linux-amd64",\n    "windows-amd64": "windows-amd64"\n  }\n}\n' "$VERSION" > "$release/manifest.json"
cp "$release/manifest.json" dist/release/latest.json
cp pve-web.yaml.example pve_web.in dist/release/

# webroot is the ready-to-publish tree. Expose dist/webroot at /pve-web/.
webroot="dist/webroot"
rm -rf "$webroot"
mkdir -p "$webroot/releases/$VERSION"
cp pve-web.yaml.example pve_web.in "$webroot/"
cp dist/release/latest.json "$webroot/releases/latest.json"
for platform in freebsd linux windows; do
    if [ -d "dist/${platform}-amd64" ]; then
        mkdir -p "$webroot/releases/$VERSION/$platform-amd64"
        cp "dist/${platform}-amd64"/* "$webroot/releases/$VERSION/$platform-amd64/"
    fi
done

# Also place the release tree at the project root. This makes a complete copy
# of the project directly publishable as /pve-web/ for deploy.sh.
published="releases"
rm -rf "$published"
mkdir -p "$published/$VERSION"
cp dist/release/latest.json "$published/latest.json"
for platform in freebsd linux windows; do
    if [ -d "dist/${platform}-amd64" ]; then
        mkdir -p "$published/$VERSION/$platform-amd64"
        cp "dist/${platform}-amd64"/* "$published/$VERSION/$platform-amd64/"
    fi
done

# Keep the tree readable by an Alpine/Nginx user after it is copied to /mnt.
find "$webroot" -type d -exec chmod 755 {} \;
find "$webroot" -type f -exec chmod 644 {} \;
for platform in freebsd linux windows; do
    if [ -f "$webroot/releases/$VERSION/$platform-amd64/pve-web" ]; then
        chmod 755 "$webroot/releases/$VERSION/$platform-amd64/pve-web"
    fi
    if [ -f "$webroot/releases/$VERSION/$platform-amd64/pve-web.exe" ]; then
        chmod 755 "$webroot/releases/$VERSION/$platform-amd64/pve-web.exe"
    fi
done

if [ "$MOUNT" = "$ROOT" ]; then
    echo "PVE_WEB_MOUNT must not be the project directory" >&2
    exit 1
fi
mkdir -p "$MOUNT"
rm -rf "$MOUNT/dist" "$MOUNT/releases"
rm -rf "$MOUNT/frontend/node_modules" "$MOUNT/frontend/dist"
tar -C "$ROOT" \
    --exclude ./frontend/node_modules \
    --exclude ./frontend/dist \
    --exclude ./dist \
    --exclude ./releases \
    -cf - . | tar --no-same-owner -C "$MOUNT" -xf -
cp -R "$ROOT/dist" "$MOUNT/dist"
cp -R "$ROOT/releases" "$MOUNT/releases"
find "$MOUNT/releases" -type d -exec chmod 755 {} \;
find "$MOUNT/releases" -type f -exec chmod 644 {} \;
find "$MOUNT/releases" -type f -name 'pve-web' -exec chmod 755 {} \;
find "$MOUNT/releases" -type f -name 'pve-web.exe' -exec chmod 755 {} \;
chmod 755 "$MOUNT/build.sh" "$MOUNT/deploy.sh"

echo "Built pve-web version $VERSION ($SOURCE)"
echo "Publish-ready webroot: $ROOT/$webroot"
echo "Synchronized project: $MOUNT"

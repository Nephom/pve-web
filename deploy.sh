#!/bin/sh
set -eu

SOURCE=${PVE_WEB_SOURCE:-http://172.20.1.6/pve-web}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PREFIX=${PVE_WEB_PREFIX:-/usr/local}
ETC=${PVE_WEB_ETC:-$PREFIX/etc/pve-web}
LIBEXEC=${PVE_WEB_LIBEXEC:-$PREFIX/libexec/pve-web}
SHARE=${PVE_WEB_SHARE:-$PREFIX/share/pve-web}
RCD=${PVE_WEB_RCD:-$PREFIX/etc/rc.d}
BACKUP="$LIBEXEC/releases"
SERVICE=pve_web

usage() { echo "usage: $0 install|upgrade|status|version|rollback|uninstall [source-url]"; }
[ $# -ge 2 ] && SOURCE=$2
command_exists() { command -v "$1" >/dev/null 2>&1; }
fetch_file() { if command_exists fetch; then fetch -q -o "$2" "$1"; else curl -fsSL "$1" -o "$2"; fi; }
current_version() { if [ -f "$LIBEXEC/VERSION" ]; then tr -d '\n' < "$LIBEXEC/VERSION"; else echo none; fi; }
latest() { fetch_file "${SOURCE%/}/releases/latest.json" "$tmp/latest.json"; }
stop_service() { if service "$SERVICE" onestatus >/dev/null 2>&1; then service "$SERVICE" stop; fi; }
start_service() { service "$SERVICE" start; }
enable_service() {
    if command_exists sysrc; then
        sysrc pve_web_enable=YES >/dev/null
    else
        echo "WARNING: sysrc is unavailable; enable pve_web_enable=YES in /etc/rc.conf" >&2
    fi
}

prepare_credentials() {
    # Windows exports use the singular portable name. The rc.d script requires
    # the plural runtime name, so convert it before attempting to start.
    if [ ! -f "$ETC/pve-web-credentials.json" ] && [ -f "$ETC/pve-web-credential.json" ]; then
        cp "$ETC/pve-web-credential.json" "$ETC/pve-web-credentials.json"
        chmod 600 "$ETC/pve-web-credentials.json"
        echo "Imported portable credentials into $ETC/pve-web-credentials.json"
    fi
}

runtime_report() {
    echo "Service status:"
    service "$SERVICE" onestatus || true
    if command_exists fetch; then
        if health=$(fetch -qo - http://127.0.0.1:8080/pve-web/health); then
            echo "Health response: $health"
        else
            echo "Health response: FAILED"
        fi
        if version_response=$(fetch -qo - http://127.0.0.1:8080/pve-web/version); then
            echo "Version response: $version_response"
        else
            echo "Version response: FAILED"
        fi
        if fetch -qo /dev/null http://127.0.0.1:8080/pve-web/data/overview; then
            echo "Overview response: OK"
        else
            echo "Overview response: FAILED"
        fi
    else
        echo "Health check: fetch -qo - http://127.0.0.1:8080/pve-web/health"
        echo "Version check: fetch -qo - http://127.0.0.1:8080/pve-web/version"
        echo "Overview check: fetch -qo - http://127.0.0.1:8080/pve-web/data/overview"
    fi
    echo ""
    echo "Backend URL (on this FreeBSD host): http://127.0.0.1:8080/pve-web/"
    echo "Browser URL after reverse proxy: ${PVE_WEB_PUBLIC_URL:-http://172.20.1.6/pve-web/}"
    echo "Required next step: configure Nginx to proxy /pve-web/ to this FreeBSD host on port 8080."
    echo "The release host at 172.20.1.6 is not the application backend unless Nginx proxies this path."
    echo "Log: tail -f /var/log/pve-web/pve-web.log"
}

post_install() {
    version=$1
    prepare_credentials
    chmod 600 "$ETC/pve-web.yaml" 2>/dev/null || true

    if [ ! -f "$ETC/pve-web-credentials.json" ]; then
        echo "Installed pve-web $version, but credentials are not configured."
        echo "Next steps:"
        echo "  1. Copy pve-web-credential.json to $ETC/"
        echo "  2. Check target IDs/endpoints in $ETC/pve-web.yaml"
        echo "  3. Run: $0 install"
        echo "  4. Check: service $SERVICE onestatus"
        return 0
    fi

    enable_service
    if service "$SERVICE" onestatus >/dev/null 2>&1; then
        echo "pve-web $version is already running and enabled."
        runtime_report
    elif service "$SERVICE" start; then
        echo "pve-web $version is enabled and running."
        runtime_report
    else
        echo "WARNING: pve-web was installed but could not be started." >&2
        echo "Check: service $SERVICE onestatus" >&2
        echo "Log: tail -n 100 /var/log/pve-web/pve-web.log" >&2
        return 1
    fi
}

install_package() {
    package=$1
    if ! command_exists pkg; then
        echo "pkg is required to install missing dependency: $package" >&2
        exit 1
    fi
    echo "Installing missing FreeBSD package: $package"
    pkg install -y "$package"
}

ensure_dependencies() {
    # fetch and sha256 are normally provided by the FreeBSD base system.
    # Only install packages when the base utilities are genuinely unavailable.
    if ! command_exists fetch && ! command_exists curl; then install_package curl; fi
    if ! command_exists sha256 && ! command_exists sha256sum; then install_package coreutils; fi
    if ! command_exists fetch && ! command_exists curl; then echo "no download utility available after dependency installation" >&2; exit 1; fi
    if ! command_exists sha256 && ! command_exists sha256sum; then echo "no SHA-256 utility available after dependency installation" >&2; exit 1; fi
}

tmp=$(mktemp -d /tmp/pve-web.XXXXXX)
trap 'rm -rf "$tmp"' EXIT INT TERM
ensure_dependencies

install_release() {
    mkdir -p "$ETC" "$LIBEXEC" "$SHARE" "$BACKUP" "$RCD" /var/log/pve-web
    if [ ! -f "$ETC/pve-web.yaml" ]; then
        if [ -f "$SCRIPT_DIR/pve-web.yaml.example" ]; then cp "$SCRIPT_DIR/pve-web.yaml.example" "$ETC/pve-web.yaml"; else fetch_file "${SOURCE%/}/pve-web.yaml.example" "$ETC/pve-web.yaml"; fi
    fi
    if [ -f "$SCRIPT_DIR/pve_web.in" ]; then cp "$SCRIPT_DIR/pve_web.in" "$RCD/pve_web"; else fetch_file "${SOURCE%/}/pve_web.in" "$RCD/pve_web"; fi
    chmod 755 "$RCD/pve_web"
    latest
    version=$(awk -F'"' '/"version"/{print $4; exit}' "$tmp/latest.json")
    [ -n "$version" ] || { echo "latest manifest has no version" >&2; exit 1; }
    if [ "$(current_version)" = "$version" ]; then
        echo "pve-web $version is already installed"
        post_install "$version"
        return
    fi
    release="${SOURCE%/}/releases/$version"
    fetch_file "$release/freebsd-amd64/pve-web" "$tmp/pve-web"
    fetch_file "$release/freebsd-amd64/frontend.tar.gz" "$tmp/frontend.tar.gz"
    fetch_file "$release/freebsd-amd64/checksums.txt" "$tmp/checksums.txt"
    if command_exists sha256; then (cd "$tmp" && sha256 -c checksums.txt); elif command_exists sha256sum; then (cd "$tmp" && sha256sum -c checksums.txt); else echo "sha256 or sha256sum is required" >&2; exit 1; fi
    chmod 755 "$tmp/pve-web"
    old=$(current_version)
    stop_service
    if [ "$old" != none ] && [ -x "$LIBEXEC/pve-web" ]; then mkdir -p "$BACKUP/$old"; cp "$LIBEXEC/pve-web" "$BACKUP/$old/pve-web"; fi
    mv "$tmp/pve-web" "$LIBEXEC/pve-web"
    rm -rf "$SHARE/frontend.new"; mkdir "$SHARE/frontend.new"; tar -xzf "$tmp/frontend.tar.gz" -C "$SHARE/frontend.new"; rm -rf "$SHARE/frontend.old"; [ -d "$SHARE/frontend" ] && mv "$SHARE/frontend" "$SHARE/frontend.old" || true; mv "$SHARE/frontend.new" "$SHARE/frontend"
    printf '%s\n' "$version" > "$LIBEXEC/VERSION"
    chmod 755 "$LIBEXEC/pve-web"
    post_install "$version"
}

case ${1:-} in
    install|upgrade) install_release ;;
    status)
        echo "version: $(current_version)"
        [ -f "$ETC/pve-web.yaml" ] && echo "config: $ETC/pve-web.yaml" || echo "config: missing"
        [ -f "$ETC/pve-web-credentials.json" ] && echo "credentials: configured" || echo "credentials: missing"
        service "$SERVICE" onestatus || true
        ;;
    version) current_version ;;
    rollback) old=$(ls -1 "$BACKUP" 2>/dev/null | sort | tail -n 1); [ -n "$old" ] || { echo "no rollback release"; exit 1; }; stop_service; cp "$BACKUP/$old/pve-web" "$LIBEXEC/pve-web"; printf '%s\n' "$old" > "$LIBEXEC/VERSION"; start_service; echo "Rolled back to $old" ;;
    uninstall) stop_service; rm -f "$LIBEXEC/pve-web" "$LIBEXEC/VERSION"; rm -rf "$SHARE/frontend"; echo "Removed binary and frontend; configuration was preserved" ;;
    *) usage; exit 2 ;;
esac

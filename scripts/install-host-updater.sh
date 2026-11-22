#!/usr/bin/env bash

set -Eeuo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/open-ai-canvas}"
# 发布仓库：Release 资产中的 Host Updater 二进制与 SHA256SUMS 都从这里下载。
REPOSITORY="${REPOSITORY:-empty616165/genvie}"
SOCKET_DIR="${CANVAS_UPDATER_SOCKET_DIR:-/run/open-ai-canvas-updater}"
# 可为空的 GitHub 加速前缀，例如 https://gh-proxy.org/。
# 留空时更新器 env 写入 https://gh-proxy.org/，与 hostupdate.DefaultGitHubProxy 保持一致。
GITHUB_PROXY="${GITHUB_PROXY:-}"
# 容器镜像源主机名，例如 ghcr.nju.edu.cn 或自建镜像；与 hostupdate.DefaultImageMirror 一致。
IMAGE_MIRROR="${IMAGE_MIRROR:-ghcr.nju.edu.cn}"
UPDATER_BIN="/usr/local/bin/open-ai-canvas-host-updater"
UPDATER_ENV="/etc/open-ai-canvas-updater.env"
UPDATER_SERVICE="/etc/systemd/system/open-ai-canvas-updater.service"

fail() {
    printf 'Host Updater 安装失败：%s\n' "$1" >&2
    exit 1
}

require_root() {
    [[ "${EUID}" -eq 0 ]] || fail "请使用 sudo 运行"
    [[ "$(uname -s)" == "Linux" ]] || fail "仅支持 Linux 服务器"
    command -v systemctl >/dev/null 2>&1 || fail "服务器必须使用 systemd"
    command -v curl >/dev/null 2>&1 || fail "缺少 curl"
    command -v sha256sum >/dev/null 2>&1 || fail "缺少 sha256sum"
    command -v openssl >/dev/null 2>&1 || fail "缺少 openssl"
    [[ -f "${INSTALL_DIR}/.env" ]] || fail "未找到 ${INSTALL_DIR}/.env"
    [[ -f "${INSTALL_DIR}/docker-compose.deploy.yml" ]] || fail "未找到部署 Compose"
    local backend_image web_image
    backend_image="$(sed -n 's/^CANVAS_BACKEND_IMAGE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    web_image="$(sed -n 's/^CANVAS_WEB_IMAGE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    # 安全属性是「必须固定 digest」，主机名允许自建镜像源（CANVAS_UPDATER_IMAGE_MIRROR）。
    [[ "$backend_image" =~ ^[^[:space:]/]+/[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || fail "CANVAS_BACKEND_IMAGE 必须以 @sha256:<64 位十六进制> 固定 digest"
    [[ "$web_image" =~ ^[^[:space:]/]+/[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || fail "CANVAS_WEB_IMAGE 必须以 @sha256:<64 位十六进制> 固定 digest"
}

read_image_tag() {
    local configured
    configured="$(sed -n 's/^CANVAS_IMAGE_TAG=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    [[ -n "$configured" && "$configured" != "latest" ]] || fail "请先把 CANVAS_IMAGE_TAG 固定为已发布版本"
    if [[ "$configured" == v* ]]; then
        RELEASE_TAG="$configured"
    else
        RELEASE_TAG="v${configured}"
    fi
}

# GitHub 直连与加速前缀都是显式配置；不设置 GITHUB_PROXY 时只走直连。
download_asset() {
    local destination="$1"
    local url="$2"
    local candidate
    for candidate in "${GITHUB_PROXY:+${GITHUB_PROXY%/}/$url}" "$url"; do
        [[ -n "$candidate" ]] || continue
        if curl -fsSL --connect-timeout 15 --max-time 120 "$candidate" -o "$destination"; then
            return 0
        fi
    done
    return 1
}

install_binary() {
    local arch asset temporary checksum_file expected release_url
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) fail "不支持的 CPU 架构：$(uname -m)" ;;
    esac
    asset="open-ai-canvas-host-updater-linux-${arch}"
    temporary="$(mktemp)"
    checksum_file="$(mktemp)"
    release_url="https://github.com/${REPOSITORY}/releases/download/${RELEASE_TAG}"
    download_asset "$temporary" "${release_url}/${asset}" \
        || fail "下载 ${asset} 失败；可设置 GITHUB_PROXY（例如 https://gh-proxy.org/）后重试"
    download_asset "$checksum_file" "${release_url}/SHA256SUMS" \
        || fail "下载 SHA256SUMS 失败；可设置 GITHUB_PROXY（例如 https://gh-proxy.org/）后重试"
    expected="$(awk -v asset="$asset" '$2 == asset { print $1 }' "$checksum_file")"
    [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "Release 校验清单缺少 ${asset}"
    printf '%s  %s\n' "$expected" "$temporary" | sha256sum -c - >/dev/null || fail "Host Updater SHA-256 校验失败"
    install -m 0755 "$temporary" "$UPDATER_BIN"
    rm -f "$temporary" "$checksum_file"
}

ensure_token() {
    local token temporary
    token="$(sed -n 's/^CANVAS_UPDATER_TOKEN=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    if [[ -z "$token" ]]; then
        token="$(openssl rand -hex 32)"
        temporary="$(mktemp "${INSTALL_DIR}/.env.XXXXXX")"
        awk -v token="$token" '
            BEGIN { updated=0 }
            /^CANVAS_UPDATER_TOKEN=/ { print "CANVAS_UPDATER_TOKEN=" token; updated=1; next }
            { print }
            END { if (!updated) print "CANVAS_UPDATER_TOKEN=" token }
        ' "${INSTALL_DIR}/.env" > "$temporary"
        chmod --reference="${INSTALL_DIR}/.env" "$temporary"
        mv "$temporary" "${INSTALL_DIR}/.env"
    fi
    [[ ${#token} -ge 32 ]] || fail "CANVAS_UPDATER_TOKEN 长度不足"
    umask 077
    # 显式写入仓库，重装脚本时即可修正旧安装遗留的仓库地址。
    # 加速前缀与镜像源同样显式落盘，避免沿用更新器内置的公共默认值；
    # 未传入加速前缀时写入实测可用的默认值，保证更新器不会退化为直连。
    local configured_proxy configured_mirror
    configured_proxy="${GITHUB_PROXY:-https://gh-proxy.org/}"
    configured_mirror="${IMAGE_MIRROR:-ghcr.nju.edu.cn}"
    printf 'CANVAS_UPDATER_TOKEN=%s\nCANVAS_UPDATER_INSTALL_DIR=%s\nCANVAS_UPDATER_SOCKET=%s/updater.sock\nCANVAS_UPDATER_REPOSITORY=%s\nCANVAS_UPDATER_GITHUB_PROXY=%s\nCANVAS_UPDATER_IMAGE_MIRROR=%s\n' \
        "$token" "$INSTALL_DIR" "$SOCKET_DIR" "$REPOSITORY" "$configured_proxy" "$configured_mirror" > "$UPDATER_ENV"
}

install_service() {
    local temporary_service
    install -d -m 0755 "$SOCKET_DIR"
    install -d -m 0700 /var/lib/open-ai-canvas-updater "${INSTALL_DIR}/backups"
    temporary_service="$(mktemp)"
    printf '%s\n' \
        '[Unit]' \
        'Description=Open AI Canvas Host Updater' \
        'After=docker.service network-online.target' \
        'Requires=docker.service' \
        'Wants=network-online.target' \
        '' \
        '[Service]' \
        'Type=simple' \
        "EnvironmentFile=${UPDATER_ENV}" \
        "ExecStart=${UPDATER_BIN}" \
        'Restart=on-failure' \
        'RestartSec=5s' \
        'NoNewPrivileges=true' \
        'PrivateTmp=true' \
        'ProtectHome=true' \
        'ProtectSystem=full' \
        "ReadWritePaths=${INSTALL_DIR} /var/lib/open-ai-canvas-updater ${SOCKET_DIR} /usr/local/bin" \
        '' \
        '[Install]' \
        'WantedBy=multi-user.target' > "$temporary_service"
    install -m 0644 "$temporary_service" "$UPDATER_SERVICE"
    rm -f "$temporary_service"
    systemctl daemon-reload
    systemctl enable --now open-ai-canvas-updater.service
    systemctl restart open-ai-canvas-updater.service
}

main() {
    require_root
    read_image_tag
    install_binary
    ensure_token
    install_service
    printf 'Host Updater 已安装，Socket：%s/updater.sock\n' "$SOCKET_DIR"
    printf '请重建 backend 容器，使 Token 与 Socket 挂载生效。\n'
}

main "$@"

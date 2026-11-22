#!/usr/bin/env bash
# 只读诊断：不修改任何配置，仅报告当前服务器能否连通更新所需的三类出口。
# 用法：
#   bash scripts/diagnose-update-egress.sh
#   GITHUB_PROXY=https://github.com IMAGE_MIRROR=ghcr.nju.edu.cn bash scripts/diagnose-update-egress.sh
set -uo pipefail

REPOSITORY="${REPOSITORY:-empty616165/genvie}"
# 与更新器默认值保持一致；可用环境变量覆盖以验证候选地址。
GITHUB_PROXY="${GITHUB_PROXY:-https://gh-proxy.org/}"
IMAGE_MIRROR="${IMAGE_MIRROR:-ghcr.nju.edu.cn}"
VERSION="${VERSION:-$(tr -d '[:space:]' < "$(dirname "$0")/../VERSION" 2>/dev/null || echo "")}"
# 吞吐抽样用的最大层摘要。优先按目标版本自动解析，避免换版本后写死的摘要失效
# 导致「ghcr.io 拿不到数据」的误报；也可用 BIG_LAYER_DIGEST 显式覆盖。
BIG_LAYER_DIGEST="${BIG_LAYER_DIGEST:-}"

ok=0
bad=0

report() {
    local name="$1" state="$2" detail="$3"
    if [[ "$state" == "ok" ]]; then
        printf '  [通过] %-28s %s\n' "$name" "$detail"
        ok=$((ok + 1))
    else
        printf '  [失败] %-28s %s\n' "$name" "$detail"
        bad=$((bad + 1))
    fi
}

# 打印 HTTP 状态码与耗时；000 表示连接层失败。
probe() {
    local url="$1"
    curl -s -o /dev/null -w '%{http_code} %{time_total}' --max-time "${PROBE_TIMEOUT:-25}" "$url"
}

echo "更新出口诊断（只读）"
echo "  GitHub 加速前缀: ${GITHUB_PROXY}"
echo "  容器镜像源:      ${IMAGE_MIRROR}"
[[ -n "$VERSION" ]] && echo "  目标版本:        ${VERSION}"
echo

echo "1. GitHub 出口"
for label in "所选加速前缀:${GITHUB_PROXY}" "官方直连:"; do
    name="${label%%:*}"
    prefix="${label#*:}"
    read -r code elapsed <<<"$(probe "${prefix}https://api.github.com/repos/${REPOSITORY}/releases?per_page=1")"
    if [[ "$code" == "200" ]]; then
        report "Release API · ${name}" ok "${code} ${elapsed}s"
    elif [[ "$code" == "403" ]]; then
        report "Release API · ${name}" fail "${code} ${elapsed}s（限流或被拒；更新器会自动尝试另一出口）"
    else
        report "Release API · ${name}" fail "${code} ${elapsed}s"
    fi
done

read -r code elapsed <<<"$(probe "${GITHUB_PROXY}https://raw.githubusercontent.com/${REPOSITORY}/main/VERSION")"
if [[ "$code" == "200" ]]; then
    report "Raw 内容" ok "${code} ${elapsed}s"
else
    report "Raw 内容" fail "${code} ${elapsed}s；更新器会因出口不可用直接失败"
fi

echo
echo "2. 容器镜像清单"
for image in genvie-canvas-web genvie-canvas-backend genvie-canvas-yingce-agent; do
    read -r code elapsed <<<"$(probe "https://${IMAGE_MIRROR}/v2/${REPOSITORY%%/*}/${image}/manifests/${VERSION#v}")"
    # 仓库若要求鉴权，401 说明网络可达但需要登录，同样值得单独提示。
    if [[ "$code" == "200" ]]; then
        report "$image" ok "${code} ${elapsed}s"
    elif [[ "$code" == "401" ]]; then
        report "$image" ok "${code} ${elapsed}s（可达但需鉴权）"
    else
        report "$image" fail "${code} ${elapsed}s"
    fi
done

# 清单可达不代表大层能下完：公共镜像源常在超大层上掉到 100~200 KB/s，
# 三镜像共享超时必然失败。这里只抽样测速，不下载完整层。
if [[ "${SKIP_THROUGHPUT:-auto}" != "yes" ]]; then
    echo
    echo "3. 大层吞吐抽样（10 秒，判断是否会拖到超时）"
    # 用限时完整下载测真实吞吐：部分镜像源不支持 Range 请求，直接用 --max-time 截断更可靠。
    throughput() {
        local reg="$1" token="$2" digest="$3"
        if [[ -n "$token" ]]; then
            curl -sL -o /dev/null -H "Authorization: Bearer $token" -w '%{speed_download}' --max-time 10 \
                "https://${reg}/v2/${REPOSITORY%%/*}/genvie-canvas-web/blobs/${digest}" 2>/dev/null
        else
            curl -sL -o /dev/null -w '%{speed_download}' --max-time 10 \
                "https://${reg}/v2/${REPOSITORY%%/*}/genvie-canvas-web/blobs/${digest}" 2>/dev/null
        fi
    }
    # ghcr.io 匿名拉取需要 token；取不到就跳过该项而不是误报失败。
    ghcr_token="$(curl -s --max-time 15 "https://ghcr.io/token?scope=repository%3A${REPOSITORY%%/*}%2Fgenvie-canvas-web%3Apull&service=ghcr.io" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
    # 自动解析目标版本 amd64 清单中的最大层，避免换版本后写死摘要失效造成误报。
    if [[ -z "$BIG_LAYER_DIGEST" ]]; then
        manifest_digest="$(curl -sL --max-time 20 ${ghcr_token:+-H "Authorization: Bearer $ghcr_token"} \
            -H 'Accept: application/vnd.oci.image.index.v1+json' \
            "https://ghcr.io/v2/${REPOSITORY%%/*}/genvie-canvas-web/manifests/${VERSION#v}" \
            | python3 -c 'import json,sys
try:
    data=json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
for m in data.get("manifests") or []:
    if (m.get("platform") or {}).get("architecture")=="amd64":
        print(m.get("digest","")); break' 2>/dev/null)"
        if [[ -n "$manifest_digest" ]]; then
            BIG_LAYER_DIGEST="$(curl -sL --max-time 20 ${ghcr_token:+-H "Authorization: Bearer $ghcr_token"} \
                -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
                "https://ghcr.io/v2/${REPOSITORY%%/*}/genvie-canvas-web/manifests/${manifest_digest}" \
                | python3 -c 'import json,sys
try:
    data=json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
layers=sorted(data.get("layers") or [], key=lambda l: l.get("size",0), reverse=True)
if layers:
    print(layers[0].get("digest",""))' 2>/dev/null)"
        fi
    fi
    if [[ -z "$BIG_LAYER_DIGEST" ]]; then
        printf '  [跳过] %-28s 无法解析目标版本的最大层摘要\n' "大层吞吐抽样"
    else
    for pair in "所选镜像源:${IMAGE_MIRROR}:" "ghcr.io 直连:ghcr.io:${ghcr_token}"; do
        name="${pair%%:*}"; rest="${pair#*:}"
        reg="${rest%%:*}"; token="${rest#*:}"
        speed="$(throughput "$reg" "$token" "$BIG_LAYER_DIGEST")"
        if [[ -z "$speed" || "$speed" == "0" ]]; then
            report "${name} 吞吐" fail "10 秒内没有拿到数据（该源大层可能停滞；更新时会自动退回 ghcr.io）"
        else
            mbps="$(awk -v s="$speed" 'BEGIN { printf "%.1f", s/1048576 }')"
            # 122 MB 单层按此速率需要多久；超过 15 分钟就不可用于自动更新。
            minutes="$(awk -v s="$speed" 'BEGIN { if (s<=0) print 999; else printf "%.0f", 122011384/s/60 }')"
            if [[ "$minutes" -le 15 ]]; then
                report "${name} 吞吐" ok "${mbps} MB/s（最大层约 ${minutes} 分钟）"
            else
                report "${name} 吞吐" fail "${mbps} MB/s（最大层约 ${minutes} 分钟，会撞上超时）"
            fi
        fi
    done
    fi
fi

echo
echo "4. Docker 守护进程"
if command -v docker >/dev/null 2>&1 && docker info --format '{{.DockerRootDir}}' >/dev/null 2>&1; then
    root="$(docker info --format '{{.DockerRootDir}}')"
    available="$(df -Pm "$root" 2>/dev/null | awk 'NR==2 {print $4}')"
    if [[ -n "$available" && "$available" -ge 4096 ]]; then
        report "数据目录空间" ok "$root，剩余 ${available} MiB"
    else
        report "数据目录空间" fail "$root，剩余 ${available:-未知} MiB（更新前要求 ≥4096 MiB）"
    fi
    report "Docker 可用" ok "守护进程可达"
elif [[ "${EXPECT_DOCKER:-auto}" == "no" ]]; then
    # 本地开发机或离线环境：本项不参与结论。
    printf '  [跳过] %-28s 按 EXPECT_DOCKER=no 跳过\n' "Docker 可用"
else
    report "Docker 可用" fail "无法连接守护进程；服务器上必须可用（本地开发机可加 EXPECT_DOCKER=no 跳过本项）"
fi

echo
echo "结论：通过 ${ok} 项，失败 ${bad} 项。"
if [[ "$bad" -gt 0 ]]; then
    echo "说明：若「官方直连」一行通过而「所选加速前缀」失败，请把后台来源切回官方直连（默认），比公共加速域名更稳定。"
    echo "若两个 GitHub 出口都失败，说明本机确实需要出站代理，请同时为更新器和 Docker 守护进程配置代理。"
    echo "「所选镜像源 吞吐」失败不影响更新：该镜像拉不动时更新器会自动改用 ghcr.io 同一 digest。"
    echo "「ghcr.io 直连 吞吐」失败才是硬问题——兜底来源也拿不到层，更新无法完成。"
    echo "镜像清单或 Docker 空间失败必须先处理再更新。"
    exit 1
fi
echo "三类出口均可连通。检测通过不等于大层可完整下载，正式更新仍会做完整拉取与摘要校验。"

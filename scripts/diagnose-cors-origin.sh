#!/usr/bin/env bash
# 只读诊断：不修改任何配置，判断「不允许的跨域来源」是 scheme 丢失、白名单缺失还是信任边界问题。
# 用法：
#   SITE_URL=https://canvas.example.com bash scripts/diagnose-cors-origin.sh
set -uo pipefail

SITE_URL="${SITE_URL:-}"
[[ -n "$SITE_URL" ]] || {
    echo "请指定对外站点来源，例如：SITE_URL=https://canvas.example.com bash scripts/diagnose-cors-origin.sh" >&2
    exit 2
}
SITE_URL="${SITE_URL%/}"
# 无鉴权的公开接口，只经过全局 CORS 中间件，不会产生业务副作用。
PROBE_PATH="${PROBE_PATH:-/api/auth/settings}"

[[ "$SITE_URL" == http://* || "$SITE_URL" == https://* ]] || {
    echo "SITE_URL 必须是 http(s):// 开头的完整来源（不要带路径）：${SITE_URL}" >&2
    exit 2
}

ok=0
bad=0
hint=""

report() {
    local name="$1" state="$2" detail="$3"
    if [[ "$state" == "ok" ]]; then
        printf '  [通过] %-34s %s\n' "$name" "$detail"
        ok=$((ok + 1))
    else
        printf '  [失败] %-34s %s\n' "$name" "$detail"
        bad=$((bad + 1))
    fi
}

# 返回 "状态码 允许来源 Set-Cookie-Secure"
probe() {
    local origin="$1" extra=("${@:2}")
    curl -sS -o /dev/null -D - --max-time "${PROBE_TIMEOUT:-15}" \
        ${origin:+-H "Origin: ${origin}"} ${extra[@]+"${extra[@]}"} \
        "${SITE_URL}${PROBE_PATH}" 2>/dev/null |
        awk '
            BEGIN { code="000"; allow=""; secure="none" }
            /^HTTP\// { code=$2 }
            tolower($1)=="access-control-allow-origin:" { allow=$2 }
            tolower($1)=="set-cookie:" && tolower($0) ~ /canvas|session/ { secure = (tolower($0) ~ /secure/) ? "yes" : "no" }
            END { gsub(/\r/, "", allow); gsub(/\r/, "", secure); printf "%s %s %s", code, (allow == "" ? "-" : allow), secure }
        '
}

echo "跨域来源诊断（只读）"
echo "  站点: ${SITE_URL}"
echo "  探测: ${PROBE_PATH}"
echo

echo "1. 浏览器真实来源"
read -r code allow secure <<<"$(probe "$SITE_URL")"
if [[ "$code" == "200" ]]; then
    report "Origin ${SITE_URL}" ok "${code} allow-origin=${allow} cookie-secure=${secure}"
    [[ "$secure" == "no" ]] && hint="回源未下发 X-Forwarded-Proto: https（会话 Cookie 缺 Secure）"
elif [[ "$code" == "403" ]]; then
    report "Origin ${SITE_URL}" fail "${code}（同源判定未命中，见下方第 2 项）"
else
    report "Origin ${SITE_URL}" fail "${code}（非 403 时先排查接口与登录态）"
fi

echo "2. 强制 https 回源头（模拟边缘下发 X-Forwarded-Proto: https）"
read -r code allow _ <<<"$(probe "$SITE_URL" -H "X-Forwarded-Proto: https")"
if [[ "$code" == "200" ]]; then
    report "Origin + X-Forwarded-Proto: https" ok "${code} allow-origin=${allow}"
    hint="边缘未下发 X-Forwarded-Proto；同源判定实际按 http 重建，需在 CDN/ESA 回源请求头补 https"
elif [[ "$code" == "403" ]]; then
    report "Origin + X-Forwarded-Proto: https" fail "${code}（白名单也没写，需在 .env 补 https Origin）"
else
    report "Origin + X-Forwarded-Proto: https" ok "${code}（已放行，转发头不参与判定）"
fi

echo "3. 未授权来源必须拒绝"
read -r code _ _ <<<"$(probe "https://cors-probe.invalid")"
if [[ "$code" == "403" ]]; then
    report "Origin https://cors-probe.invalid" ok "${code}"
else
    report "Origin https://cors-probe.invalid" fail "${code}（未授权来源未被拒绝，检查 CANVAS_CORS_ORIGINS 是否用了 *）"
fi

echo "4. 同主机其他端口必须拒绝"
read -r code _ _ <<<"$(probe "${SITE_URL}:8443")"
if [[ "$code" == "403" ]]; then
    report "Origin ${SITE_URL}:8443" ok "${code}"
else
    report "Origin ${SITE_URL}:8443" fail "${code}（同主机其他端口被放行，Cookie 不按端口隔离）"
fi

echo
printf '通过 %d 项，失败 %d 项\n' "$ok" "$bad"
if [[ -n "$hint" ]]; then
    printf '提示：%s\n' "$hint"
fi
[[ "$bad" -eq 0 ]]

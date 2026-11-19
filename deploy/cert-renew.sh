#!/usr/bin/env bash
# 为公网 IP 签发 / 续期 Let's Encrypt 短期证书（shortlived profile，约 6 天有效）。
# 用法：
#   ./cert-renew.sh            首次签发或按需续期（剩余不足 3 天才续）
#   ./cert-renew.sh --self-signed   签不到 IP 证书时，生成自签名证书兜底
# 建议 crontab：17 */12 * * * /opt/nex_call/deploy/cert-renew.sh >> /var/log/nexcall-cert.log 2>&1
set -euo pipefail

cd "$(dirname "$0")"
set -a
# shellcheck disable=SC1091
source ./.env
set +a

: "${PUBLIC_IP:?PUBLIC_IP 未设置}"
: "${ACME_EMAIL:?ACME_EMAIL 未设置}"

CERT_DIR=./certs
ACME_DIR=./acme
LEGO_DIR=./lego
# lego v5 的命令行和证书目录布局不同，这里锁定 v4
LEGO_IMAGE=${LEGO_IMAGE:-goacme/lego:v4.28.1}
RENEW_DAYS=${RENEW_DAYS:-3}

mkdir -p "$CERT_DIR" "$ACME_DIR" "$LEGO_DIR"

log() { echo "[$(date '+%F %T')] $*"; }

reload_caddy() {
  if docker compose ps --status running caddy 2>/dev/null | grep -q caddy; then
    docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile --force
    log "Caddy 已重新加载证书"
  fi
}

self_signed() {
  log "生成自签名证书（有效期 365 天）: $PUBLIC_IP"
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -subj "/CN=$PUBLIC_IP" \
    -addext "subjectAltName=IP:$PUBLIC_IP" \
    -keyout "$CERT_DIR/privkey.pem" -out "$CERT_DIR/fullchain.pem" 2>/dev/null
  chmod 600 "$CERT_DIR/privkey.pem"
}

if [[ "${1:-}" == "--self-signed" ]]; then
  self_signed
  reload_caddy
  exit 0
fi

# Caddy 启动时必须能读到证书文件，首次部署先放一张自签名占位证书，
# 等 Caddy 跑起来提供 HTTP-01 验证目录后再换成正式证书。
if [[ ! -s "$CERT_DIR/fullchain.pem" ]]; then
  self_signed
fi

if ! docker compose ps --status running caddy 2>/dev/null | grep -q caddy; then
  log "启动 Caddy 以提供 HTTP-01 验证"
  docker compose up -d caddy
  sleep 3
fi

lego() {
  # 镜像里 /lego 是入口二进制，数据目录必须挂 /.lego
  # Let's Encrypt 拒绝把 IP 写进 CSR 的 CN，IP 证书必须 --disable-cn
  docker run --rm \
    -v "$(pwd)/$LEGO_DIR:/.lego" \
    -v "$(pwd)/$ACME_DIR:/srv/acme" \
    "$LEGO_IMAGE" \
    --path /.lego \
    --email "$ACME_EMAIL" \
    --accept-tos \
    --domains "$PUBLIC_IP" \
    --disable-cn \
    --http --http.webroot /srv/acme \
    "$@"
}

LEGO_CRT="$LEGO_DIR/certificates/$PUBLIC_IP.crt"
LEGO_KEY="$LEGO_DIR/certificates/$PUBLIC_IP.key"

if [[ -s "$LEGO_CRT" ]]; then
  log "检查续期（剩余不足 $RENEW_DAYS 天则续期）"
  lego renew --profile shortlived --days "$RENEW_DAYS"
else
  log "首次签发 IP 证书: $PUBLIC_IP"
  lego run --profile shortlived
fi

if [[ ! -s "$LEGO_CRT" ]]; then
  log "签发失败，可执行 ./cert-renew.sh --self-signed 使用自签名证书兜底"
  exit 1
fi

if ! cmp -s "$LEGO_CRT" "$CERT_DIR/fullchain.pem"; then
  cp "$LEGO_CRT" "$CERT_DIR/fullchain.pem"
  cp "$LEGO_KEY" "$CERT_DIR/privkey.pem"
  chmod 600 "$CERT_DIR/privkey.pem"
  log "证书已更新：$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -enddate)"
  reload_caddy
else
  log "证书无变化"
fi

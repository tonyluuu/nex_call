#!/usr/bin/env bash
# 在香橙派上安装 / 升级 device-agent 并注册为 systemd 服务。
# 用法：./install.sh <device-agent 可执行文件路径>
set -euo pipefail

BIN=${1:-./device-agent}
HERE=$(cd "$(dirname "$0")" && pwd)
RUN_USER=${SUDO_USER:-$(id -un)}
RUN_UID=$(id -u "$RUN_USER")

[[ -x "$BIN" ]] || { echo "找不到可执行文件: $BIN"; exit 1; }

sudo install -m 755 "$BIN" /usr/local/bin/device-agent

sudo mkdir -p /etc/nexcall
if [[ ! -f /etc/nexcall/device.env ]]; then
  sudo install -m 600 -o "$RUN_USER" "$HERE/device.env.example" /etc/nexcall/device.env
  echo "已生成 /etc/nexcall/device.env，请先填写 SERVER_URL / DEVICE_ID / DEVICE_SECRET"
fi

sed -e "s/^User=.*/User=$RUN_USER/" \
    -e "s#/run/user/1000#/run/user/$RUN_UID#g" \
    "$HERE/device-agent.service" | sudo tee /etc/systemd/system/device-agent.service >/dev/null

sudo systemctl daemon-reload
sudo systemctl enable device-agent
if grep -q '^DEVICE_SECRET=.\+' /etc/nexcall/device.env; then
  sudo systemctl restart device-agent
  echo "device-agent 已启动，查看日志：journalctl -u device-agent -f"
else
  echo "填写配置后执行：sudo systemctl restart device-agent"
fi

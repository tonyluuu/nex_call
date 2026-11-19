#!/usr/bin/env bash
# 香橙派 Zero2（Armbian / Debian bookworm, arm64）一次性初始化。
# 以普通用户（例如 orangepi）执行：./setup.sh
set -euo pipefail

GO_VERSION=${GO_VERSION:-1.26.0}
RUN_USER=$(id -un)
HERE=$(cd "$(dirname "$0")" && pwd)

echo "==> 安装系统依赖"
sudo apt-get update
sudo apt-get install -y \
  pipewire pipewire-pulse pipewire-alsa wireplumber libspa-0.2-modules \
  pulseaudio-utils alsa-utils \
  libopus-dev libopusfile-dev pkg-config build-essential git curl

echo "==> 安装 Go $GO_VERSION（Debian 自带的 Go 版本太旧）"
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go$GO_VERSION"; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tgz
  rm -f /tmp/go.tgz
fi
grep -q '/usr/local/go/bin' ~/.profile || echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile

echo "==> 启用 PipeWire 用户服务，并允许开机无登录运行"
systemctl --user enable --now pipewire pipewire-pulse wireplumber
sudo loginctl enable-linger "$RUN_USER"

echo "==> 安装回声消除配置"
mkdir -p ~/.config/pipewire/pipewire.conf.d
cp "$HERE/pipewire-echo-cancel.conf" ~/.config/pipewire/pipewire.conf.d/60-echo-cancel.conf
systemctl --user restart pipewire pipewire-pulse wireplumber
sleep 2

echo "==> 将回声消除设备设为默认"
pactl set-default-source echo-cancel-source
pactl set-default-sink echo-cancel-sink

echo "==> GPIO 访问权限"
sudo usermod -aG gpio "$RUN_USER" 2>/dev/null || {
  sudo groupadd -f gpio
  sudo usermod -aG gpio "$RUN_USER"
  echo 'SUBSYSTEM=="gpio", KERNEL=="gpiochip*", GROUP="gpio", MODE="0660"' | sudo tee /etc/udev/rules.d/60-gpio.rules >/dev/null
  sudo udevadm control --reload-rules && sudo udevadm trigger
}

echo
echo "完成。下一步："
echo "  1. 重新登录一次让 gpio 用户组生效"
echo "  2. 运行 ./audio-test.sh 检查采集、播放和回声消除"
echo "  3. 参考 README 编译并安装 device-agent"

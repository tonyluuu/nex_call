#!/usr/bin/env bash
# 香橙派 arm64 一次性初始化。以普通用户执行：./setup.sh
# - Debian bookworm / Armbian：PipeWire + WebRTC 回声消除
# - Ubuntu 18.04 等没有 PipeWire 的系统：PulseAudio module-echo-cancel
set -euo pipefail

RUN_USER=$(id -un)
HERE=$(cd "$(dirname "$0")" && pwd)
GO_VERSION=${GO_VERSION:-$(awk '/^go / {print $2; exit}' "$HERE/../../device/go.mod")}
GO_VERSION=${GO_VERSION:-1.26.8}

if [[ "$(uname -m)" != "aarch64" ]]; then
  echo "device-agent 只提供 aarch64 构建，当前架构是 $(uname -m)"
  exit 1
fi

pkg_exists() {
  apt-cache show "$1" 2>/dev/null | grep -q "^Package: $1$"
}

ensure_user_runtime() {
  local uid="${1}"
  export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/${uid}}"
  sudo loginctl enable-linger "$RUN_USER"
  if [[ ! -d "$XDG_RUNTIME_DIR" ]]; then
    sudo systemctl start "user@${uid}.service"
    sleep 1
  fi
  if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" && -S "${XDG_RUNTIME_DIR}/bus" ]]; then
    export DBUS_SESSION_BUS_ADDRESS="unix:path=${XDG_RUNTIME_DIR}/bus"
  fi
}

opus_has_in_dtx() {
  pkg-config --exists opus || return 1
  local cflags
  cflags=$(pkg-config --cflags opus) || return 1
  # shellcheck disable=SC2086
  printf '#include <opus.h>\n' | gcc -E -dM $cflags - 2>/dev/null | grep -q 'OPUS_GET_IN_DTX' || return 1
}

# Ubuntu 18.04 的 libopus 1.1.2 没有 OPUS_GET_IN_DTX，device-agent 链不过。
# 装一份 1.3.1 静态库到 /usr/local，供在派上直接编译时使用。
install_opus_if_old() {
  if opus_has_in_dtx; then
    return
  fi
  local ver=1.3.1 url ok=0
  echo "==> 系统 libopus 过旧，编译安装 ${ver}"
  for url in \
    "https://github.com/xiph/opus/archive/refs/tags/v${ver}.tar.gz"
  do
    if curl -fL --retry 2 --connect-timeout 15 --max-time 120 -o /tmp/opus.tar.gz "$url"; then
      ok=1
      break
    fi
  done
  [[ "$ok" == 1 ]] || { echo "下载 libopus ${ver} 失败"; exit 1; }
  rm -rf /tmp/opus-src
  mkdir -p /tmp/opus-src
  tar -C /tmp/opus-src --strip-components=1 -xzf /tmp/opus.tar.gz
  sudo apt-get install -y autoconf automake libtool
  (
    cd /tmp/opus-src
    ./autogen.sh
    ./configure --disable-shared --enable-static --prefix=/usr/local --disable-doc
    make -j"$(nproc)"
    sudo make install
  )
  local pc libdir
  pc=$(find /usr/local -name opus.pc | head -1)
  libdir=$(PKG_CONFIG_PATH="$(dirname "$pc")" pkg-config --variable=libdir opus)
  sudo sed -i "s|^Libs:.*|Libs: ${libdir}/libopus.a -lm|" "$pc"
  grep -q '/usr/local/lib/pkgconfig' ~/.profile || echo 'export PKG_CONFIG_PATH=/usr/local/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}' >> ~/.profile
  export PKG_CONFIG_PATH="/usr/local/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
  rm -rf /tmp/opus-src /tmp/opus.tar.gz
}

install_go() {
  echo "==> 安装 Go ${GO_VERSION}（发行版自带的 Go 版本太旧）"
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    local url ok=0
    for url in \
      "https://mirrors.aliyun.com/golang/go${GO_VERSION}.linux-arm64.tar.gz" \
      "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz"
    do
      echo "    下载 ${url}"
      if curl -fL --retry 2 --connect-timeout 15 --max-time 180 -o /tmp/go.tgz "$url"; then
        ok=1
        break
      fi
    done
    [[ "$ok" == 1 ]] || { echo "下载 Go ${GO_VERSION} 失败"; exit 1; }
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tgz
    rm -f /tmp/go.tgz
  fi
  /usr/local/go/bin/go version
  grep -q '/usr/local/go/bin' ~/.profile || echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile
  /usr/local/go/bin/go env -w GOPROXY=https://goproxy.cn,direct
  /usr/local/go/bin/go env -w GOSUMDB=sum.golang.google.cn
}

setup_pipewire() {
  local uid
  uid=$(id -u)
  echo "==> 安装 PipeWire"
  sudo apt-get install -y \
    pipewire pipewire-pulse pipewire-alsa wireplumber libspa-0.2-modules \
    pulseaudio-utils alsa-utils \
    libopus-dev libopusfile-dev pkg-config build-essential git curl ca-certificates

  install_go
  ensure_user_runtime "$uid"

  echo "==> 启用 PipeWire 用户服务"
  systemctl --user enable --now pipewire pipewire-pulse wireplumber

  echo "==> 安装回声消除配置"
  mkdir -p ~/.config/pipewire/pipewire.conf.d
  cp "$HERE/pipewire-echo-cancel.conf" ~/.config/pipewire/pipewire.conf.d/60-echo-cancel.conf
  systemctl --user restart pipewire pipewire-pulse wireplumber
  sleep 2
  pactl set-default-source echo-cancel-source
  pactl set-default-sink echo-cancel-sink
}

setup_pulse() {
  local uid
  uid=$(id -u)
  echo "==> 当前系统没有 PipeWire，改用 PulseAudio 回声消除"
  sudo apt-get install -y \
    pulseaudio pulseaudio-utils alsa-utils libasound2-plugins libpam-systemd \
    libopus0 libopus-dev pkg-config build-essential git curl ca-certificates

  install_go
  ensure_user_runtime "$uid"

  echo "==> 安装 PulseAudio 回声消除配置"
  mkdir -p ~/.config/pulse
  cp "$HERE/pulse-echo-cancel.pa" ~/.config/pulse/default.pa

  if systemctl --user list-unit-files 'pulseaudio.service' --no-legend 2>/dev/null | grep -q pulseaudio.service; then
    printf '%s\n' '# 由 setup.sh 写入，交给 systemd 用户服务拉起，避免 autospawn 抢套接字。' 'autospawn = no' > ~/.config/pulse/client.conf
    systemctl --user enable pulseaudio.socket pulseaudio.service || true
    pulseaudio -k >/dev/null 2>&1 || true
    sleep 1
    if ! systemctl --user restart pulseaudio.socket pulseaudio.service; then
      echo "systemd 没有拉起 PulseAudio，改为直接启动守护进程。"
      pulseaudio --start
    fi
  else
    echo "没有 pulseaudio 用户服务，改为直接启动守护进程。重新登录后再执行一次 ./setup.sh，以便开机自动拉起音频。"
    pulseaudio -k >/dev/null 2>&1 || true
    pulseaudio --start
  fi

  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    pactl info >/dev/null 2>&1 && break
    sleep 1
  done
  if ! pactl info >/dev/null 2>&1; then
    echo "PulseAudio 没有起来。查看：journalctl --user -u pulseaudio -n 50"
    exit 1
  fi

  if ! pactl list short sources | grep -q echo-cancel-source; then
    echo "配置文件未加载回声消除模块，改为手动加载"
    if ! pactl load-module module-echo-cancel use_master_format=1 aec_method=webrtc \
      source_name=echo-cancel-source sink_name=echo-cancel-sink; then
      echo "加载 module-echo-cancel 失败。先接上 USB 声卡或 USB 麦克风，再重新执行 ./setup.sh"
      echo "当前 source："
      pactl list short sources || true
      echo "当前 sink："
      pactl list short sinks || true
      exit 1
    fi
  fi
  if ! pactl list short sources | grep -q echo-cancel-source; then
    echo "未生成 echo-cancel-source。先接上 USB 声卡或 USB 麦克风再重新执行 ./setup.sh"
    echo "当前 source："
    pactl list short sources || true
    echo "当前 sink："
    pactl list short sinks || true
    exit 1
  fi
  pactl set-default-source echo-cancel-source
  pactl set-default-sink echo-cancel-sink
}

echo "==> 安装系统依赖"
sudo apt-get update
if pkg_exists pipewire && pkg_exists wireplumber && pkg_exists libspa-0.2-modules; then
  setup_pipewire
else
  setup_pulse
fi
install_opus_if_old

echo "==> GPIO 访问权限"
sudo usermod -aG gpio "$RUN_USER" 2>/dev/null || {
  sudo groupadd -f gpio
  sudo usermod -aG gpio "$RUN_USER"
  echo 'SUBSYSTEM=="gpio", KERNEL=="gpiochip*", GROUP="gpio", MODE="0660"' | sudo tee /etc/udev/rules.d/60-gpio.rules >/dev/null
  sudo udevadm control --reload-rules && sudo udevadm trigger
}

echo
echo "完成。下一步："
echo "  1. 重新登录一次，让 gpio 组和用户会话生效"
echo "  2. 运行 ./audio-test.sh 检查采集、播放和回声消除"
echo "  3. 参考 README 编译并安装 device-agent"
echo "     当前 shell 可先执行：export PATH=\$PATH:/usr/local/go/bin"

#!/usr/bin/env bash
# 在香橙派上安装 / 升级网页音乐播放器并注册为 systemd 服务。
# 用法：./install.sh（以运行 PulseAudio 的用户执行，需要 sudo）
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RUN_USER=${SUDO_USER:-$(id -un)}
RUN_UID=$(id -u "$RUN_USER")

if ! command -v mpg123 >/dev/null 2>&1; then
  echo "==> 安装 mpg123"
  sudo apt-get install -y mpg123
fi
if ! command -v parec >/dev/null 2>&1 || ! command -v paplay >/dev/null 2>&1; then
  echo "==> 安装 pulseaudio-utils（录音和播放）"
  sudo apt-get install -y pulseaudio-utils
fi
if ! command -v speaker-test >/dev/null 2>&1; then
  echo "==> 安装 alsa-utils（测试音）"
  sudo apt-get install -y alsa-utils
fi
# Ubuntu 18.04 自带的 1.49 会把汉字拆成多余的音节，普通话听着不像话。
espeak_ver=""
if command -v espeak-ng >/dev/null 2>&1; then
  espeak_ver=$(espeak-ng --version | awk '{print $NF}')
fi
if [ -z "$espeak_ver" ] || [ "$(printf '%s\n' "$espeak_ver" 1.52.0 | sort -V | head -1)" != "1.52.0" ]; then
  echo "==> 安装 espeak-ng 1.52（普通话提示音）"
  sudo apt-get install -y espeak-ng autoconf automake libtool pkg-config libsonic-dev make gcc
  tmp=$(mktemp -d)
  curl -fL --retry 3 --connect-timeout 20 --max-time 1800 -o "$tmp/espeak.tar.gz" \
    https://codeload.github.com/espeak-ng/espeak-ng/tar.gz/refs/tags/1.52.0
  tar -C "$tmp" -xzf "$tmp/espeak.tar.gz"
  (
    cd "$tmp/espeak-ng-1.52.0"
    ./autogen.sh
    ./configure --prefix=/usr/local --without-pcaudio --with-extdict-cmn \
      LDFLAGS="-Wl,-rpath,/usr/local/lib"
    make -j2
    sudo make install
  )
  sudo ldconfig
  rm -rf "$tmp"
fi
if ! python3 -c 'import flask' >/dev/null 2>&1; then
  echo "==> 安装 Flask"
  sudo apt-get install -y python3-flask
fi
if ! python3 -c 'import mutagen' >/dev/null 2>&1; then
  echo "==> 安装 mutagen（读取歌曲标签和封面）"
  sudo apt-get install -y python3-mutagen
fi

echo "==> 复制程序到 /opt/nexcall-music"
sudo mkdir -p /opt/nexcall-music/templates /opt/nexcall-music/sounds /var/lib/nexcall-music/recordings
sudo install -m 644 "$HERE/app.py" /opt/nexcall-music/app.py
sudo install -m 644 "$HERE/announce.py" /opt/nexcall-music/announce.py
sudo install -m 644 "$HERE/levels.py" /opt/nexcall-music/levels.py
sudo install -m 644 "$HERE/record.py" /opt/nexcall-music/record.py
sudo install -m 644 "$HERE/templates/index.html" /opt/nexcall-music/templates/index.html
sudo install -m 644 "$HERE"/sounds/*.wav /opt/nexcall-music/sounds/
sudo chown -R "$RUN_USER" /var/lib/nexcall-music

sed -e "s/^User=.*/User=$RUN_USER/" \
    -e "s#/run/user/1000#/run/user/$RUN_UID#g" \
    "$HERE/nexcall-music.service" | sudo tee /etc/systemd/system/nexcall-music.service >/dev/null

sudo systemctl daemon-reload
sudo systemctl enable nexcall-music
sudo systemctl restart nexcall-music

ip=$(hostname -I 2>/dev/null | awk '{print $1}')
echo "已启动。浏览器打开 http://${ip:-<香橙派IP>}:8090"
echo "查看日志：journalctl -u nexcall-music -f"

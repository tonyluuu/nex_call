#!/usr/bin/env bash
# 香橙派音频自检：设备枚举、录音回放、回声消除对比。
# PipeWire 用 pw-play / pw-record；Ubuntu 18.04 的 PulseAudio 11 用 paplay / parec。
set -euo pipefail

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

step() { echo; echo "==> $*"; }

default_of() {
  local label=$1 cmd=$2
  if pactl "$cmd" >/dev/null 2>&1; then
    pactl "$cmd"
    return
  fi
  pactl info | awk -v k="$label" 'index($0, k ": ")==1 {print substr($0, length(k)+3)}'
}

play_to() {
  local sink=$1 file=$2
  if command -v pw-play >/dev/null 2>&1; then
    pw-play --target "$sink" "$file"
  else
    paplay --device="$sink" "$file"
  fi
}

start_record() {
  local source=$1 file=$2
  if command -v pw-record >/dev/null 2>&1; then
    pw-record --target "$source" --rate 48000 --channels 1 "$file" &
  else
    parec --device="$source" --rate=48000 --channels=1 --file-format=wav >"$file" &
  fi
}

step "音频设备（应包含 echo-cancel-source / echo-cancel-sink）"
pactl list short sources
pactl list short sinks
echo "默认 source: $(default_of "Default Source" get-default-source)"
echo "默认 sink:   $(default_of "Default Sink" get-default-sink)"

if ! pactl list short sources | grep -q echo-cancel-source; then
  echo "未找到 echo-cancel-source。"
  echo "PipeWire：检查 ~/.config/pipewire/pipewire.conf.d/60-echo-cancel.conf"
  echo "          日志：journalctl --user -u pipewire -n 50"
  echo "PulseAudio：检查 ~/.config/pulse/default.pa"
  echo "            日志：journalctl --user -u pulseaudio -n 50"
  exit 1
fi

step "播放测试音（1 秒 440Hz），应能从喇叭听到"
play_to echo-cancel-sink /usr/share/sounds/alsa/Front_Center.wav 2>/dev/null \
  || speaker-test -t sine -f 440 -l 1 -c 1 >/dev/null 2>&1 || true

step "录音 5 秒（对着麦克风说话）"
start_record echo-cancel-source "$TMP/mic.wav"
REC=$!
sleep 5
kill -INT "$REC" 2>/dev/null || true
wait "$REC" 2>/dev/null || true

step "回放刚才的录音"
play_to echo-cancel-sink "$TMP/mic.wav"

step "回声消除对比：播放音乐的同时录音 6 秒，请保持安静"
RAW_SRC=$(pactl list short sources | awk '!/echo-cancel|monitor/ {print $2; exit}')
start_record echo-cancel-source "$TMP/aec.wav"
R1=$!
start_record "$RAW_SRC" "$TMP/raw.wav"
R2=$!
speaker-test -D pulse -t sine -f 600 -c 1 -l 3 >/dev/null 2>&1 || true
sleep 1
kill -INT "$R1" "$R2" 2>/dev/null || true
wait "$R1" "$R2" 2>/dev/null || true

step "先播放原始麦克风录音（$RAW_SRC，应能听到明显的测试音）"
play_to echo-cancel-sink "$TMP/raw.wav"
step "再播放回声消除后的录音（测试音应明显减弱或消失）"
play_to echo-cancel-sink "$TMP/aec.wav"

echo
echo "如果回声消除效果不理想：降低喇叭音量（alsamixer）、拉开喇叭与麦克风距离，"
echo "或换用带硬件回声消除的 USB 会议麦克风。"

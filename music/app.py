#!/usr/bin/env python3
"""香橙派网页音乐播放器：上传 MP3，用 mpg123 播放到 PulseAudio 默认输出。

NexCall 通话开始时自动暂停，挂断后不自动恢复。需兼容 Ubuntu 18.04 的 Python 3.6。
"""
import glob
import json
import logging
import os
import random
import shutil
import subprocess
import threading
import time

from flask import Flask, Response, jsonify, render_template, request, send_file

from announce import AnnounceError, SOUNDS, clean_sound, speak
from levels import (
    AEC_KEYS,
    Pactl,
    echo_masters,
    format_aec,
    parse_aec,
    parse_list,
    pactl_run,
    rewrite_echo_line,
    supported_aec_keys,
)
from record import AEC_SINK, AEC_SOURCE, RAW_SOURCE, MicMeter, RecordError, Recorder, source_names

try:
    from mutagen.mp3 import MP3
except ImportError:  # 没装 python3-mutagen 时只显示文件名和大小
    MP3 = None

MUSIC_DIR = os.environ.get("MUSIC_DIR", "/var/lib/nexcall-music")
REC_DIR = os.path.join(MUSIC_DIR, "recordings")
PA_CONFIG = os.path.expanduser("~/.config/pulse/default.pa")
MUSIC_PORT = int(os.environ.get("MUSIC_PORT", "8090"))
CALL_STATE_FILE = os.environ.get("CALL_STATE_FILE", "/run/nexcall/call-state")
MAX_UPLOAD_MB = int(os.environ.get("MAX_UPLOAD_MB", "50"))
SETTINGS_FILE = os.path.join(MUSIC_DIR, ".settings.json")
MODES = ("sequential", "shuffle", "repeat_one")
HISTORY_LIMIT = 50
MIN_PLAY_SECONDS = 0.5

log = logging.getLogger("nexcall-music")


def load_settings():
    try:
        with open(SETTINGS_FILE, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_settings(updates):
    """合并写入，只覆盖传进来的键。"""
    data = load_settings()
    data.update(updates)
    tmp = SETTINGS_FILE + ".tmp"
    try:
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(data, f)
        os.replace(tmp, SETTINGS_FILE)
    except OSError as e:
        log.warning("save settings: %s", e)


class Player:
    """常驻一个 mpg123 -R 子进程，通过标准输入发命令。"""

    def __init__(self):
        self.lock = threading.Lock()
        self.proc = None
        self.track = ""
        self.status = "stopped"  # stopped / playing / paused
        self.call_paused = False
        settings = load_settings()
        self.volume = clamp_volume(settings.get("volume", 100))
        self.mode = settings.get("mode") if settings.get("mode") in MODES else "sequential"
        self.history = []  # 播过的曲目，随机模式下「上一曲」从这里往回走
        self.rate = 0  # 当前曲目的采样率，来自 @S
        self.position = 0.0
        self.duration = 0.0
        self.position_at = time.time()
        self.awaiting_start = False
        self.started_at = 0.0
        threading.Thread(target=self._poll_position, daemon=True).start()

    def _ensure(self):
        if self.proc is not None and self.proc.poll() is None:
            return
        self.proc = subprocess.Popen(
            ["mpg123", "-R", "-o", "pulse"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            universal_newlines=True,
            encoding="utf-8",
            bufsize=1,
        )
        self.status = "stopped"
        # 关掉每帧进度输出，只保留状态变化
        self._send("SILENCE")
        self._send("VOLUME %d" % self.volume)
        threading.Thread(target=self._read, args=(self.proc,), daemon=True).start()

    def _send(self, cmd):
        try:
            self.proc.stdin.write(cmd + "\n")
            self.proc.stdin.flush()
        except (BrokenPipeError, OSError) as e:
            log.warning("mpg123 command %r failed: %s", cmd, e)

    def _read(self, proc):
        for line in proc.stdout:
            parts = line.split()
            if not parts:
                continue
            with self.lock:
                if proc is not self.proc:
                    return
                self._handle(parts)
        log.warning("mpg123 exited with %s", proc.wait())
        with self.lock:
            if proc is self.proc:
                self.status = "stopped"

    def _handle(self, parts):
        kind = parts[0]
        if kind == "@E":
            # 不能据此判断打开失败：播完瞬间送达的 SAMPLE 也会回 @E No stream opened
            log.warning("mpg123: %s", " ".join(parts))
        elif kind == "@S" and len(parts) > 3:
            # @S <MPEG版本> <layer> <采样率> ...
            self.rate = int(parts[3])
        elif kind == "@SAMPLE" and len(parts) == 3 and self.rate:
            self.position = int(parts[1]) / self.rate
            self.duration = int(parts[2]) / self.rate
            self.position_at = time.time()
        elif kind == "@P" and len(parts) > 1:
            code = parts[1]
            if code == "1":
                self.status = "paused"
            elif code == "2":
                self.status = "playing"
                if self.awaiting_start:
                    self.awaiting_start = False
                    self.started_at = time.time()
            elif code == "0":
                # 手动停止时 status 已是 stopped；LOAD 后、新曲目的 @P 2 之前收到的 @P 0
                # 属于上一首或打开失败（打不开的文件不会有 @P 2），都不算播完
                natural_end = self.status == "playing" and not self.awaiting_start
                self.status = "stopped"
                if natural_end and time.time() - self.started_at < MIN_PLAY_SECONDS:
                    # 损坏或空文件一开始就结束，继续自动切歌会在单曲循环下空转
                    log.warning("%s ended right after start, auto-advance stopped", self.track)
                    natural_end = False
                if natural_end:
                    if self.mode == "repeat_one":
                        nxt = self.track if self.track in list_tracks() else ""
                    elif self.mode == "shuffle":
                        nxt = random_track(self.track)
                    else:
                        nxt = neighbor(self.track, 1, wrap=False)
                    if nxt:
                        log.info("track finished (%s), next: %s", self.mode, nxt)
                        self._load(nxt)

    def _poll_position(self):
        while True:
            time.sleep(0.5)
            with self.lock:
                if self.status == "playing" and self.proc is not None and self.proc.poll() is None:
                    self._send("SAMPLE")

    def _load(self, name, remember=True):
        self._ensure()
        if remember and self.track and self.track != name:
            self.history = (self.history + [self.track])[-HISTORY_LIMIT:]
        self.track = name
        info = track_info(name)
        self.duration = info["duration"] if info else 0.0
        self.position = 0.0
        self.position_at = time.time()
        self.awaiting_start = True
        self._send("LOAD " + os.path.join(MUSIC_DIR, name))
        self.status = "playing"

    def play(self, name):
        with self.lock:
            self.call_paused = False
            if name == self.track and self.status == "paused":
                self._ensure()
                self._send("PAUSE")
                self.status = "playing"
                self.position_at = time.time()
                return
            self._load(name)

    def skip(self, step):
        """上一曲 / 下一曲。随机模式下一曲随机选、上一曲沿历史返回；其他模式按列表循环。"""
        with self.lock:
            self.call_paused = False
            if self.mode == "shuffle" and step > 0:
                nxt = random_track(self.track)
            elif self.mode == "shuffle":
                tracks = list_tracks()
                while self.history and self.history[-1] not in tracks:
                    self.history.pop()
                if self.history:
                    self._load(self.history.pop(), remember=False)
                    return
                nxt = neighbor(self.track, step, wrap=True)
            else:
                nxt = neighbor(self.track, step, wrap=True)
            if nxt:
                self._load(nxt)

    def set_mode(self, mode):
        with self.lock:
            self.mode = mode
            save_settings({"mode": mode})

    def seek(self, seconds):
        with self.lock:
            if self.status == "stopped" or self.proc is None or self.proc.poll() is not None:
                return
            seconds = max(0.0, seconds)
            if self.duration:
                seconds = min(seconds, max(0.0, self.duration - 0.5))
            self._send("JUMP %.2fs" % seconds)
            self.position = seconds
            self.position_at = time.time()

    def forget(self, name):
        """删除曲目前调用：正在播放或暂停在这首时先停掉。"""
        with self.lock:
            if name != self.track:
                return
            if self.proc is not None and self.proc.poll() is None:
                self._send("STOP")
            self.status = "stopped"
            self.track = ""
            self.call_paused = False

    def toggle(self):
        with self.lock:
            self.call_paused = False
            if self.status in ("playing", "paused"):
                self._ensure()
                self._send("PAUSE")
                self._freeze_position()
                self.status = "paused" if self.status == "playing" else "playing"

    def _freeze_position(self):
        """在播放 / 暂停切换时把推算出的位置落下来，之后从这里继续推算。"""
        self.position = self._current_position()
        self.position_at = time.time()

    def _current_position(self):
        pos = self.position
        if self.status == "playing":
            pos += time.time() - self.position_at
        if self.duration:
            pos = min(pos, self.duration)
        return pos

    def stop(self):
        with self.lock:
            self.call_paused = False
            if self.proc is not None and self.proc.poll() is None:
                self._send("STOP")
            self.status = "stopped"
            self.position = 0.0

    def pause_for_call(self):
        with self.lock:
            if self.status == "playing":
                self._send("PAUSE")
                self._freeze_position()
                self.status = "paused"
                self.call_paused = True
                log.info("call started, music paused")

    def pause_held(self):
        """录音或测试开始前暂停，结束后不自动继续。"""
        with self.lock:
            if self.status != "playing":
                return
            self._send("PAUSE")
            self._freeze_position()
            self.status = "paused"
            self.call_paused = False

    def set_volume(self, volume):
        with self.lock:
            self.volume = clamp_volume(volume)
            if self.proc is not None and self.proc.poll() is None:
                self._send("VOLUME %d" % self.volume)
            save_settings({"volume": self.volume})

    def snapshot(self):
        with self.lock:
            return {
                "track": self.track,
                "status": self.status,
                "callPaused": self.call_paused,
                "volume": self.volume,
                "mode": self.mode,
                "position": round(self._current_position(), 2) if self.status != "stopped" else 0,
                "duration": round(self.duration, 2),
            }


def neighbor(current, step, wrap):
    """按列表顺序找相邻曲目；当前曲目不在列表里时，下一曲从头、上一曲从尾开始。"""
    tracks = list_tracks()
    if not tracks:
        return ""
    if current not in tracks:
        return tracks[0] if step > 0 else tracks[-1]
    i = tracks.index(current) + step
    if 0 <= i < len(tracks):
        return tracks[i]
    return tracks[i % len(tracks)] if wrap else ""


def random_track(current):
    """随机选一首，尽量避开当前这首；只有一首时就是它。"""
    tracks = list_tracks()
    others = [t for t in tracks if t != current]
    return random.choice(others or tracks) if tracks else ""


def clamp_volume(v):
    try:
        return max(0, min(100, int(round(float(v)))))
    except (TypeError, ValueError):
        return 100


player = Player()


def read_call_state():
    try:
        with open(CALL_STATE_FILE, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return "idle"


def watch_calls():
    last = read_call_state()
    while True:
        time.sleep(1)
        state = read_call_state()
        if state == "active" and last != "active":
            player.pause_for_call()
            recorder.stop()
        last = state


def list_tracks():
    try:
        names = [n for n in os.listdir(MUSIC_DIR) if n.lower().endswith(".mp3")]
    except OSError:
        return []
    return sorted(names, key=str.lower)


def safe_name(raw):
    """保留中文等字符，去掉目录部分和会造成越界的写法。"""
    name = os.path.basename(raw.replace("\\", "/")).strip()
    name = "".join(c for c in name if c not in "\0\r\n").lstrip(".")
    return name


def unique_path(name):
    base, ext = os.path.splitext(name)
    path = os.path.join(MUSIC_DIR, name)
    n = 1
    while os.path.exists(path):
        path = os.path.join(MUSIC_DIR, "%s (%d)%s" % (base, n, ext))
        n += 1
    return path


def tag_text(tags, key):
    if tags is None:
        return ""
    for frame in tags.getall(key):
        text = "/".join(str(t) for t in frame.text if str(t).strip())
        # 不少国产 MP3 把 GBK 字节标成 latin-1，按 GBK 还原
        if getattr(frame, "encoding", None) == 0 and any(ord(c) > 127 for c in text):
            try:
                text = text.encode("latin-1").decode("gbk")
            except (UnicodeEncodeError, UnicodeDecodeError):
                pass
        if text.strip():
            return text.strip()
    return ""


def pick_cover(tags):
    if tags is None:
        return None
    pics = tags.getall("APIC")
    if not pics:
        return None
    # type 3 是正面封面
    return next((p for p in pics if p.type == 3), pics[0])


_info_cache = {}
_info_lock = threading.Lock()


def track_info(name):
    """读取文件大小、时长、码率和 ID3 标签，按修改时间缓存。"""
    path = os.path.join(MUSIC_DIR, name)
    try:
        st = os.stat(path)
    except OSError:
        return None
    key = (st.st_mtime, st.st_size)
    with _info_lock:
        hit = _info_cache.get(name)
        if hit and hit[0] == key:
            return hit[1]
    info = {
        "name": name,
        "title": os.path.splitext(name)[0],
        "artist": "",
        "album": "",
        "year": "",
        "genre": "",
        "duration": 0,
        "bitrate": 0,
        "sampleRate": 0,
        "channels": 0,
        "size": st.st_size,
        "cover": False,
        "version": int(st.st_mtime),
    }
    if MP3 is not None:
        try:
            audio = MP3(path)
            tags = audio.tags
            info.update(
                title=tag_text(tags, "TIT2") or info["title"],
                artist=tag_text(tags, "TPE1") or tag_text(tags, "TPE2"),
                album=tag_text(tags, "TALB"),
                year=tag_text(tags, "TDRC")[:4],
                genre=tag_text(tags, "TCON"),
                duration=round(audio.info.length or 0, 1),
                bitrate=int((audio.info.bitrate or 0) / 1000),
                sampleRate=audio.info.sample_rate or 0,
                channels=audio.info.channels or 0,
                cover=pick_cover(tags) is not None,
            )
        except Exception as e:  # 标签损坏不影响播放
            log.warning("read tags of %s: %s", name, e)
    with _info_lock:
        _info_cache[name] = (key, info)
    return info


def read_cover(name):
    if MP3 is None:
        return None
    try:
        pic = pick_cover(MP3(os.path.join(MUSIC_DIR, name)).tags)
    except Exception:
        return None
    if pic is None or not pic.data:
        return None
    mime = pic.mime if pic.mime.startswith("image/") else "image/jpeg"
    return pic.data, mime


app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = MAX_UPLOAD_MB * 1024 * 1024


@app.get("/")
def index():
    return render_template("index.html", max_mb=MAX_UPLOAD_MB)


@app.get("/api/state")
def state():
    data = player.snapshot()
    data["tracks"] = [i for i in map(track_info, list_tracks()) if i]
    data["call"] = read_call_state()
    data["storage"] = storage_usage(data["tracks"])
    data["recording"] = recorder.status()
    data["recordings"] = recorder.items()
    flags, ready, aec_error = current_aec()
    data["aec"] = {key: flags[key] for key in AEC_KEYS}
    data["aecSupported"] = sorted(aec_supported())
    data["aecReady"] = ready
    data["aecError"] = aec_error
    try:
        speaker, mic = Pactl().get()
        data["levels"] = {"speaker": speaker, "mic": mic}
        data["levelsError"] = ""
    except (RuntimeError, ValueError) as exc:
        data["levels"] = None
        data["levelsError"] = str(exc)
    data["announce"] = announce_config()
    return jsonify(data)


def announce_config():
    data = load_settings()
    enabled = data.get("announceEnabled", True)
    if not isinstance(enabled, bool):
        enabled = True
    return {
        "enabled": enabled,
        "sound": clean_sound(data.get("announceSound")),
        "sounds": [{"id": sound_id, "label": label} for sound_id, label in SOUNDS],
    }


def save_announce(enabled, sound):
    if not isinstance(enabled, bool):
        raise AnnounceError("enabled 需要是布尔值")
    save_settings({"announceEnabled": enabled, "announceSound": clean_sound(sound)})
    return announce_config()


def storage_usage(tracks):
    usage = {"dir": MUSIC_DIR, "music": sum(t["size"] for t in tracks)}
    try:
        disk = shutil.disk_usage(MUSIC_DIR)
        usage.update(total=disk.total, used=disk.used, free=disk.free)
    except OSError:
        pass
    return usage


def list_pulse_sources():
    try:
        return source_names(pactl_run(["list", "short", "sources"]))
    except (RuntimeError, OSError) as exc:
        log.warning("list sources: %s", exc)
        return []


recorder = Recorder(REC_DIR, lambda: read_call_state() == "active", player.pause_held, list_pulse_sources)


def card_source():
    """声卡原始输入。usb-mic-mono 的上游就是 USB 采集，还没进降噪。"""
    nodes = parse_list(pactl_run(["list", "sources"]))
    for node in nodes:
        if node["name"] == RAW_SOURCE and node["master"]:
            return node["master"]
    for node in nodes:
        name = node["name"]
        if "alsa_input" in name and "usb" in name.lower() and "monitor" not in name:
            return name
    return ""


meter = MicMeter(card_source)

_aec_supported = None


def aec_supported():
    """PulseAudio 11 的 WebRTC 没有 delay_agnostic，写进参数会导致模块加载失败。"""
    global _aec_supported
    if _aec_supported is not None:
        return _aec_supported
    paths = glob.glob("/usr/lib/pulse-*/modules/libwebrtc-util.so")
    if not paths:
        _aec_supported = set(AEC_KEYS)
        return _aec_supported
    try:
        text = subprocess.check_output(
            ["strings", paths[0]], universal_newlines=True, timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        _aec_supported = set(AEC_KEYS) - {"delay_agnostic"}
        return _aec_supported
    _aec_supported = supported_aec_keys(text)
    return _aec_supported


def current_aec():
    """返回 (开关, 模块是否已加载, 错误信息)。"""
    try:
        text = pactl_run(["list", "modules"])
    except (RuntimeError, OSError) as exc:
        return parse_aec(""), False, str(exc)
    argument = ""
    seen = False
    for line in text.splitlines():
        trim = line.strip()
        if trim.startswith("Name:") and trim.endswith("module-echo-cancel"):
            seen = True
            continue
        if seen and trim.startswith("Argument:"):
            argument = trim[len("Argument:"):].strip()
            break
        if seen and (trim.startswith("Name:") or trim.startswith("Module #")):
            break
    if not argument:
        return parse_aec(""), False, ""
    return parse_aec(argument), True, ""


def speaker_sink():
    """提示音直接送到喇叭，避开通话用的回声消除。"""
    sink, _ = module_masters()
    return sink or ""


def module_masters():
    try:
        text = pactl_run(["list", "modules"])
    except (RuntimeError, OSError):
        text = ""
    argument = ""
    seen = False
    for line in text.splitlines():
        trim = line.strip()
        if trim.startswith("Name:") and trim.endswith("module-echo-cancel"):
            seen = True
            continue
        if seen and trim.startswith("Argument:"):
            argument = trim[len("Argument:"):].strip()
            break
    sink, source = echo_masters(argument)
    if sink and source:
        return sink, source
    try:
        sinks, sources, _, _ = Pactl().snapshot()
    except (RuntimeError, OSError, ValueError):
        return sink, source
    if not sink:
        for node in sinks:
            if node["name"] == AEC_SINK:
                sink = node["master"]
    if not source:
        for node in sources:
            if node["name"] == AEC_SOURCE:
                source = node["master"]
    return sink, source


def _rebind_call_audio():
    """重载回声消除后，把通话进程拉回 echo-cancel，避免它落到板载麦克风。"""
    jobs = (
        ("source-outputs", "Source Output #", "move-source-output", AEC_SOURCE),
        ("sink-inputs", "Sink Input #", "move-sink-input", AEC_SINK),
    )
    for kind, header, verb, target in jobs:
        try:
            text = pactl_run(["list", kind])
        except (RuntimeError, OSError) as exc:
            log.warning("list %s: %s", kind, exc)
            continue
        index = None
        app = ""

        def move():
            if app != "device-agent" or not index:
                return
            try:
                pactl_run([verb, index, target])
            except (RuntimeError, OSError) as exc:
                log.warning("%s %s: %s", verb, index, exc)

        for line in text.splitlines():
            trim = line.strip()
            if trim.startswith(header):
                move()
                index = trim[len(header):].strip()
                app = ""
            elif "application.name" in trim and "=" in trim:
                app = trim.split("=", 1)[1].strip().strip('"')
        move()


def apply_aec(flags):
    if read_call_state() == "active":
        raise RecordError("通话中不能修改降噪")
    sink, source = module_masters()
    if not source:
        raise RecordError("找不到 %s，无法重新加载降噪" % RAW_SOURCE)
    args = format_aec(flags, aec_supported())
    previous, ready, _ = current_aec()
    load = [
        "load-module", "module-echo-cancel",
        "use_master_format=1", "aec_method=webrtc",
        "aec_args=\"%s\"" % args,
        "source_name=%s" % AEC_SOURCE,
        "sink_name=%s" % AEC_SINK,
        "source_master=%s" % source,
    ]
    if sink:
        load.append("sink_master=%s" % sink)
    try:
        pactl_run(["unload-module", "module-echo-cancel"])
    except (RuntimeError, OSError) as exc:
        log.warning("unload echo-cancel: %s", exc)
    try:
        pactl_run(load)
    except (RuntimeError, OSError):
        if ready:
            restore = list(load)
            restore[4] = "aec_args=\"%s\"" % format_aec(previous, aec_supported())
            try:
                pactl_run(restore)
                pactl_run(["set-default-source", AEC_SOURCE])
                pactl_run(["set-default-sink", AEC_SINK])
                _rebind_call_audio()
                meter.ensure_source()
            except (RuntimeError, OSError) as exc:
                log.warning("restore echo-cancel: %s", exc)
        raise
    try:
        pactl_run(["set-default-source", AEC_SOURCE])
        pactl_run(["set-default-sink", AEC_SINK])
    except (RuntimeError, OSError) as exc:
        log.warning("reset default devices: %s", exc)
    _rebind_call_audio()
    meter.ensure_source()
    return _save_aec_config(args, sink, source)


def _save_aec_config(args, sink, source):
    try:
        with open(PA_CONFIG, encoding="utf-8") as handle:
            text = handle.read()
    except OSError:
        return "已生效，但找不到开机配置，重启后会恢复原来的设置"
    updated = rewrite_echo_line(text, args, sink, source)
    if updated is None:
        return "已生效，但开机配置里没有回声消除那一行，重启后会恢复原来的设置"
    tmp = PA_CONFIG + ".tmp"
    try:
        with open(tmp, "w", encoding="utf-8") as handle:
            handle.write(updated)
        os.replace(tmp, PA_CONFIG)
    except OSError as exc:
        log.warning("write pulse config: %s", exc)
        return "已生效，但写不了开机配置，重启后会恢复原来的设置"
    return ""


def _wav_response(path, name):
    kwargs = {"mimetype": "audio/wav", "as_attachment": True}
    if "download_name" in send_file.__code__.co_varnames:
        kwargs["download_name"] = name
    else:
        kwargs["attachment_filename"] = name
    return send_file(path, **kwargs)


def _audio_call(fn):
    try:
        return fn()
    except RecordError as exc:
        return jsonify(error=str(exc)), 400
    except (RuntimeError, ValueError, OSError) as exc:
        return jsonify(error=str(exc)), 500


@app.get("/api/cover")
def cover():
    name = request.args.get("name", "")
    if name not in list_tracks():
        return jsonify(error="找不到曲目"), 404
    found = read_cover(name)
    if found is None:
        return jsonify(error="没有封面"), 404
    data, mime = found
    # 地址里带了文件修改时间，可以放心长期缓存
    return Response(data, mimetype=mime, headers={"Cache-Control": "max-age=86400"})


@app.post("/api/announce/settings")
def announce_settings():
    body = request.get_json(silent=True) or {}
    try:
        saved = save_announce(body.get("enabled"), body.get("sound"))
    except AnnounceError as exc:
        return jsonify(error=str(exc)), 400
    return jsonify(saved)


@app.post("/api/announce/preview")
def announce_preview():
    body = request.get_json(silent=True) or {}
    sound = clean_sound(body.get("sound") or announce_config()["sound"])
    try:
        speak(sound, player.pause_for_call, speaker_sink())
    except AnnounceError as exc:
        return jsonify(error=str(exc)), 400
    return jsonify(played=True)


@app.post("/api/announce")
def announce_play():
    """设备在接通前调用。关掉或放不出来时返回 played=false，不让通话卡住。"""
    settings = announce_config()
    if not settings["enabled"]:
        return jsonify(played=False)
    try:
        speak(settings["sound"], player.pause_for_call, speaker_sink())
    except AnnounceError as exc:
        log.warning("announce: %s", exc)
        return jsonify(played=False)
    return jsonify(played=True)


@app.post("/api/volume")
def volume():
    body = request.get_json(silent=True) or {}
    try:
        value = float(body["volume"])
    except (KeyError, TypeError, ValueError):
        return jsonify(error="volume 需要是 0 到 100 的数字"), 400
    if value != value:  # NaN
        return jsonify(error="volume 需要是 0 到 100 的数字"), 400
    player.set_volume(value)
    return jsonify(player.snapshot())


@app.post("/api/upload")
def upload():
    files = request.files.getlist("file")
    if not files:
        return jsonify(error="没有收到文件"), 400
    saved = []
    for f in files:
        name = safe_name(f.filename or "")
        if not name.lower().endswith(".mp3") or len(name) <= 4:
            return jsonify(error="只支持 MP3 文件：%s" % (f.filename or "")), 400
        path = unique_path(name)
        f.save(path)
        saved.append(os.path.basename(path))
    return jsonify(saved=saved)


@app.post("/api/play")
def play():
    name = (request.get_json(silent=True) or {}).get("name", "")
    if name not in list_tracks():
        return jsonify(error="找不到曲目"), 404
    player.play(name)
    return jsonify(player.snapshot())


@app.post("/api/pause")
def pause():
    player.toggle()
    return jsonify(player.snapshot())


@app.post("/api/mode")
def mode():
    value = (request.get_json(silent=True) or {}).get("mode")
    if value not in MODES:
        return jsonify(error="mode 只能是 %s" % " / ".join(MODES)), 400
    player.set_mode(value)
    return jsonify(player.snapshot())


@app.post("/api/next")
def next_track():
    player.skip(1)
    return jsonify(player.snapshot())


@app.post("/api/prev")
def prev_track():
    player.skip(-1)
    return jsonify(player.snapshot())


@app.post("/api/seek")
def seek():
    try:
        position = float((request.get_json(silent=True) or {})["position"])
    except (KeyError, TypeError, ValueError):
        return jsonify(error="position 需要是秒数"), 400
    if position != position:  # NaN
        return jsonify(error="position 需要是秒数"), 400
    player.seek(position)
    return jsonify(player.snapshot())


@app.post("/api/delete")
def delete():
    name = (request.get_json(silent=True) or {}).get("name", "")
    if name not in list_tracks():
        return jsonify(error="找不到曲目"), 404
    player.forget(name)
    try:
        os.remove(os.path.join(MUSIC_DIR, name))
    except OSError as e:
        return jsonify(error="删除失败：%s" % e.strerror), 500
    with _info_lock:
        _info_cache.pop(name, None)
    log.info("deleted %s", name)
    return jsonify(player.snapshot())


@app.post("/api/stop")
def stop():
    player.stop()
    return jsonify(player.snapshot())


@app.get("/api/mic")
def mic_level():
    return jsonify(meter.snapshot())


@app.post("/api/record/start")
def record_start():
    def run():
        recorder.start()
        return jsonify(recorder.status())
    return _audio_call(run)


@app.post("/api/record/stop")
def record_stop():
    recorder.stop()
    return jsonify(recorder.status())


@app.post("/api/record/test")
def record_test():
    def run():
        names = recorder.test()
        return jsonify(saved=names, recordings=recorder.items())
    return _audio_call(run)


@app.post("/api/recordings/play")
def recording_play():
    name = (request.get_json(silent=True) or {}).get("name", "")

    def run():
        recorder.play(name)
        return jsonify(recorder.status())
    return _audio_call(run)


@app.post("/api/recordings/stop")
def recording_stop_play():
    recorder.stop_play()
    return jsonify(recorder.status())


@app.post("/api/recordings/delete")
def recording_delete():
    name = (request.get_json(silent=True) or {}).get("name", "")

    def run():
        recorder.delete(name)
        return jsonify(recordings=recorder.items())
    return _audio_call(run)


@app.get("/api/recordings/<name>")
def recording_download(name):
    path = recorder._existing(name)
    if path is None:
        return jsonify(error="找不到录音"), 404
    return _wav_response(path, name)


@app.post("/api/beep")
def beep():
    """网页调完喇叭后由设备调用。不暂停正在放的音乐。"""
    try:
        speak("high", None, speaker_sink())
    except AnnounceError as exc:
        log.warning("volume beep: %s", exc)
        return jsonify(played=False)
    return jsonify(played=True)


@app.post("/api/levels")
def levels():
    body = request.get_json(silent=True) or {}
    if "speaker" not in body and "mic" not in body:
        return jsonify(error="需要 speaker 或 mic"), 400

    def run():
        speaker = body["speaker"] if "speaker" in body else None
        mic = body["mic"] if "mic" in body else None
        got_speaker, got_mic = Pactl().set(speaker, mic)
        if speaker is not None and body.get("beep"):
            try:
                speak("high", None, speaker_sink())
            except AnnounceError as exc:
                log.warning("volume beep: %s", exc)
        return jsonify(speaker=got_speaker, mic=got_mic)
    return _audio_call(run)


@app.post("/api/aec")
def aec():
    body = request.get_json(silent=True) or {}

    def run():
        flags, _, _ = current_aec()
        for key in AEC_KEYS:
            if key in body:
                flags[key] = bool(body[key])
        warning = apply_aec(flags)
        applied, ready, err = current_aec()
        return jsonify(aec={key: applied[key] for key in AEC_KEYS}, aecReady=ready, aecError=err, warning=warning)
    return _audio_call(run)


@app.errorhandler(413)
def too_large(_):
    return jsonify(error="文件超过 %d MB" % MAX_UPLOAD_MB), 413


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    os.makedirs(MUSIC_DIR, exist_ok=True)
    os.makedirs(REC_DIR, exist_ok=True)
    threading.Thread(target=watch_calls, daemon=True).start()
    log.info("music player on :%d, dir %s", MUSIC_PORT, MUSIC_DIR)
    app.run(host="0.0.0.0", port=MUSIC_PORT, threaded=True)


if __name__ == "__main__":
    main()

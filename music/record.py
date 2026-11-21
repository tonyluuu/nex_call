# -*- coding: utf-8 -*-
"""从 PulseAudio 录音、在喇叭上播放，以及降噪对比。"""
import math
import os
import select
import shutil
import signal
import struct
import subprocess
import threading
import time
import wave

RAW_SOURCE = "usb-mic-mono"
AEC_SOURCE = "echo-cancel-source"
AEC_SINK = "echo-cancel-sink"
MAX_SECONDS = 180
TEST_SECONDS = 6


class RecordError(Exception):
    pass


def _which_record():
    if shutil.which("parec"):
        return "parec"
    if shutil.which("pw-record"):
        return "pw-record"
    return ""


def _which_play():
    if shutil.which("paplay"):
        return "paplay"
    if shutil.which("pw-play"):
        return "pw-play"
    return ""


def source_names(text):
    names = []
    for line in (text or "").splitlines():
        parts = line.split()
        if len(parts) >= 2:
            names.append(parts[1])
    return names


def recording_label(name):
    if name.endswith("-raw.wav"):
        return "处理前"
    if name.endswith("-aec.wav"):
        return "处理后"
    return "录音"


def safe_wav(name):
    base = os.path.basename(name or "")
    if base != name or not base.endswith(".wav"):
        return ""
    if not all(c.isalnum() or c in "._-" for c in base):
        return ""
    return base


def write_tone(path, seconds=TEST_SECONDS, freq=600, rate=48000):
    count = int(seconds * rate)
    with wave.open(path, "w") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(rate)
        frames = bytearray()
        for i in range(count):
            sample = int(0.3 * 32767 * math.sin(2 * math.pi * freq * i / rate))
            frames += struct.pack("<h", sample)
        handle.writeframes(frames)


class Recorder(object):
    def __init__(self, directory, call_active, pause_music, list_sources):
        self.directory = directory
        self.call_active = call_active
        self.pause_music = pause_music
        self.list_sources = list_sources
        self.lock = threading.Lock()
        self.procs = []
        self.files = []
        self.mode = ""
        self.started = 0.0
        self.timer = None
        self.play_proc = None
        self.playing = ""
        self.play_started = 0.0
        self._durations = {}

    def status(self):
        with self.lock:
            return {
                "active": bool(self.procs),
                "mode": self.mode,
                "startedAt": int(self.started * 1000) if self.procs else 0,
                "playing": self.playing,
                "playStartedAt": int(self.play_started * 1000) if self.playing else 0,
            }

    def items(self):
        try:
            names = [n for n in os.listdir(self.directory) if safe_wav(n) and not n.endswith("-tone.wav")]
        except OSError:
            return []
        rows = []
        for name in names:
            path = os.path.join(self.directory, name)
            try:
                st = os.stat(path)
            except OSError:
                continue
            rows.append({
                "name": name,
                "label": recording_label(name),
                "size": st.st_size,
                "mtime": int(st.st_mtime),
                "duration": round(self._duration(path, name, st), 2),
            })
        rows.sort(key=lambda row: row["mtime"], reverse=True)
        return rows

    def start(self):
        self._ensure_idle()
        if AEC_SOURCE not in self.list_sources():
            raise RecordError("找不到 %s，无法录音" % AEC_SOURCE)
        self.pause_music()
        path = self._new_path("rec-%s.wav" % time.strftime("%Y%m%d-%H%M%S"))
        proc = self._spawn(AEC_SOURCE, path)
        with self.lock:
            self.procs = [proc]
            self.files = [path]
            self.mode = "record"
            self.started = time.time()
            self._arm(MAX_SECONDS)
        return os.path.basename(path)

    def test(self):
        self._ensure_idle()
        names = self.list_sources()
        if RAW_SOURCE not in names or AEC_SOURCE not in names:
            raise RecordError("找不到 %s 或 %s，无法对比降噪" % (RAW_SOURCE, AEC_SOURCE))
        if _which_play() == "":
            raise RecordError("系统没有 paplay 或 pw-play，无法播放测试音")
        self.pause_music()
        stamp = time.strftime("%Y%m%d-%H%M%S")
        raw_path = self._new_path("test-%s-raw.wav" % stamp)
        aec_path = self._new_path("test-%s-aec.wav" % stamp)
        tone = self._new_path("test-%s-tone.wav" % stamp)
        write_tone(tone)
        raw = None
        try:
            raw = self._spawn(RAW_SOURCE, raw_path)
            aec = self._spawn(AEC_SOURCE, aec_path)
        except Exception:
            if raw is not None:
                _finish(raw)
            try:
                os.remove(tone)
            except OSError:
                pass
            raise
        with self.lock:
            self.procs = [raw, aec]
            self.files = [raw_path, aec_path]
            self.mode = "test"
            self.started = time.time()
        play = subprocess.Popen(
            self._play_args(tone, AEC_SINK),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        try:
            play.wait(timeout=TEST_SECONDS + 8)
        except subprocess.TimeoutExpired:
            play.kill()
            play.wait()
        finally:
            self.stop()
            try:
                os.remove(tone)
            except OSError:
                pass
        return [os.path.basename(raw_path), os.path.basename(aec_path)]

    def stop(self):
        with self.lock:
            procs = list(self.procs)
            timer = self.timer
            self.procs = []
            self.files = []
            self.mode = ""
            self.started = 0.0
            self.timer = None
        if timer is not None:
            timer.cancel()
        for proc in procs:
            _finish(proc)
        return True

    def play(self, name):
        path = self._existing(name)
        if path is None:
            raise RecordError("找不到录音")
        with self.lock:
            if self.procs:
                raise RecordError("录音或测试进行中，不能播放")
        self.stop_play()
        self.pause_music()
        proc = subprocess.Popen(
            self._play_args(path, AEC_SINK),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        with self.lock:
            self.play_proc = proc
            self.playing = name
            self.play_started = time.time()
        threading.Thread(target=self._wait_play, args=(proc,), daemon=True).start()

    def stop_play(self):
        with self.lock:
            proc = self.play_proc
            self.play_proc = None
            self.playing = ""
            self.play_started = 0.0
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                proc.kill()

    def delete(self, name):
        path = self._existing(name)
        if path is None:
            raise RecordError("找不到录音")
        with self.lock:
            if self.playing == name:
                proc = self.play_proc
                self.play_proc = None
                self.playing = ""
                self.play_started = 0.0
            else:
                proc = None
            recording = path in self.files
        if recording:
            raise RecordError("这段录音还在进行")
        if proc is not None and proc.poll() is None:
            proc.terminate()
        os.remove(path)

    def _ensure_idle(self):
        if self.call_active():
            raise RecordError("通话中不能录音")
        if _which_record() == "":
            raise RecordError("系统没有 parec 或 pw-record")
        with self.lock:
            if self.procs:
                raise RecordError("已经在录音或测试")

    def _spawn(self, source, path):
        tool = _which_record()
        if tool == "parec":
            handle = open(path, "wb")
            proc = subprocess.Popen(
                [
                    "parec",
                    "--device=" + source,
                    "--rate=48000",
                    "--channels=1",
                    "--format=s16le",
                    "--file-format=wav",
                ],
                stdout=handle,
                stderr=subprocess.DEVNULL,
            )
            proc._out = handle
            return proc
        return subprocess.Popen(
            ["pw-record", "--target", source, "--rate", "48000", "--channels", "1", path],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    def _play_args(self, path, sink):
        if _which_play() == "paplay":
            return ["paplay", "--device=" + sink, path]
        return ["pw-play", "--target", sink, path]

    def _arm(self, seconds):
        timer = threading.Timer(seconds, self.stop)
        timer.daemon = True
        timer.start()
        self.timer = timer

    def _new_path(self, name):
        os.makedirs(self.directory, exist_ok=True)
        path = os.path.join(self.directory, name)
        if not os.path.exists(path):
            return path
        base, ext = os.path.splitext(name)
        n = 1
        while True:
            path = os.path.join(self.directory, "%s-%d%s" % (base, n, ext))
            if not os.path.exists(path):
                return path
            n += 1

    def _duration(self, path, name, st):
        cached = self._durations.get(name)
        if cached and cached[0] == st.st_mtime_ns and cached[1] == st.st_size:
            return cached[2]
        duration = wav_duration(path)
        self._durations[name] = (st.st_mtime_ns, st.st_size, duration)
        return duration

    def _existing(self, name):
        safe = safe_wav(name)
        if not safe:
            return None
        path = os.path.join(self.directory, safe)
        if not os.path.isfile(path):
            return None
        return path

    def _wait_play(self, proc):
        proc.wait()
        with self.lock:
            if self.play_proc is proc:
                self.play_proc = None
                self.playing = ""
                self.play_started = 0.0


def wav_duration(path):
    try:
        handle = wave.open(path, "rb")
    except (wave.Error, EOFError, OSError):
        return 0.0
    try:
        rate = handle.getframerate()
        frames = handle.getnframes()
    finally:
        handle.close()
    if not rate:
        return 0.0
    return frames / float(rate)


class MicMeter(object):
    """持续读取声卡原始输入，供页面看它有没有送来采样。"""

    def __init__(self, resolve_source):
        self.resolve_source = resolve_source
        self.lock = threading.Lock()
        self.peak = 0
        self.updated = 0.0
        self.source = ""
        self.error = ""
        self._proc = None
        threading.Thread(target=self._run, daemon=True).start()

    def snapshot(self):
        with self.lock:
            age = time.time() - self.updated if self.updated else 99
            fresh = age < 4
            peak = self.peak if fresh else 0
            if peak <= 0:
                level = 0
            else:
                level = min(100, int((peak / 32767.0) ** 0.5 * 100))
            if level == 0 and peak > 0:
                level = 1
            error = self.error
            if not fresh and not error:
                error = "声卡检测中断"
            return {
                "source": self.source,
                "peak": peak,
                "level": level,
                "heard": fresh and peak > 0,
                "error": error,
            }

    def _run(self):
        while True:
            if _which_record() != "parec":
                self._fail("系统没有 parec")
                time.sleep(2)
                continue
            try:
                source = self.resolve_source() or ""
            except Exception as exc:
                self._fail("找不到声卡输入：%s" % exc)
                time.sleep(1)
                continue
            if not source:
                self._fail("找不到声卡输入")
                time.sleep(1)
                continue
            command = []
            if shutil.which("stdbuf"):
                command.extend(["stdbuf", "-o0"])
            command.extend([
                "parec",
                "--device=" + source,
                "--client-name=nexcall-mic-meter",
                "--raw",
                "--rate=16000",
                "--channels=2",
                "--format=s16le",
            ])
            try:
                proc = subprocess.Popen(
                    command,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                )
            except OSError as exc:
                self._fail("无法读取声卡：%s" % exc)
                time.sleep(1)
                continue
            with self.lock:
                self.source = source
                self.error = ""
                self._proc = proc
            self._stick_to_source(proc, source, wait=True)
            try:
                fd = proc.stdout.fileno()
                checked = time.time()
                while proc.poll() is None:
                    if time.time() - checked >= 2:
                        self._stick_to_source(proc, source)
                        checked = time.time()
                    ready, _, _ = select.select([fd], [], [], 1.0)
                    if not ready:
                        with self.lock:
                            self.peak = 0
                            self.updated = time.time()
                        continue
                    try:
                        chunk = os.read(fd, 6400)
                    except OSError:
                        break
                    if not chunk:
                        break
                    count = len(chunk) // 2
                    peak = 0
                    if count:
                        samples = struct.unpack("<%dh" % count, chunk[:count * 2])
                        for sample in samples:
                            value = sample if sample >= 0 else -sample
                            if value > peak:
                                peak = value
                    with self.lock:
                        self.peak = peak
                        self.updated = time.time()
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()
                with self.lock:
                    if self._proc is proc:
                        self._proc = None
            time.sleep(0.4)

    def ensure_source(self):
        with self.lock:
            proc = self._proc
            source = self.source
        if proc is not None and source:
            self._stick_to_source(proc, source)

    def _stick_to_source(self, proc, source, wait=False):
        """Pulse 重载回声消除时会把监测挪到板载麦克风，这里拉回 USB 声卡。"""
        if wait:
            time.sleep(0.3)
        if proc.poll() is not None:
            return
        try:
            outputs = subprocess.check_output(
                ["pactl", "list", "source-outputs"],
                universal_newlines=True,
                timeout=3,
            )
            listing = subprocess.check_output(
                ["pactl", "list", "short", "sources"],
                universal_newlines=True,
                timeout=3,
            )
        except (OSError, subprocess.SubprocessError):
            return
        names = {}
        for line in listing.splitlines():
            parts = line.split()
            if len(parts) >= 2:
                names[parts[0]] = parts[1]
        index = ""
        current = ""
        app = ""

        def consider():
            if app != "nexcall-mic-meter" or not index:
                return
            if names.get(current, current) == source:
                return
            try:
                subprocess.check_call(
                    ["pactl", "move-source-output", index, source],
                    timeout=3,
                )
            except (OSError, subprocess.SubprocessError):
                pass

        for line in outputs.splitlines():
            trim = line.strip()
            if trim.startswith("Source Output #"):
                consider()
                index = trim.split("#", 1)[1].strip()
                current = ""
                app = ""
            elif trim.startswith("Source:"):
                current = trim.split(":", 1)[1].strip()
            elif "application.name" in trim and "=" in trim:
                app = trim.split("=", 1)[1].strip().strip('"')
        consider()

    def _fail(self, message):
        with self.lock:
            self.error = message
            self.peak = 0
            self.updated = time.time()


def _finish(proc):
    if proc.poll() is None:
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
    handle = getattr(proc, "_out", None)
    if handle is not None:
        handle.close()

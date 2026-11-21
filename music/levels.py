# -*- coding: utf-8 -*-
"""读取和设置 PulseAudio 音量，规则与 device/internal/volume 相同。

0–100% 落在回声消除链路最底层的硬件设备上，中间的虚拟设备固定 100%。
超过 100% 时硬件保持满幅，多出来的部分由 PulseAudio 做软件放大。
"""
import re
import subprocess

MAX_VOLUME = 200
PERCENT_RE = re.compile(r"(\d+)\s*%")

# 页面上可调的开关。模拟增益始终关闭，不出现在页面上。
AEC_KEYS = (
    "noise_suppression",
    "high_pass_filter",
    "extended_filter",
    "digital_gain_control",
    "delay_agnostic",
)
AEC_DEFAULTS = {
    "noise_suppression": True,
    "high_pass_filter": True,
    "extended_filter": True,
    "digital_gain_control": True,
    "delay_agnostic": False,
    "analog_gain_control": False,
}
AEC_ORDER = (
    "high_pass_filter",
    "noise_suppression",
    "analog_gain_control",
    "digital_gain_control",
    "extended_filter",
    "delay_agnostic",
)


def clamp_level(value):
    try:
        number = int(round(float(value)))
    except (TypeError, ValueError):
        raise ValueError("音量需要是数字")
    if number != number:  # NaN
        raise ValueError("音量需要是数字")
    return max(0, min(MAX_VOLUME, number))


def parse_info(text):
    sink = source = ""
    for line in text.splitlines():
        trim = line.strip()
        if trim.startswith("Default Sink:"):
            sink = trim[len("Default Sink:"):].strip()
        elif trim.startswith("Default Source:"):
            source = trim[len("Default Source:"):].strip()
    return sink, source


def _prop_value(line):
    if "=" not in line:
        return ""
    value = line.split("=", 1)[1].strip().strip('"')
    if value in ("", "(null)"):
        return ""
    return value


def parse_list(text):
    nodes = []
    cur = None

    def flush():
        if cur is not None and cur["name"]:
            nodes.append(cur)

    for line in text.splitlines():
        trim = line.strip()
        if trim.startswith("Sink #") or trim.startswith("Source #"):
            flush()
            cur = {"name": "", "volume": 0, "has_vol": False, "master": ""}
            continue
        if cur is None:
            continue
        if trim.startswith("Name:"):
            cur["name"] = trim[len("Name:"):].strip()
        elif trim.startswith("Volume:") and not cur["has_vol"]:
            found = PERCENT_RE.search(trim)
            if found:
                cur["volume"] = int(found.group(1))
                cur["has_vol"] = True
        elif "device.master_device" in trim:
            cur["master"] = _prop_value(trim)
    flush()
    return nodes


def resolve(nodes, def_name, hint=""):
    """决定真正要调节的设备。hint 按名称子串匹配；否则沿 master 走到硬件。"""
    if hint:
        want = hint.lower()
        match = [n for n in nodes if want in n["name"].lower()]
        if len(match) == 1:
            return match[0]["name"], []
        if not match:
            raise ValueError("找不到设备 %s" % hint)
        raise ValueError("设备名 %s 对应了多个设备" % hint)
    if not def_name:
        raise ValueError("没有默认设备")
    return _follow(nodes, def_name)


def _follow(nodes, start):
    by_name = {n["name"]: n for n in nodes}
    cur = by_name.get(start)
    if cur is None:
        return start, []
    seen = set()
    mids = []
    while cur["master"] and cur["name"] not in seen:
        nxt = by_name.get(cur["master"])
        if nxt is None:
            break
        seen.add(cur["name"])
        mids.append(cur["name"])
        cur = nxt
    return cur["name"], mids


def _level(nodes, def_name):
    target, _ = resolve(nodes, def_name, "")
    for node in nodes:
        if node["name"] == target and node["has_vol"]:
            return node["volume"]
    raise ValueError("读不到 %s 的音量" % target)


def parse_aec(text):
    """从模块参数或 default.pa 的一行里读出开关。缺省按当前开机配置。"""
    flags = dict(AEC_DEFAULTS)
    quoted = re.search(r'aec_args="([^"]*)"', text or "")
    blob = quoted.group(1) if quoted else (text or "")
    for key in AEC_DEFAULTS:
        found = re.search(r"\b%s=(\d)" % key, blob)
        if found:
            flags[key] = found.group(1) == "1"
    flags["analog_gain_control"] = False
    return flags


def supported_aec_keys(text):
    """从 libwebrtc-util 的字符串里判断页面上的开关哪些真能传给模块。"""
    names = set((text or "").split())
    return set(key for key in AEC_KEYS if key in names)


def format_aec(flags, supported=None):
    parts = []
    for key in AEC_ORDER:
        if key == "analog_gain_control":
            parts.append("analog_gain_control=0")
            continue
        if supported is not None and key not in supported:
            continue
        parts.append("%s=%d" % (key, 1 if flags.get(key) else 0))
    return " ".join(parts)


def echo_masters(line):
    sink = source = ""
    found = re.search(r"sink_master=(\S+)", line or "")
    if found:
        sink = found.group(1)
    found = re.search(r"source_master=(\S+)", line or "")
    if found:
        source = found.group(1)
    return sink, source


def build_echo_line(args, sink_master, source_master):
    parts = [
        "load-module module-echo-cancel use_master_format=1 aec_method=webrtc",
        'aec_args="%s"' % args,
        "source_name=echo-cancel-source sink_name=echo-cancel-sink",
    ]
    if sink_master:
        parts.append("sink_master=%s" % sink_master)
    if source_master:
        parts.append("source_master=%s" % source_master)
    return " ".join(parts)


def rewrite_echo_line(text, args, sink_master, source_master):
    """替换 default.pa 里的回声消除行。没有这一行时返回 None。"""
    found = False
    out = []
    for raw in (text or "").splitlines():
        if raw.startswith("load-module module-echo-cancel"):
            old_sink, old_source = echo_masters(raw)
            out.append(build_echo_line(args, sink_master or old_sink, source_master or old_source))
            found = True
        else:
            out.append(raw)
    if not found:
        return None
    return "\n".join(out) + "\n"


def pactl_run(args):
    try:
        return subprocess.check_output(
            ["pactl"] + list(args),
            stderr=subprocess.STDOUT,
            universal_newlines=True,
            timeout=5,
        )
    except subprocess.CalledProcessError as exc:
        msg = (exc.output or "").strip()
        raise RuntimeError(msg or "pactl 失败")
    except OSError as exc:
        raise RuntimeError("无法运行 pactl：%s" % exc)


class Pactl(object):
    def __init__(self, run=None):
        self._run = run or pactl_run

    def _exec(self, *args):
        return self._run(list(args))

    def snapshot(self):
        info = self._exec("info")
        sinks = parse_list(self._exec("list", "sinks"))
        sources = parse_list(self._exec("list", "sources"))
        def_sink, def_source = parse_info(info)
        return sinks, sources, def_sink, def_source

    def get(self):
        sinks, sources, def_sink, def_source = self.snapshot()
        return _level(sinks, def_sink), _level(sources, def_source)

    def set(self, speaker=None, mic=None):
        sinks, sources, def_sink, def_source = self.snapshot()
        if speaker is not None:
            self._apply("sink", sinks, def_sink, clamp_level(speaker))
        if mic is not None:
            self._apply("source", sources, def_source, clamp_level(mic))
        return self.get()

    def _apply(self, kind, nodes, def_name, value):
        target, mids = resolve(nodes, def_name, "")
        verb = "set-sink-volume" if kind == "sink" else "set-source-volume"
        for mid in mids:
            self._exec(verb, mid, "100%")
        self._exec(verb, target, "%d%%" % value)

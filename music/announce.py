"""来电提示：播放所选的短滴。旧的句子函数仍留着，页面已经不再念字。

需兼容 Ubuntu 18.04 的 Python 3.6。
"""
import os
import shutil
import subprocess
import threading

DEFAULT_TEXT = "收到来自{name}的通话，即将自动接听"
SOUNDS = (
    ("single", "一声短滴"),
    ("double", "滴滴两声"),
    ("high", "高音短促"),
    ("low", "低音一声"),
    ("phone", "电话铃"),
    ("triple", "三声短滴"),
)
DEFAULT_SOUND = "single"
SOUNDS_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sounds")
_SOUND_IDS = dict(SOUNDS)
MAX_TEXT = 80
MAX_NAME = 32
FALLBACK_NAME = "有人"

_lock = threading.Lock()


class AnnounceError(Exception):
    pass


def clean_template(text):
    if not isinstance(text, str):
        return ""
    text = "".join(ch for ch in text if ch not in "\r\n")
    text = text.strip()
    return text[:MAX_TEXT].strip()


def clean_name(name):
    if not isinstance(name, str):
        name = ""
    chars = []
    for ch in name.strip():
        if ch < " " or ch == "\x7f" or ch in "{}":
            continue
        chars.append(ch)
        if len(chars) >= MAX_NAME:
            break
    text = "".join(chars).strip()
    return text or FALLBACK_NAME


def render_announce(template, name):
    text = clean_template(template) or DEFAULT_TEXT
    return text.replace("{name}", clean_name(name))


def clean_sound(sound):
    if sound in _SOUND_IDS:
        return sound
    return DEFAULT_SOUND


def speak(sound, before=None, device=""):
    """播放一种滴声。before 在真正出声前调用，用来先暂停音乐。同一时间只放一声。

    device 是喇叭本身。不填就走系统默认输出，通话降噪会把声音滤得发闷。
    """
    sound = clean_sound(sound)
    path = os.path.join(SOUNDS_DIR, sound + ".wav")
    if not os.path.isfile(path):
        raise AnnounceError("没有这种提示音")
    if not shutil.which("paplay"):
        raise AnnounceError("系统没有 paplay，无法播放提示音")
    with _lock:
        if before is not None:
            before()
        cmd = ["paplay", path]
        if device:
            cmd.insert(1, "--device=%s" % device)
        try:
            play = subprocess.Popen(
                cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
            )
        except OSError as exc:
            raise AnnounceError("无法播放提示音：%s" % exc)
        try:
            play.wait(timeout=20)
        except subprocess.TimeoutExpired:
            play.kill()
            raise AnnounceError("提示音播放超时")
        if play.returncode != 0:
            err = b""
            if play.stderr:
                err = play.stderr.read() or b""
            raise AnnounceError("提示音播放失败：%s" % err.decode("utf-8", "replace").strip())

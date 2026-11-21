# -*- coding: utf-8 -*-
import unittest

from levels import (
    AEC_DEFAULTS,
    AEC_KEYS,
    Pactl,
    format_aec,
    parse_aec,
    parse_info,
    parse_list,
    resolve,
    rewrite_echo_line,
)

PA_INFO = """
Server String: unix:/run/user/1000/pulse/native
Default Sink: echo-cancel-sink
Default Source: echo-cancel-source
"""

PA_SINKS = """
Sink #0
	Name: alsa_output.usb-Generic_USB_Audio-00.analog-stereo
	Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB
	Properties:
		device.description = "USB Audio"

Sink #1
	Name: echo-cancel-sink
	Volume: front-left: 65536 / 80% / -5.81 dB,   front-right: 65536 / 80% / -5.81 dB
	Properties:
		device.master_device = "alsa_output.usb-Generic_USB_Audio-00.analog-stereo"
"""

PA_SOURCES = """
Source #0
	Name: alsa_input.usb-Generic_USB_Audio-00.analog-stereo
	Volume: front-left: 98304 / 150% / 10.57 dB,   front-right: 98304 / 150% / 10.57 dB
	Properties:
		alsa.card_name = "USB Audio"

Source #1
	Name: usb-mic-mono
	Volume: mono: 65536 / 100% / 0.00 dB
	Properties:
		device.master_device = "alsa_input.usb-Generic_USB_Audio-00.analog-stereo"

Source #2
	Name: echo-cancel-source
	Volume: mono: 0 / 0% / -inf dB
	Properties:
		device.master_device = "usb-mic-mono"
"""

PIPEWIRE_SINKS = """
Sink #40
	Name: echo-cancel-sink
	Volume: front-left: 65536 / 60% / -13.31 dB,   front-right: 65536 / 60% / -13.31 dB
	Properties:
		node.name = "echo-cancel-sink"
		device.class = "Audio/Sink"
"""


class LevelsTest(unittest.TestCase):
    def test_pulse_chain(self):
        sink, source = parse_info(PA_INFO)
        self.assertEqual(sink, "echo-cancel-sink")
        self.assertEqual(source, "echo-cancel-source")
        sinks = parse_list(PA_SINKS)
        target, mids = resolve(sinks, sink)
        self.assertEqual(target, "alsa_output.usb-Generic_USB_Audio-00.analog-stereo")
        self.assertEqual(mids, ["echo-cancel-sink"])
        self.assertEqual(sinks[0]["volume"], 100)
        self.assertEqual(sinks[1]["volume"], 80)

        sources = parse_list(PA_SOURCES)
        target, mids = resolve(sources, source)
        self.assertEqual(target, "alsa_input.usb-Generic_USB_Audio-00.analog-stereo")
        self.assertEqual(mids, ["echo-cancel-source", "usb-mic-mono"])
        self.assertEqual(sources[0]["volume"], 150)
        self.assertTrue(sources[2]["has_vol"])
        self.assertEqual(sources[2]["volume"], 0)

    def test_pipewire_without_master(self):
        sinks = parse_list(PIPEWIRE_SINKS)
        target, mids = resolve(sinks, "echo-cancel-sink")
        self.assertEqual(target, "echo-cancel-sink")
        self.assertEqual(mids, [])
        self.assertEqual(sinks[0]["volume"], 60)

    def test_set_writes_hardware_and_clamps(self):
        cmds = []
        lists = {"sinks": PA_SINKS, "sources": PA_SOURCES}

        def run(args):
            cmds.append(" ".join(args))
            if args[0] == "info":
                return PA_INFO
            if args[0] == "list":
                return lists[args[1]]
            return ""

        Pactl(run).set(250, -5)
        sets = [c for c in cmds if c.startswith("set-")]
        self.assertEqual(sets, [
            "set-sink-volume echo-cancel-sink 100%",
            "set-sink-volume alsa_output.usb-Generic_USB_Audio-00.analog-stereo 200%",
            "set-source-volume echo-cancel-source 100%",
            "set-source-volume usb-mic-mono 100%",
            "set-source-volume alsa_input.usb-Generic_USB_Audio-00.analog-stereo 0%",
        ])

    def test_rewrite_keeps_masters(self):
        original = "\n".join([
            ".include /etc/pulse/default.pa",
            'load-module module-echo-cancel use_master_format=1 aec_method=webrtc '
            'aec_args="high_pass_filter=1 noise_suppression=1 analog_gain_control=0 digital_gain_control=1 extended_filter=1" '
            "source_name=echo-cancel-source sink_name=echo-cancel-sink "
            "sink_master=alsa_output.usb source_master=usb-mic-mono",
        ])
        flags = parse_aec(original)
        self.assertTrue(flags["noise_suppression"])
        self.assertFalse(flags["delay_agnostic"])
        flags["noise_suppression"] = False
        flags["delay_agnostic"] = True
        updated = rewrite_echo_line(original, "high_pass_filter=1 noise_suppression=0 analog_gain_control=0 digital_gain_control=1 extended_filter=1 delay_agnostic=1", "", "")
        self.assertIn("sink_master=alsa_output.usb", updated)
        self.assertIn("source_master=usb-mic-mono", updated)
        self.assertIn("noise_suppression=0", updated)
        self.assertIn("delay_agnostic=1", updated)
        self.assertEqual(updated.count("load-module module-echo-cancel"), 1)

    def test_format_skips_unsupported_key(self):
        flags = dict(AEC_DEFAULTS)
        flags["noise_suppression"] = False
        flags["delay_agnostic"] = True
        text = format_aec(flags, supported=set(AEC_KEYS) - {"delay_agnostic"})
        self.assertEqual(
            text,
            "high_pass_filter=1 noise_suppression=0 analog_gain_control=0 "
            "digital_gain_control=1 extended_filter=1",
        )
        self.assertNotIn("delay_agnostic", text)


if __name__ == "__main__":
    unittest.main()

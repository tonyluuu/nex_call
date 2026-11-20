package volume

import (
	"strings"
	"testing"
)

const paInfo = `
Server String: unix:/run/user/1000/pulse/native
Server Name: pulseaudio
Server Version: 11.1
Default Sink: echo-cancel-sink
Default Source: echo-cancel-source
Cookie: 1234:5678
`

const paSinks = `
Sink #0
	State: IDLE
	Name: alsa_output.usb-Generic_USB_Audio-00.analog-stereo
	Description: USB Audio Analog Stereo
	Mute: no
	Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB
	        balance 0.00
	Base Volume: 65536 / 100% / 0.00 dB
	Properties:
		alsa.card_name = "USB Audio"
		device.description = "USB Audio"

Sink #1
	State: IDLE
	Name: echo-cancel-sink
	Description: Echo-Cancel Sink
	Mute: no
	Volume: front-left: 65536 / 80% / -5.81 dB,   front-right: 65536 / 80% / -5.81 dB
	        balance 0.00
	Base Volume: 65536 / 100% / 0.00 dB
	Properties:
		device.master_device = "alsa_output.usb-Generic_USB_Audio-00.analog-stereo"
		device.class = "filter"
`

const paSources = `
Source #0
	State: IDLE
	Name: alsa_input.usb-Generic_USB_Audio-00.analog-stereo
	Mute: no
	Volume: front-left: 98304 / 150% / 10.57 dB,   front-right: 98304 / 150% / 10.57 dB
	        balance 0.00
	Properties:
		alsa.card_name = "USB Audio"

Source #1
	State: IDLE
	Name: usb-mic-mono
	Driver: module-remap-source.c
	Volume: mono: 65536 / 100% / 0.00 dB
	Properties:
		device.master_device = "alsa_input.usb-Generic_USB_Audio-00.analog-stereo"

Source #2
	State: IDLE
	Name: echo-cancel-source
	Driver: module-echo-cancel.c
	Volume: mono: 0 / 0% / -inf dB
	Properties:
		device.master_device = "usb-mic-mono"
`

const pipewireSinks = `
Sink #40
	State: RUNNING
	Name: echo-cancel-sink
	Description: Echo-Cancel Sink
	Mute: no
	Volume: front-left: 65536 / 60% / -13.31 dB,   front-right: 65536 / 60% / -13.31 dB
	        balance 0.00
	Properties:
		node.name = "echo-cancel-sink"
		device.class = "Audio/Sink"
`

func TestParsePulseAudioChain(t *testing.T) {
	sink, source := ParseInfo(paInfo)
	if sink != "echo-cancel-sink" || source != "echo-cancel-source" {
		t.Fatalf("defaults %q %q", sink, source)
	}
	sinks := ParseList(paSinks)
	target, mids, err := Resolve(sinks, sink, "")
	if err != nil {
		t.Fatal(err)
	}
	if target != "alsa_output.usb-Generic_USB_Audio-00.analog-stereo" || len(mids) != 1 || mids[0] != "echo-cancel-sink" {
		t.Fatalf("sink target %q mids %v", target, mids)
	}
	if sinks[0].Volume != 100 || sinks[1].Volume != 80 {
		t.Fatalf("sink volumes %+v", sinks)
	}

	sources := ParseList(paSources)
	target, mids, err = Resolve(sources, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if target != "alsa_input.usb-Generic_USB_Audio-00.analog-stereo" {
		t.Fatalf("source target %q", target)
	}
	if strings.Join(mids, ",") != "echo-cancel-source,usb-mic-mono" {
		t.Fatalf("source mids %v", mids)
	}
	if sources[0].Volume != 150 || sources[2].Volume != 0 || !sources[2].HasVol {
		t.Fatalf("source volumes %+v", sources)
	}
}

func TestParsePipeWireFallsBackToDefault(t *testing.T) {
	sinks := ParseList(pipewireSinks)
	target, mids, err := Resolve(sinks, "echo-cancel-sink", "")
	if err != nil {
		t.Fatal(err)
	}
	if target != "echo-cancel-sink" || mids != nil {
		t.Fatalf("target %q mids %v", target, mids)
	}
	if sinks[0].Volume != 60 {
		t.Fatalf("volume %d", sinks[0].Volume)
	}
}

func TestResolveHint(t *testing.T) {
	sinks := ParseList(paSinks)
	target, mids, err := Resolve(sinks, "echo-cancel-sink", "Generic_USB")
	if err != nil {
		t.Fatal(err)
	}
	if target != "alsa_output.usb-Generic_USB_Audio-00.analog-stereo" || mids != nil {
		t.Fatalf("hint target %q mids %v", target, mids)
	}
	if _, _, err := Resolve(sinks, "echo-cancel-sink", "a"); err == nil {
		t.Fatal("ambiguous hint accepted")
	}
	if _, _, err := Resolve(sinks, "echo-cancel-sink", "missing"); err == nil {
		t.Fatal("unknown hint accepted")
	}
	if _, _, err := Resolve(sinks, "", ""); err == nil {
		t.Fatal("missing default accepted")
	}
}

func TestSetTargetsHardwareAndClamps(t *testing.T) {
	var cmds []string
	lists := map[string]string{"sinks": paSinks, "sources": paSources}
	p := &Pactl{run: func(args ...string) (string, error) {
		cmds = append(cmds, strings.Join(args, " "))
		switch args[0] {
		case "info":
			return paInfo, nil
		case "list":
			return lists[args[1]], nil
		default:
			return "", nil
		}
	}}
	speaker, mic := 250, -5
	if _, _, err := p.Set(&speaker, &mic); err != nil {
		t.Fatal(err)
	}
	var sets []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "set-") {
			sets = append(sets, c)
		}
	}
	want := []string{
		"set-sink-volume echo-cancel-sink 100%",
		"set-sink-volume alsa_output.usb-Generic_USB_Audio-00.analog-stereo 200%",
		"set-source-volume echo-cancel-source 100%",
		"set-source-volume usb-mic-mono 100%",
		"set-source-volume alsa_input.usb-Generic_USB_Audio-00.analog-stereo 0%",
	}
	if strings.Join(sets, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s", strings.Join(sets, "\n"))
	}
}

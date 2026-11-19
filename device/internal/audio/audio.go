// Package audio 通过 miniaudio（malgo）读写声卡。
// 在香橙派上走 PulseAudio 后端。bookworm 上实际连接 PipeWire 的回声消除设备，
// Ubuntu 18.04 上连接 PulseAudio module-echo-cancel 的同名虚拟设备。
package audio

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/gen2brain/malgo"
)

type Config struct {
	// Backend: auto | pulseaudio | alsa | null（null 用于无声卡环境调试）
	Backend string
	// CaptureDevice / PlaybackDevice 为设备名子串，空表示系统默认设备
	CaptureDevice  string
	PlaybackDevice string
	SampleRate     int
	FrameSize      int
	PeriodMs       int
}

// FrameFunc 每采集满一帧调用一次；在音频线程中执行，不能阻塞。
type FrameFunc func(pcm []int16)

// PullFunc 由播放线程调用，需要填满 out；在音频线程中执行，不能阻塞。
type PullFunc func(out []int16)

type Engine struct {
	ctx      *malgo.AllocatedContext
	capture  *malgo.Device
	playback *malgo.Device
	once     sync.Once
}

func backends(name string) ([]malgo.Backend, error) {
	switch name {
	case "", "auto":
		return []malgo.Backend{malgo.BackendPulseaudio, malgo.BackendAlsa}, nil
	case "pulseaudio", "pipewire":
		return []malgo.Backend{malgo.BackendPulseaudio}, nil
	case "alsa":
		return []malgo.Backend{malgo.BackendAlsa}, nil
	case "null":
		return []malgo.Backend{malgo.BackendNull}, nil
	}
	return nil, fmt.Errorf("unknown audio backend %q", name)
}

func findDevice(ctx *malgo.AllocatedContext, kind malgo.DeviceType, name string) (*malgo.DeviceID, error) {
	if name == "" {
		return nil, nil
	}
	infos, err := ctx.Devices(kind)
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range infos {
		n := infos[i].Name()
		names = append(names, n)
		if strings.Contains(strings.ToLower(n), strings.ToLower(name)) {
			id := infos[i].ID
			return &id, nil
		}
	}
	return nil, fmt.Errorf("audio device %q not found, available: %q", name, names)
}

// Open 打开采集和播放设备并开始工作。onFrame 或 pull 为 nil 时不打开对应设备。
func Open(cfg Config, onFrame FrameFunc, pull PullFunc) (*Engine, error) {
	bs, err := backends(cfg.Backend)
	if err != nil {
		return nil, err
	}
	ctx, err := malgo.InitContext(bs, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio context: %w", err)
	}
	e := &Engine{ctx: ctx}
	if cfg.PeriodMs <= 0 {
		cfg.PeriodMs = 10
	}

	if onFrame != nil {
		if e.capture, err = e.openCapture(cfg, onFrame); err != nil {
			e.Close()
			return nil, err
		}
	}
	if pull != nil {
		if e.playback, err = e.openPlayback(cfg, pull); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}

func (e *Engine) baseConfig(kind malgo.DeviceType, cfg Config) malgo.DeviceConfig {
	dc := malgo.DefaultDeviceConfig(kind)
	dc.SampleRate = uint32(cfg.SampleRate)
	dc.PeriodSizeInMilliseconds = uint32(cfg.PeriodMs)
	dc.Periods = 3
	dc.PerformanceProfile = malgo.LowLatency
	return dc
}

func (e *Engine) openCapture(cfg Config, onFrame FrameFunc) (*malgo.Device, error) {
	dc := e.baseConfig(malgo.Capture, cfg)
	dc.Capture.Format = malgo.FormatS16
	dc.Capture.Channels = 1
	id, err := findDevice(e.ctx, malgo.Capture, cfg.CaptureDevice)
	if err != nil {
		return nil, err
	}
	if id != nil {
		dc.Capture.DeviceID = id.Pointer()
	}

	frame := make([]int16, 0, cfg.FrameSize)
	cb := malgo.DeviceCallbacks{
		Data: func(_, in []byte, frames uint32) {
			for i := 0; i+1 < len(in); i += 2 {
				frame = append(frame, int16(binary.LittleEndian.Uint16(in[i:])))
				if len(frame) == cfg.FrameSize {
					onFrame(frame)
					frame = frame[:0]
				}
			}
		},
	}
	dev, err := malgo.InitDevice(e.ctx.Context, dc, cb)
	if err != nil {
		return nil, fmt.Errorf("open capture: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, fmt.Errorf("start capture: %w", err)
	}
	slog.Info("audio capture started", "device", deviceLabel(cfg.CaptureDevice), "rate", cfg.SampleRate)
	return dev, nil
}

func (e *Engine) openPlayback(cfg Config, pull PullFunc) (*malgo.Device, error) {
	dc := e.baseConfig(malgo.Playback, cfg)
	dc.Playback.Format = malgo.FormatS16
	dc.Playback.Channels = 1
	id, err := findDevice(e.ctx, malgo.Playback, cfg.PlaybackDevice)
	if err != nil {
		return nil, err
	}
	if id != nil {
		dc.Playback.DeviceID = id.Pointer()
	}

	var pcm []int16
	cb := malgo.DeviceCallbacks{
		Data: func(out, _ []byte, frames uint32) {
			n := len(out) / 2
			if cap(pcm) < n {
				pcm = make([]int16, n)
			}
			pcm = pcm[:n]
			pull(pcm)
			for i, s := range pcm {
				binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
			}
		},
	}
	dev, err := malgo.InitDevice(e.ctx.Context, dc, cb)
	if err != nil {
		return nil, fmt.Errorf("open playback: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, fmt.Errorf("start playback: %w", err)
	}
	slog.Info("audio playback started", "device", deviceLabel(cfg.PlaybackDevice), "rate", cfg.SampleRate)
	return dev, nil
}

func deviceLabel(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

func (e *Engine) Close() {
	e.once.Do(func() {
		for _, d := range []*malgo.Device{e.capture, e.playback} {
			if d != nil {
				_ = d.Stop()
				d.Uninit()
			}
		}
		if e.ctx != nil {
			_ = e.ctx.Uninit()
			e.ctx.Free()
		}
	})
}

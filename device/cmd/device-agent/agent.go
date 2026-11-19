package main

import (
	"context"
	"log/slog"
	"os"
	ossignal "os/signal"
	"sync"
	"syscall"

	"nexcall/device/internal/agent"
	"nexcall/device/internal/audio"
	"nexcall/device/internal/codec"
	"nexcall/device/internal/config"
	"nexcall/device/internal/gpio"
	"nexcall/device/internal/rtc"
	"nexcall/device/internal/signal"
)

func runAgent(ctx context.Context, cfg *config.Config) error {
	sig, err := signal.New(signal.Config{
		ServerURL: cfg.ServerURL,
		DeviceID:  cfg.DeviceID,
		Secret:    cfg.DeviceSecret,
		CACert:    cfg.CACert,
	})
	if err != nil {
		return err
	}

	panel, err := gpio.Open(gpio.Config{
		Chip:        cfg.GPIOChip,
		ButtonLine:  cfg.ButtonLine,
		LEDLine:     cfg.LEDLine,
		LEDActiveLo: cfg.LEDActiveLo,
		LongPress:   cfg.LongPress,
	})
	if err != nil {
		return err
	}
	defer panel.Close()

	opts := sessionOptions(cfg)
	join := func(url, token string) (agent.Media, error) {
		return rtc.Join(url, token, opts)
	}
	ringer := &toneRinger{audio: opts.Audio, enabled: cfg.RingTone}
	defer ringer.Stop()

	a := agent.New(agent.Config{AutoAnswer: cfg.AutoAnswer, CallTarget: cfg.CallTarget}, sig, join, ringer, panel)

	go simulateButton(ctx, panel)

	slog.Info("device-agent starting", "server", cfg.ServerURL, "device", cfg.DeviceID, "auto_answer", cfg.AutoAnswer)
	go sig.Run(ctx)
	a.Run(ctx, sig.Messages, panel.Presses())
	return ctx.Err()
}

// simulateButton: SIGUSR1 等同短按（呼叫/接听/挂断），SIGUSR2 等同长按（拒接/挂断）。
func simulateButton(ctx context.Context, panel *gpio.Panel) {
	ch := make(chan os.Signal, 1)
	ossignal.Notify(ch, syscall.SIGUSR1, syscall.SIGUSR2)
	defer ossignal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-ch:
			if s == syscall.SIGUSR1 {
				panel.Inject(gpio.Short)
			} else {
				panel.Inject(gpio.Long)
			}
		}
	}
}

// toneRinger 通过喇叭播放来电铃声或回铃音。
type toneRinger struct {
	audio   audio.Config
	enabled bool

	mu     sync.Mutex
	engine *audio.Engine
}

func (r *toneRinger) Start(incoming bool) {
	if !r.enabled {
		return
	}
	r.Stop()
	tone := audio.Ringtone(codec.SampleRate)
	if !incoming {
		tone = audio.NewTone(codec.SampleRate, 0.15, 1000, 3000, 450)
	}
	var mu sync.Mutex
	e, err := audio.Open(r.audio, nil, func(out []int16) {
		mu.Lock()
		tone.Fill(out)
		mu.Unlock()
	})
	if err != nil {
		slog.Warn("ring tone unavailable", "err", err)
		return
	}
	r.mu.Lock()
	r.engine = e
	r.mu.Unlock()
}

func (r *toneRinger) Stop() {
	r.mu.Lock()
	e := r.engine
	r.engine = nil
	r.mu.Unlock()
	if e != nil {
		e.Close()
	}
}

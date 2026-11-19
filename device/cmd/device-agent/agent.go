package main

import (
	"context"
	"log/slog"
	"os"
	ossignal "os/signal"
	"syscall"

	"nexcall/device/internal/agent"
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
	a := agent.New(agent.Config{DeviceID: cfg.DeviceID, CallTarget: cfg.CallTarget}, sig, join, panel)

	go simulateButton(ctx, panel)

	slog.Info("device-agent starting", "server", cfg.ServerURL, "device", cfg.DeviceID)
	go sig.Run(ctx)
	a.Run(ctx, sig.Messages, panel.Presses())
	return ctx.Err()
}

// simulateButton: SIGUSR1 等同短按（提醒网页用户，或在通话中切换静音）。
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

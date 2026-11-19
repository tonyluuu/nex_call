package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"nexcall/device/internal/audio"
	"nexcall/device/internal/codec"
	"nexcall/device/internal/config"
	"nexcall/device/internal/rtc"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	setupLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Token != "" {
		err = runDirect(ctx, cfg)
	} else {
		err = runAgent(ctx, cfg)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func setupLogger(level string) {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

func sessionOptions(cfg *config.Config) rtc.Options {
	return rtc.Options{
		Audio: audio.Config{
			Backend:        cfg.AudioBackend,
			CaptureDevice:  cfg.CaptureDevice,
			PlaybackDevice: cfg.PlaybackDevice,
			SampleRate:     codec.SampleRate,
			FrameSize:      codec.FrameSize,
			PeriodMs:       cfg.AudioPeriodMs,
		},
		Encoder: codec.EncoderConfig{
			Bitrate:        cfg.Bitrate,
			PacketLossPerc: cfg.PacketLossPerc,
			DTX:            cfg.DTX,
		},
		JitterTargetMs: cfg.JitterTargetMs,
		JitterMaxMs:    cfg.JitterMaxMs,
		TestTone:       cfg.TestTone,
		StatsInterval:  cfg.StatsInterval,
	}
}

// runDirect 调试用：不经过 call-server，直接用 Token 进房通话，Ctrl+C 退出。
func runDirect(ctx context.Context, cfg *config.Config) error {
	s, err := rtc.Join(cfg.LiveKitURL, cfg.Token, sessionOptions(cfg))
	if err != nil {
		return err
	}
	defer s.Close()
	select {
	case <-ctx.Done():
	case <-s.Done():
	}
	return nil
}

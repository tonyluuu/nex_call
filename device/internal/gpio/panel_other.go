//go:build !linux

package gpio

import "log/slog"

func Open(cfg Config) (*Panel, error) {
	if cfg.ButtonLine >= 0 || cfg.LEDLine >= 0 {
		slog.Warn("gpio is only supported on linux, ignoring BUTTON_LINE / LED_LINE")
	}
	p := newPanel()
	go p.runLED()
	return p, nil
}

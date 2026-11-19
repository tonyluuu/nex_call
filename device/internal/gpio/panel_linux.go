//go:build linux

package gpio

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/warthog618/go-gpiocdev"
)

// Open 申请 GPIO。按键接在 GPIO 与 GND 之间（内部上拉，低电平为按下）。
func Open(cfg Config) (*Panel, error) {
	p := newPanel()
	logDisabled(cfg)

	if cfg.LEDLine >= 0 {
		led, err := gpiocdev.RequestLine(cfg.Chip, cfg.LEDLine, gpiocdev.AsOutput(0), gpiocdev.WithConsumer("nexcall-led"))
		if err != nil {
			return nil, fmt.Errorf("request led line %s/%d: %w", cfg.Chip, cfg.LEDLine, err)
		}
		p.setLED = func(on bool) {
			v := 0
			if on != cfg.LEDActiveLo {
				v = 1
			}
			_ = led.SetValue(v)
		}
		p.closers = append(p.closers, func() { led.Close() })
	}

	if cfg.ButtonLine >= 0 {
		tracker := &pressTracker{long: cfg.LongPress}
		btn, err := gpiocdev.RequestLine(cfg.Chip, cfg.ButtonLine,
			gpiocdev.AsInput,
			gpiocdev.WithPullUp,
			gpiocdev.AsActiveLow,
			gpiocdev.WithBothEdges,
			gpiocdev.WithDebounce(30*time.Millisecond),
			gpiocdev.WithConsumer("nexcall-button"),
			gpiocdev.WithEventHandler(func(evt gpiocdev.LineEvent) {
				if pr, ok := tracker.edge(evt.Type == gpiocdev.LineEventRisingEdge, evt.Timestamp); ok {
					slog.Debug("button", "long", pr == Long)
					p.emit(pr)
				}
			}),
		)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("request button line %s/%d: %w", cfg.Chip, cfg.ButtonLine, err)
		}
		p.closers = append(p.closers, func() { btn.Close() })
	}

	go p.runLED()
	return p, nil
}

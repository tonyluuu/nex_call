// Package gpio 提供按键和状态灯：按键区分短按/长按，LED 按状态显示不同闪烁节奏。
package gpio

import (
	"log/slog"
	"sync"
	"time"
)

type Press int

const (
	Short Press = iota
	Long
)

type Pattern int

const (
	// Offline 未连上服务器：慢闪
	Offline Pattern = iota
	// Idle 在线待机：每 3 秒短亮一次
	Idle
	// Calling 保留的中速闪
	Calling
	// Incoming 通话中本地静音：快闪
	Incoming
	// Active 有人在房间且麦克风打开：常亮
	Active
)

// 每个模式是一组交替的 亮/灭 时长
var patterns = map[Pattern][]time.Duration{
	Offline:  {time.Second, time.Second},
	Idle:     {80 * time.Millisecond, 3 * time.Second},
	Calling:  {500 * time.Millisecond, 500 * time.Millisecond},
	Incoming: {120 * time.Millisecond, 120 * time.Millisecond},
	Active:   {time.Hour, 0},
}

type Config struct {
	Chip        string
	ButtonLine  int
	LEDLine     int
	LEDActiveLo bool
	LongPress   time.Duration
}

type Panel struct {
	presses chan Press
	pattern chan Pattern
	setLED  func(on bool)
	closers []func()
	once    sync.Once
	done    chan struct{}
}

func (p *Panel) Presses() <-chan Press { return p.presses }

func (p *Panel) SetPattern(pt Pattern) {
	select {
	case p.pattern <- pt:
	default:
	}
}

func (p *Panel) Close() {
	p.once.Do(func() {
		close(p.done)
		for _, c := range p.closers {
			c()
		}
	})
}

func newPanel() *Panel {
	return &Panel{
		presses: make(chan Press, 4),
		pattern: make(chan Pattern, 4),
		done:    make(chan struct{}),
	}
}

// Inject 模拟一次按键（无物理按键时通过信号触发）。
func (p *Panel) Inject(pr Press) { p.emit(pr) }

func (p *Panel) emit(pr Press) {
	select {
	case p.presses <- pr:
	default:
	}
}

// pressTracker 把按下/松开的边沿转换成短按或长按。
type pressTracker struct {
	long    time.Duration
	pressed bool
	at      time.Duration
}

func (t *pressTracker) edge(down bool, ts time.Duration) (Press, bool) {
	if down {
		t.pressed, t.at = true, ts
		return 0, false
	}
	if !t.pressed {
		return 0, false
	}
	t.pressed = false
	if ts-t.at >= t.long {
		return Long, true
	}
	return Short, true
}

func (p *Panel) runLED() {
	if p.setLED == nil {
		for {
			select {
			case <-p.done:
				return
			case <-p.pattern:
			}
		}
	}
	current := patterns[Offline]
	step := 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-p.done:
			p.setLED(false)
			return
		case pt := <-p.pattern:
			current = patterns[pt]
			step = 0
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(0)
		case <-timer.C:
			d := current[step%len(current)]
			if d > 0 {
				p.setLED(step%2 == 0)
				timer.Reset(d)
			} else {
				timer.Reset(time.Hour)
			}
			step++
		}
	}
}

func logDisabled(cfg Config) {
	if cfg.ButtonLine < 0 && cfg.LEDLine < 0 {
		slog.Info("gpio disabled (BUTTON_LINE / LED_LINE not set)")
	}
}

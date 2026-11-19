// Package config 从环境变量和命令行参数加载 device-agent 配置，命令行优先。
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// 业务服务地址，例如 https://203.0.113.10
	ServerURL    string
	DeviceID     string
	DeviceSecret string
	// CACert 为自签名证书兜底时固定信任的证书文件；为空则使用系统根证书
	CACert string

	// CallTarget 短按提醒的 Web 用户名，空表示提醒所有在线用户
	CallTarget string
	// CallStateFile 写出 offline / idle / active，供音乐播放器等本机程序读取；空表示不写
	CallStateFile string

	AudioBackend   string
	CaptureDevice  string
	PlaybackDevice string
	AudioPeriodMs  int
	Bitrate        int
	PacketLossPerc int
	DTX            bool
	JitterTargetMs int
	JitterMaxMs    int
	PlaybackGain   float64
	StatsInterval  time.Duration

	GPIOChip    string
	ButtonLine  int
	LEDLine     int
	LEDActiveLo bool
	LongPress   time.Duration

	LogLevel string

	// 直连模式（调试用）：跳过业务服务，直接用 Token 进房
	LiveKitURL string
	Token      string
	TestTone   bool
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("device-agent", flag.ContinueOnError)

	fs.StringVar(&c.ServerURL, "server", env("SERVER_URL", ""), "call-server 地址，如 https://203.0.113.10 [SERVER_URL]")
	fs.StringVar(&c.DeviceID, "device-id", env("DEVICE_ID", ""), "设备 ID [DEVICE_ID]")
	fs.StringVar(&c.DeviceSecret, "device-secret", env("DEVICE_SECRET", ""), "设备密钥 [DEVICE_SECRET]")
	fs.StringVar(&c.CACert, "ca-cert", env("CA_CERT", ""), "固定信任的服务端证书（自签名时使用）[CA_CERT]")

	fs.StringVar(&c.CallTarget, "call-target", env("CALL_TARGET", ""), "短按提醒的 Web 用户，空为全部在线用户 [CALL_TARGET]")

	fs.StringVar(&c.CallStateFile, "call-state-file", env("CALL_STATE_FILE", "/run/nexcall/call-state"), "通话状态文件，空为不写 [CALL_STATE_FILE]")

	fs.StringVar(&c.AudioBackend, "audio-backend", env("AUDIO_BACKEND", "auto"), "auto|pulseaudio|alsa|null [AUDIO_BACKEND]")
	fs.StringVar(&c.CaptureDevice, "capture-device", env("CAPTURE_DEVICE", ""), "采集设备名子串，空为默认 [CAPTURE_DEVICE]")
	fs.StringVar(&c.PlaybackDevice, "playback-device", env("PLAYBACK_DEVICE", ""), "播放设备名子串，空为默认 [PLAYBACK_DEVICE]")
	fs.IntVar(&c.AudioPeriodMs, "audio-period-ms", envInt("AUDIO_PERIOD_MS", 10), "声卡缓冲周期 [AUDIO_PERIOD_MS]")
	fs.IntVar(&c.Bitrate, "bitrate", envInt("OPUS_BITRATE", 24000), "Opus 码率 bps [OPUS_BITRATE]")
	fs.IntVar(&c.PacketLossPerc, "packet-loss-perc", envInt("OPUS_PACKET_LOSS_PERC", 10), "预期丢包率，影响 FEC 冗余量 [OPUS_PACKET_LOSS_PERC]")
	fs.BoolVar(&c.DTX, "dtx", envBool("OPUS_DTX", true), "静音不发包 [OPUS_DTX]")
	fs.IntVar(&c.JitterTargetMs, "jitter-target-ms", envInt("JITTER_TARGET_MS", 60), "抖动缓冲起播深度 [JITTER_TARGET_MS]")
	fs.IntVar(&c.JitterMaxMs, "jitter-max-ms", envInt("JITTER_MAX_MS", 300), "抖动缓冲上限 [JITTER_MAX_MS]")
	fs.Float64Var(&c.PlaybackGain, "playback-gain", envFloat("PLAYBACK_GAIN", 1), "通话播放增益倍数，1 为原样，2 约 +6dB [PLAYBACK_GAIN]")
	fs.DurationVar(&c.StatsInterval, "stats-interval", envDuration("STATS_INTERVAL", 10*time.Second), "通话统计日志间隔 [STATS_INTERVAL]")

	fs.StringVar(&c.GPIOChip, "gpio-chip", env("GPIO_CHIP", "gpiochip0"), "GPIO 控制器 [GPIO_CHIP]")
	fs.IntVar(&c.ButtonLine, "button-line", envInt("BUTTON_LINE", -1), "按键 GPIO 线号，-1 禁用 [BUTTON_LINE]")
	fs.IntVar(&c.LEDLine, "led-line", envInt("LED_LINE", -1), "LED GPIO 线号，-1 禁用 [LED_LINE]")
	fs.BoolVar(&c.LEDActiveLo, "led-active-low", envBool("LED_ACTIVE_LOW", false), "LED 低电平点亮 [LED_ACTIVE_LOW]")
	fs.DurationVar(&c.LongPress, "long-press", envDuration("LONG_PRESS", 1500*time.Millisecond), "长按判定时长 [LONG_PRESS]")

	fs.StringVar(&c.LogLevel, "log-level", env("LOG_LEVEL", "info"), "debug|info|warn|error [LOG_LEVEL]")

	fs.StringVar(&c.LiveKitURL, "livekit-url", env("LIVEKIT_URL", ""), "直连模式：LiveKit 地址 wss://IP")
	fs.StringVar(&c.Token, "token", env("LIVEKIT_TOKEN", ""), "直连模式：LiveKit Token")
	fs.BoolVar(&c.TestTone, "test-tone", envBool("TEST_TONE", false), "用 440Hz 测试音代替麦克风 [TEST_TONE]")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.PlaybackGain <= 0 || c.PlaybackGain > 8 {
		return nil, fmt.Errorf("PLAYBACK_GAIN 需在 0 到 8 之间，当前 %v", c.PlaybackGain)
	}

	if c.Token != "" {
		if c.LiveKitURL == "" {
			return nil, fmt.Errorf("直连模式需要 -livekit-url")
		}
		return c, nil
	}
	if c.ServerURL == "" || c.DeviceID == "" || c.DeviceSecret == "" {
		return nil, fmt.Errorf("需要 SERVER_URL、DEVICE_ID、DEVICE_SECRET（或使用 -token 直连模式）")
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	return c, nil
}

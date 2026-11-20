// Package volume 通过 pactl 调节系统默认喇叭和麦克风的音量。
// 音量设在回声消除链路最底层的硬件设备上：0–100% 由声卡硬件完成，
// 超过 100% 时硬件保持满幅，多出来的部分由 PulseAudio 或 PipeWire 做软件放大。
// 中间的虚拟设备固定为 100%，避免增益叠在一起。
package volume

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const maxVolume = 200

var percentRe = regexp.MustCompile(`(\d+)\s*%`)

// Node 是 pactl list 里的一个 sink 或 source。
type Node struct {
	Name   string
	Volume int
	HasVol bool
	Master string
}

// Pactl 调用本机 pactl。Sink / Source 非空时按名称子串指定目标设备，不再沿 master 链查找。
type Pactl struct {
	Sink   string
	Source string
	run    func(args ...string) (string, error)
}

func New(sink, source string) *Pactl {
	return &Pactl{Sink: sink, Source: source}
}

func (p *Pactl) exec(args ...string) (string, error) {
	if p.run != nil {
		return p.run(args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("pactl %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("pactl %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return string(out), nil
}

// Get 返回喇叭和麦克风当前的百分比。
func (p *Pactl) Get() (speaker, mic int, err error) {
	sinks, sources, defSink, defSource, err := p.snapshot()
	if err != nil {
		return 0, 0, err
	}
	speaker, err = level(sinks, defSink, p.Sink)
	if err != nil {
		return 0, 0, err
	}
	mic, err = level(sources, defSource, p.Source)
	if err != nil {
		return 0, 0, err
	}
	return speaker, mic, nil
}

// Set 修改非 nil 的那一路，并返回设置后读到的两路音量。
func (p *Pactl) Set(speaker, mic *int) (int, int, error) {
	sinks, sources, defSink, defSource, err := p.snapshot()
	if err != nil {
		return 0, 0, err
	}
	if speaker != nil {
		if err := p.apply("sink", sinks, defSink, p.Sink, *speaker); err != nil {
			return 0, 0, err
		}
	}
	if mic != nil {
		if err := p.apply("source", sources, defSource, p.Source, *mic); err != nil {
			return 0, 0, err
		}
	}
	return p.Get()
}

func (p *Pactl) apply(kind string, nodes []Node, defName, hint string, value int) error {
	target, mids, err := Resolve(nodes, defName, hint)
	if err != nil {
		return err
	}
	verb := "set-sink-volume"
	if kind == "source" {
		verb = "set-source-volume"
	}
	for _, mid := range mids {
		if _, err := p.exec(verb, mid, "100%"); err != nil {
			return err
		}
	}
	_, err = p.exec(verb, target, fmt.Sprintf("%d%%", clamp(value)))
	return err
}

func (p *Pactl) snapshot() (sinks, sources []Node, defSink, defSource string, err error) {
	info, err := p.exec("info")
	if err != nil {
		return nil, nil, "", "", err
	}
	defSink, defSource = ParseInfo(info)
	sinkText, err := p.exec("list", "sinks")
	if err != nil {
		return nil, nil, "", "", err
	}
	sourceText, err := p.exec("list", "sources")
	if err != nil {
		return nil, nil, "", "", err
	}
	return ParseList(sinkText), ParseList(sourceText), defSink, defSource, nil
}

func level(nodes []Node, defName, hint string) (int, error) {
	target, _, err := Resolve(nodes, defName, hint)
	if err != nil {
		return 0, err
	}
	for _, n := range nodes {
		if n.Name == target && n.HasVol {
			return n.Volume, nil
		}
	}
	return 0, fmt.Errorf("volume of %s unknown", target)
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxVolume {
		return maxVolume
	}
	return v
}

// ParseInfo 从 `pactl info` 取出默认 sink 和 source 的名字。
func ParseInfo(text string) (sink, source string) {
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(trim, "Default Sink:"); ok {
			sink = strings.TrimSpace(name)
		}
		if name, ok := strings.CutPrefix(trim, "Default Source:"); ok {
			source = strings.TrimSpace(name)
		}
	}
	return sink, source
}

// ParseList 解析 `pactl list sinks` 或 `pactl list sources`。
func ParseList(text string) []Node {
	var out []Node
	var cur *Node
	flush := func() {
		if cur != nil && cur.Name != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "Sink #") || strings.HasPrefix(trim, "Source #") {
			flush()
			cur = &Node{}
			continue
		}
		if cur == nil {
			continue
		}
		if name, ok := strings.CutPrefix(trim, "Name:"); ok {
			cur.Name = strings.TrimSpace(name)
			continue
		}
		if strings.HasPrefix(trim, "Volume:") && !cur.HasVol {
			if v, ok := firstPercent(trim); ok {
				cur.Volume = v
				cur.HasVol = true
			}
			continue
		}
		if strings.Contains(trim, "device.master_device") {
			cur.Master = propValue(trim)
		}
	}
	flush()
	return out
}

func firstPercent(line string) (int, bool) {
	m := percentRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
}

func propValue(line string) string {
	_, v, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	v = strings.Trim(strings.TrimSpace(v), `"`)
	if v == "" || v == "(null)" {
		return ""
	}
	return v
}

// Resolve 决定真正要调节的设备。hint 按名称子串匹配；否则从默认设备沿
// device.master_device 走到没有上游的那一个。mids 是路上的虚拟设备，调用方把它们固定在 100%。
func Resolve(nodes []Node, defName, hint string) (target string, mids []string, err error) {
	if hint != "" {
		var match []Node
		want := strings.ToLower(hint)
		for _, n := range nodes {
			if strings.Contains(strings.ToLower(n.Name), want) {
				match = append(match, n)
			}
		}
		switch len(match) {
		case 1:
			return match[0].Name, nil, nil
		case 0:
			return "", nil, fmt.Errorf("no device matching %q", hint)
		default:
			return "", nil, fmt.Errorf("multiple devices matching %q", hint)
		}
	}
	if defName == "" {
		return "", nil, errors.New("no default device")
	}
	target, mids = follow(nodes, defName)
	return target, mids, nil
}

func follow(nodes []Node, start string) (string, []string) {
	by := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		by[n.Name] = n
	}
	cur, ok := by[start]
	if !ok {
		return start, nil
	}
	seen := map[string]bool{}
	var mids []string
	for {
		if cur.Master == "" || seen[cur.Name] {
			return cur.Name, mids
		}
		next, ok := by[cur.Master]
		if !ok {
			return cur.Name, mids
		}
		seen[cur.Name] = true
		mids = append(mids, cur.Name)
		cur = next
	}
}

// Package codec 封装 libopus，固定 48kHz 单声道、20ms 帧。
package codec

import (
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

const (
	SampleRate = 48000
	Channels   = 1
	FrameMs    = 20
	FrameSize  = SampleRate * FrameMs / 1000 // 960 samples
	maxPacket  = 1500
	// 浏览器可能发送最长 120ms 的 Opus 帧
	maxFrameSize = SampleRate * 120 / 1000
)

type EncoderConfig struct {
	Bitrate        int
	PacketLossPerc int
	DTX            bool
}

type Encoder struct {
	enc *opus.Encoder
	buf []byte
}

func NewEncoder(cfg EncoderConfig) (*Encoder, error) {
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = 24000
	}
	settings := []error{
		enc.SetBitrate(cfg.Bitrate),
		enc.SetInBandFEC(true),
		enc.SetPacketLossPerc(cfg.PacketLossPerc),
		enc.SetDTX(cfg.DTX),
		enc.SetComplexity(5),
		enc.SetMaxBandwidth(opus.Wideband),
	}
	for _, err := range settings {
		if err != nil {
			return nil, fmt.Errorf("opus encoder settings: %w", err)
		}
	}
	return &Encoder{enc: enc, buf: make([]byte, maxPacket)}, nil
}

// Encode 编码一帧 FrameSize 个采样，返回的切片在下次调用前有效。
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	n, err := e.enc.Encode(pcm, e.buf)
	if err != nil {
		return nil, err
	}
	return e.buf[:n], nil
}

type Decoder struct {
	dec       *opus.Decoder
	buf       []int16
	lastFrame int
}

func NewDecoder() (*Decoder, error) {
	dec, err := opus.NewDecoder(SampleRate, Channels)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	return &Decoder{dec: dec, buf: make([]int16, maxFrameSize), lastFrame: FrameSize}, nil
}

// Decode 解码一个包，返回的切片在下次调用前有效。
func (d *Decoder) Decode(packet []byte) ([]int16, error) {
	n, err := d.dec.Decode(packet, d.buf)
	if err != nil {
		return nil, err
	}
	d.lastFrame = n
	return d.buf[:n], nil
}

// DecodeFEC 利用下一个包里的带内 FEC 恢复丢失的一帧。
func (d *Decoder) DecodeFEC(next []byte) ([]int16, error) {
	pcm := d.buf[:d.lastFrame]
	if err := d.dec.DecodeFEC(next, pcm); err != nil {
		return nil, err
	}
	return pcm, nil
}

// DecodePLC 在没有任何冗余信息时做丢包补偿。
func (d *Decoder) DecodePLC() ([]int16, error) {
	pcm := d.buf[:d.lastFrame]
	if err := d.dec.DecodePLC(pcm); err != nil {
		return nil, err
	}
	return pcm, nil
}

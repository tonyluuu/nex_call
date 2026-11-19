// Package rtc 管理一次通话的 LiveKit 房间连接和音频收发。
package rtc

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"nexcall/device/internal/audio"
	"nexcall/device/internal/codec"
	"nexcall/device/internal/playout"
)

type Options struct {
	Audio          audio.Config
	Encoder        codec.EncoderConfig
	JitterTargetMs int
	JitterMaxMs    int
	// PlaybackGain 对收到的通话声音做软件放大，1 为原样播放
	PlaybackGain float64
	// TestTone 用 440Hz 正弦波代替麦克风，便于在没有麦克风时验证链路
	TestTone      bool
	StatsInterval time.Duration
}

type Session struct {
	opts   Options
	room   *lksdk.Room
	engine *audio.Engine
	enc    *codec.Encoder
	track  *webrtc.TrackLocalStaticSample
	frames chan []int16

	mu      sync.Mutex
	players map[string]*playout.Player
	muted   atomic.Bool

	occ       chan int
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once

	txFrames atomic.Uint64
	txDrops  atomic.Uint64
	rxTracks atomic.Uint64
}

func Join(url, token string, opts Options) (*Session, error) {
	enc, err := codec.NewEncoder(opts.Encoder)
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   codec.SampleRate,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}, "audio", "mic")
	if err != nil {
		return nil, err
	}
	if opts.StatsInterval == 0 {
		opts.StatsInterval = 10 * time.Second
	}

	s := &Session{
		opts:    opts,
		enc:     enc,
		track:   track,
		frames:  make(chan []int16, 8),
		players: map[string]*playout.Player{},
		occ:     make(chan int, 4),
		done:    make(chan struct{}),
	}
	s.muted.Store(true)

	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = s.onTrackSubscribed
	cb.OnTrackUnsubscribed = s.onTrackUnsubscribed
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) {
		slog.Info("participant left", "identity", rp.Identity())
		s.publishCount()
	}
	cb.OnParticipantConnected = func(rp *lksdk.RemoteParticipant) {
		slog.Info("participant joined", "identity", rp.Identity())
		s.publishCount()
	}
	cb.OnReconnecting = func() { slog.Warn("livekit reconnecting") }
	cb.OnReconnected = func() { slog.Info("livekit reconnected") }
	cb.OnDisconnected = func() {
		slog.Info("livekit disconnected")
		s.doneOnce.Do(func() { close(s.done) })
	}

	room, err := lksdk.ConnectToRoomWithToken(url, token, cb, lksdk.WithAutoSubscribe(true))
	if err != nil {
		return nil, fmt.Errorf("connect livekit: %w", err)
	}
	s.room = room
	slog.Info("joined room", "room", room.Name(), "identity", room.LocalParticipant.Identity())

	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:       "microphone",
		Source:     livekit.TrackSource_MICROPHONE,
		DisableDTX: !opts.Encoder.DTX,
	}); err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("publish track: %w", err)
	}

	var onFrame audio.FrameFunc
	if !opts.TestTone {
		onFrame = s.onCaptureFrame
	}
	engine, err := audio.Open(opts.Audio, onFrame, s.pull)
	if err != nil {
		room.Disconnect()
		return nil, err
	}
	s.engine = engine

	go s.encodeLoop()
	if opts.TestTone {
		go s.toneLoop()
	}
	go s.statsLoop()
	s.publishCount()
	return s, nil
}

// Occupancy 在远端参与者人数变化时送出最新人数。
func (s *Session) Occupancy() <-chan int { return s.occ }

// SetMuted 为 true 时不往房间送麦克风数据。
func (s *Session) SetMuted(muted bool) { s.muted.Store(muted) }

func (s *Session) publishCount() {
	n := 0
	if s.room != nil {
		n = len(s.room.GetRemoteParticipants())
	}
	select {
	case s.occ <- n:
	default:
		select {
		case <-s.occ:
		default:
		}
		select {
		case s.occ <- n:
		default:
		}
	}
}

// Done 在与 LiveKit 的连接断开后关闭。
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if s.engine != nil {
			s.engine.Close()
		}
		s.room.Disconnect()
		s.doneOnce.Do(func() { close(s.done) })
		close(s.frames)
		s.logStats()
	})
}

func (s *Session) onCaptureFrame(pcm []int16) {
	frame := make([]int16, len(pcm))
	copy(frame, pcm)
	select {
	case s.frames <- frame:
	default:
		s.txDrops.Add(1)
	}
}

func (s *Session) toneLoop() {
	tone := audio.NewTone(codec.SampleRate, 0.3, 0, 0, 440)
	t := time.NewTicker(codec.FrameMs * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			frame := make([]int16, codec.FrameSize)
			tone.Fill(frame)
			select {
			case s.frames <- frame:
			default:
				s.txDrops.Add(1)
			}
		}
	}
}

func (s *Session) encodeLoop() {
	for frame := range s.frames {
		pkt, err := s.enc.Encode(frame)
		if err != nil {
			slog.Warn("opus encode failed", "err", err)
			continue
		}
		if s.muted.Load() {
			continue
		}
		if err := s.track.WriteSample(media.Sample{Data: pkt, Duration: codec.FrameMs * time.Millisecond}); err != nil {
			continue
		}
		s.txFrames.Add(1)
	}
}

func (s *Session) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	dec, err := codec.NewDecoder()
	if err != nil {
		slog.Error("create decoder", "err", err)
		return
	}
	frameMs := codec.FrameMs
	target := max(1, s.opts.JitterTargetMs/frameMs)
	maxFrames := max(target*2, s.opts.JitterMaxMs/frameMs)
	p := playout.New(dec, codec.FrameSize, target, maxFrames)
	key := rp.Identity() + "/" + track.ID()

	s.mu.Lock()
	s.players[key] = p
	s.mu.Unlock()
	s.rxTracks.Add(1)
	slog.Info("subscribed remote audio", "identity", rp.Identity(), "codec", track.Codec().MimeType)

	go func() {
		defer s.removePlayer(key)
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(pkt.Payload) == 0 {
				continue
			}
			payload := make([]byte, len(pkt.Payload))
			copy(payload, pkt.Payload)
			p.Push(pkt.SequenceNumber, payload)
		}
	}()
}

func (s *Session) onTrackUnsubscribed(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	s.removePlayer(rp.Identity() + "/" + track.ID())
}

func (s *Session) removePlayer(key string) {
	s.mu.Lock()
	delete(s.players, key)
	s.mu.Unlock()
}

func (s *Session) pull(out []int16) {
	s.mu.Lock()
	players := make([]*playout.Player, 0, len(s.players))
	for _, p := range s.players {
		players = append(players, p)
	}
	s.mu.Unlock()
	if len(players) == 0 {
		clear(out)
		return
	}
	bufs := make([][]int16, len(players))
	for i, p := range players {
		bufs[i] = make([]int16, len(out))
		p.Pull(bufs[i])
	}
	playout.Mix(out, bufs)
	playout.ApplyGain(out, s.opts.PlaybackGain)
}

func (s *Session) statsLoop() {
	t := time.NewTicker(s.opts.StatsInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.logStats()
		}
	}
}

func (s *Session) logStats() {
	attrs := []any{"tx_frames", s.txFrames.Load(), "tx_drops", s.txDrops.Load()}
	s.mu.Lock()
	players := make([]*playout.Player, 0, len(s.players))
	for _, p := range s.players {
		players = append(players, p)
	}
	s.mu.Unlock()
	var st playout.Stats
	for _, p := range players {
		one := p.Stats()
		st.Pushed += one.Pushed
		st.Played += one.Played
		st.Lost += one.Lost
		st.FEC += one.FEC
		st.PLC += one.PLC
		st.Late += one.Late
		st.Dropped += one.Dropped
		st.Underruns += one.Underruns
		st.Stretched += one.Stretched
		if one.Depth > st.Depth {
			st.Depth = one.Depth
		}
		if one.Target > st.Target {
			st.Target = one.Target
		}
	}
	if len(players) > 0 {
		attrs = append(attrs,
			"tracks", len(players),
			"rx_packets", st.Pushed, "played", st.Played, "lost", st.Lost,
			"fec", st.FEC, "plc", st.PLC, "late", st.Late, "dropped", st.Dropped,
			"underruns", st.Underruns, "stretched", st.Stretched,
			"jitter_depth_ms", st.Depth*codec.FrameMs, "jitter_target_ms", st.Target*codec.FrameMs,
		)
	}
	slog.Info("call stats", attrs...)
}

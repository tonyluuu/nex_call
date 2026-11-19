import {
  ConnectionQuality,
  Room,
  RoomEvent,
  Track,
  type RemoteParticipant,
  type RemoteTrack,
} from 'livekit-client'

export interface RemotePeer {
  identity: string
  speaking: boolean
}

export interface CallEvents {
  onDisconnected: () => void
  onAudioBlocked: (blocked: boolean) => void
  onQuality: (text: string) => void
  onParticipants: (list: RemotePeer[]) => void
}

const qualityText: Record<string, string> = {
  [ConnectionQuality.Excellent]: '网络很好',
  [ConnectionQuality.Good]: '网络良好',
  [ConnectionQuality.Poor]: '网络较差',
  [ConnectionQuality.Lost]: '网络中断，正在重连',
}

// MediaCall 封装一次通话的 LiveKit 房间。
export class MediaCall {
  private room: Room
  private audioEls: HTMLMediaElement[] = []
  private closed = false

  constructor(private events: CallEvents) {
    this.room = new Room({
      adaptiveStream: false,
      dynacast: false,
      audioCaptureDefaults: {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
        channelCount: 1,
      },
      publishDefaults: {
        dtx: true,
        red: true,
      },
    })

    this.room
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack) => {
        if (track.kind !== Track.Kind.Audio) return
        const el = track.attach()
        el.style.display = 'none'
        document.body.appendChild(el)
        this.audioEls.push(el)
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack) => {
        for (const el of track.detach()) el.remove()
      })
      .on(RoomEvent.ParticipantConnected, () => this.emitParticipants())
      .on(RoomEvent.ParticipantDisconnected, () => this.emitParticipants())
      .on(RoomEvent.ActiveSpeakersChanged, () => this.emitParticipants())
      .on(RoomEvent.Disconnected, () => {
        if (!this.closed) this.events.onDisconnected()
      })
      .on(RoomEvent.AudioPlaybackStatusChanged, () => {
        this.events.onAudioBlocked(!this.room.canPlaybackAudio)
      })
      .on(RoomEvent.ConnectionQualityChanged, (q, p) => {
        if (p.isLocal) this.events.onQuality(qualityText[q] ?? '')
      })
  }

  async start(url: string, token: string) {
    await this.room.connect(url, token)
    await this.room.localParticipant.setMicrophoneEnabled(true)
    this.events.onAudioBlocked(!this.room.canPlaybackAudio)
    this.emitParticipants()
  }

  private emitParticipants() {
    const speaking = new Set(this.room.activeSpeakers.map((p) => p.identity))
    const list: RemotePeer[] = []
    this.room.remoteParticipants.forEach((p: RemoteParticipant) => {
      list.push({ identity: p.identity, speaking: speaking.has(p.identity) })
    })
    this.events.onParticipants(list)
  }

  startAudio() {
    return this.room.startAudio()
  }

  async setMuted(muted: boolean) {
    await this.room.localParticipant.setMicrophoneEnabled(!muted)
  }

  async close() {
    this.closed = true
    for (const el of this.audioEls) el.remove()
    this.audioEls = []
    await this.room.disconnect()
  }
}

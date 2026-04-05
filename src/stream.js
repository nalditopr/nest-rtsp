/**
 * Camera stream — connects to one Nest camera via Foyer WebRTC,
 * forwards H.264 RTP to ffmpeg for RTSP output.
 */

import { RTCPeerConnection, RTCRtpCodecParameters } from 'werift'
import { createSocket } from 'node:dgram'
import { spawn } from 'node:child_process'
import { writeFile, unlink } from 'node:fs/promises'
import { EventEmitter } from 'node:events'
import { pickPort } from './ports.js'

export class CameraStream extends EventEmitter {
  #name
  #deviceId
  #resolution
  #client
  #rtspPort
  #log

  #pc = null
  #udp = null
  #ffmpeg = null
  #sdpFile = null
  #running = false
  #failures = 0
  #retryTimer = null

  /**
   * @param {object} opts
   * @param {string} opts.name - Camera name (used as RTSP path)
   * @param {string} opts.deviceId - Foyer device ID
   * @param {number} opts.resolution - Video resolution (0-3)
   * @param {import('./foyer.js').FoyerClient} opts.client - Authenticated Foyer client
   * @param {number} opts.rtspPort - RTSP server port
   * @param {function} opts.log - Logger function
   */
  constructor({ name, deviceId, resolution = 3, client, rtspPort, log }) {
    super()
    this.#name = name
    this.#deviceId = deviceId
    this.#resolution = resolution
    this.#client = client
    this.#rtspPort = rtspPort
    this.#log = log || ((...args) => console.log(`[${name}]`, ...args))
  }

  get name() { return this.#name }
  get running() { return this.#running }

  async start() {
    this.#running = true
    this.#failures = 0
    await this.#connect()
  }

  async stop() {
    this.#running = false
    clearTimeout(this.#retryTimer)
    this.#cleanup()
    this.#log('stopped')
  }

  #cleanup() {
    this.#pc?.close()
    this.#pc = null
    this.#ffmpeg?.kill('SIGTERM')
    this.#ffmpeg = null
    this.#udp?.close()
    this.#udp = null
    if (this.#sdpFile) unlink(this.#sdpFile).catch(() => {})
  }

  async #connect() {
    try {
      await this.#doConnect()
    } catch (err) {
      this.#log(`error: ${err.message}`)
      this.#retry()
    }
  }

  #retry() {
    if (!this.#running) return
    this.#cleanup()
    this.#failures++
    const delay = Math.min(2000 * Math.pow(2, this.#failures - 1), 300000)
    this.#log(`retry in ${delay / 1000}s (failure #${this.#failures})`)
    this.#retryTimer = setTimeout(() => this.#connect(), delay)
  }

  async #doConnect() {
    const log = this.#log

    // Pick UDP ports
    const videoPort = await pickPort()
    const videoRtcpPort = await pickPort()
    const audioPort = await pickPort()
    const audioRtcpPort = await pickPort()
    const udp = createSocket('udp4')
    this.#udp = udp

    // WebRTC PeerConnection — offer VP9 + H.264 (Foyer prefers H.264)
    const pc = new RTCPeerConnection({
      bundlePolicy: 'max-bundle',
      codecs: {
        audio: [new RTCRtpCodecParameters({ mimeType: 'audio/opus', clockRate: 48000, channels: 2 })],
        video: [
          new RTCRtpCodecParameters({
            mimeType: 'video/VP9', clockRate: 90000,
            rtcpFeedback: [{ type: 'transport-cc' }, { type: 'nack' }, { type: 'nack', parameter: 'pli' }, { type: 'goog-remb' }],
          }),
          new RTCRtpCodecParameters({
            mimeType: 'video/H264', clockRate: 90000,
            rtcpFeedback: [{ type: 'transport-cc' }, { type: 'nack' }, { type: 'nack', parameter: 'pli' }, { type: 'goog-remb' }],
            parameters: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f',
          }),
        ],
      },
      iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
    })
    this.#pc = pc

    pc.createDataChannel('dc', { id: 1 })
    pc.addTransceiver('audio', { direction: 'sendrecv' })
    pc.addTransceiver('video', { direction: 'sendrecv' })

    // Track RTP readiness — resolve immediately on first packet, not after timeout
    let resolveVideo, resolveAudio
    const videoReady = new Promise(r => { resolveVideo = r; setTimeout(r, 8000) })
    const audioReady = new Promise(r => { resolveAudio = r; setTimeout(r, 3000) }) // short audio timeout
    let hasAudio = false
    let videoCodec = 'H264'
    let videoPayloadType = 97

    // Handle incoming media tracks
    pc.addEventListener('track', (event) => {
      if (event.track.kind === 'video') {
        const codec = event.track.codec?.mimeType || ''
        videoCodec = codec.includes('H264') ? 'H264' : codec.includes('VP9') ? 'VP9' : 'H264'
        log(`video track: ${codec}`)
      }

      let keyframeOk = false
      const kfTimeout = setTimeout(() => { keyframeOk = true }, 3000)

      event.track.onReceiveRtp.subscribe((rtp) => {
        if (event.track.kind === 'video') {
          // Skip empty/tiny packets that cause "Empty H.264 RTP packet" errors
          if (!rtp.payload || rtp.payload.length < 2) return

          if (!keyframeOk) {
            const p = rtp.payload
            const nal = p[0] & 0x1f
            if (nal === 7 || nal === 5 || nal === 24 || (nal === 28 && ((p[1] & 0x1f) === 7 || (p[1] & 0x1f) === 5))) {
              keyframeOk = true
              clearTimeout(kfTimeout)
            }
            if (!keyframeOk) return
          }
          udp.send(rtp.serialize(), videoPort, '0.0.0.0', () => {})
          resolveVideo()
        } else if (event.track.kind === 'audio') {
          hasAudio = true
          udp.send(rtp.serialize(), audioPort, '0.0.0.0', () => {})
          resolveAudio()
        }
      })
    })

    pc.addEventListener('connectionstatechange', () => {
      log(`webrtc: ${pc.connectionState}`)
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        if (this.#running) this.#retry()
      }
    })

    // Create offer and gather ICE candidates
    const offer = await pc.createOffer()
    await pc.setLocalDescription(offer)

    if (pc.iceGatheringState !== 'complete') {
      await new Promise(r => {
        pc.addEventListener('icegatheringstatechange', () => { if (pc.iceGatheringState === 'complete') r() })
        setTimeout(r, 5000)
      })
    }

    // Negotiate with Foyer API
    log(`connecting to ${this.#deviceId}`)
    const answer = await this.#client.joinStream(this.#deviceId, pc.localDescription.sdp, this.#resolution)
    if (!answer.sdp) throw new Error('No answer SDP from Foyer API')

    // Extract payload type from answer
    const ptMatch = answer.sdp.match(/a=rtpmap:(\d+)\s+(?:H264|VP9)\/90000/)
    if (ptMatch) videoPayloadType = parseInt(ptMatch[1])

    await pc.setRemoteDescription({ type: 'answer', sdp: answer.sdp })
    log(`negotiated: ${videoCodec} pt=${videoPayloadType}`)

    // Wait for media
    await Promise.all([videoReady, audioReady])

    // Send PLI for keyframe
    try {
      const vr = pc.getReceivers().find(r => r.track?.kind === 'video')
      if (vr?.sendRtcpPLI) vr.sendRtcpPLI()
    } catch {}

    // Write SDP for ffmpeg
    const codecLine = videoCodec === 'H264'
      ? `a=rtpmap:${videoPayloadType} H264/90000\na=fmtp:${videoPayloadType} packetization-mode=1;profile-level-id=42e01f`
      : `a=rtpmap:${videoPayloadType} VP9/90000`

    const audioSdp = hasAudio ? `\nm=audio ${audioPort} RTP/AVP 96\na=rtpmap:96 OPUS/48000/2\na=recvonly\na=rtcp:${audioRtcpPort}` : ''

    this.#sdpFile = `/tmp/nest-rtsp-${this.#name}.sdp`
    await writeFile(this.#sdpFile, `v=0\no=- 0 0 IN IP4 127.0.0.1\ns=NestRTSP\nc=IN IP4 127.0.0.1\nt=0 0\n\nm=video ${videoPort} RTP/AVP ${videoPayloadType}\n${codecLine}\na=recvonly\na=rtcp:${videoRtcpPort}${audioSdp}\n`)

    // Launch ffmpeg — H.264 passthrough (no transcode!)
    const dest = `rtsp://127.0.0.1:${this.#rtspPort}/${this.#name}`
    const args = [
      '-y', '-hide_banner', '-loglevel', 'warning',
      '-protocol_whitelist', 'file,crypto,data,udp,rtp',
      '-fflags', '+discardcorrupt+genpts',
      '-flags', 'low_delay',
      // Use RTP timestamps (90kHz clock) — NOT wallclock which has arrival jitter
      '-analyzeduration', '2000000',
      '-probesize', '2000000',
      '-reorder_queue_size', '100',
      '-max_delay', '500000',
      '-err_detect', 'ignore_err',
      '-i', this.#sdpFile,
      '-c:v', 'copy',
      '-fps_mode', 'passthrough',
      ...(hasAudio ? ['-c:a:0', 'aac', '-b:a:0', '128k', '-af', 'aresample=async=1:first_pts=0'] : []),
      '-map', '0:v', ...(hasAudio ? ['-map', '0:a'] : []),
      '-rtsp_transport', 'tcp',
      '-f', 'rtsp', dest,
    ]

    log(`ffmpeg ${videoCodec} passthrough → ${dest}`)
    const ff = spawn('ffmpeg', args, { stdio: ['pipe', 'pipe', 'pipe'] })
    this.#ffmpeg = ff
    ff.stdin?.on('error', () => {})
    ff.stderr?.on('data', (d) => {
      const msg = d.toString().trim()
      if (msg && !msg.includes('frames duplicated') && !msg.includes('deprecated pixel format'))
        log(`ffmpeg: ${msg}`)
    })
    ff.on('exit', (code) => {
      if (this.#ffmpeg === ff && this.#running && code !== 0) {
        log(`ffmpeg exited ${code}`)
        this.#retry()
      }
    })

    this.#failures = 0
    log('streaming')
    this.emit('streaming')
  }
}

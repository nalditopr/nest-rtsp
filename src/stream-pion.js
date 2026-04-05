/**
 * Pion-backed camera stream — spawns the Go pion-bridge binary for WebRTC,
 * forwards H.264 RTP via UDP to ffmpeg for RTSP output.
 *
 * Gets 30fps/1.2Mbps from Foyer (vs 10fps with werift) thanks to
 * proper transport-wide-cc bandwidth estimation.
 */

import { spawn } from 'node:child_process'
import { writeFile, unlink } from 'node:fs/promises'
import { EventEmitter } from 'node:events'
import { pickPort } from './ports.js'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = dirname(fileURLToPath(import.meta.url))
const PION_BINARY = join(__dirname, '..', 'pion-bridge', 'pion-bridge')

export class PionStream extends EventEmitter {
  #name
  #deviceId
  #resolution
  #cookiesPath
  #rtspPort
  #log

  #pion = null
  #ffmpeg = null
  #sdpFile = null
  #running = false
  #failures = 0
  #retryTimer = null

  constructor({ name, deviceId, resolution = 3, cookiesPath, rtspPort, log }) {
    super()
    this.#name = name
    this.#deviceId = deviceId
    this.#resolution = resolution
    this.#cookiesPath = cookiesPath
    this.#rtspPort = rtspPort
    this.#log = log || console.log
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
    this.#pion?.kill('SIGTERM')
    this.#pion = null
    this.#ffmpeg?.kill('SIGTERM')
    this.#ffmpeg = null
    if (this.#sdpFile) unlink(this.#sdpFile).catch(() => {})
  }

  #retry() {
    if (!this.#running) return
    this.#cleanup()
    this.#failures++
    const delay = Math.min(2000 * Math.pow(2, this.#failures - 1), 300000)
    this.#log(`retry in ${delay / 1000}s (#${this.#failures})`)
    this.#retryTimer = setTimeout(() => this.#connect(), delay)
  }

  async #connect() {
    try {
      await this.#doConnect()
    } catch (err) {
      this.#log(`error: ${err.message}`)
      this.#retry()
    }
  }

  async #doConnect() {
    const log = this.#log

    // Pick UDP ports
    const videoPort = await pickPort()
    const videoRtcpPort = await pickPort()
    const audioPort = await pickPort()
    const audioRtcpPort = await pickPort()

    // Spawn pion-bridge Go binary
    log(`connecting to ${this.#deviceId} via Pion`)
    const pion = spawn(PION_BINARY, [
      '-cookies', this.#cookiesPath,
      '-device', this.#deviceId,
      '-port', String(videoPort),
      '-audio-port', String(audioPort),
      '-resolution', String(this.#resolution),
    ], { stdio: ['pipe', 'pipe', 'pipe'] })
    this.#pion = pion

    // Parse pion-bridge output
    let videoCodec = 'H264'
    let videoPayloadType = 96
    let hasAudio = false
    let connected = false

    const pionReady = new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('Pion connection timeout')), 20000)

      pion.stderr.on('data', (d) => {
        const lines = d.toString().trim().split('\n')
        for (const line of lines) {
          if (line.startsWith('STATE connected')) {
            connected = true
          } else if (line.startsWith('TRACK video')) {
            const match = line.match(/codec=(\S+)/)
            if (match) videoCodec = match[1].includes('VP9') ? 'VP9' : 'H264'
            log(`video track: ${videoCodec}`)
          } else if (line.startsWith('TRACK audio')) {
            hasAudio = true
          } else if (line.startsWith('ANSWER')) {
            log(line)
          } else if (line.startsWith('CONNECTED')) {
            // Pion is connected and tracks will follow
          } else if (line.startsWith('ERROR')) {
            log(line)
          }
        }
      })

      // Video info comes on stdout as JSON
      pion.stdout.on('data', (d) => {
        for (const line of d.toString().trim().split('\n')) {
          try {
            const info = JSON.parse(line)
            if (info.type === 'video_info') {
              videoPayloadType = info.payloadType
              videoCodec = info.codec?.includes('VP9') ? 'VP9' : 'H264'
              log(`negotiated: ${videoCodec} pt=${videoPayloadType}`)
              clearTimeout(timeout)
              resolve()
            } else if (info.type === 'audio_info') {
              hasAudio = true
            }
          } catch {}
        }
      })

      pion.on('exit', (code) => {
        if (this.#pion === pion) {
          clearTimeout(timeout)
          if (this.#running) {
            log(`pion-bridge exited ${code}`)
            reject(new Error(`pion-bridge exited ${code}`))
          }
        }
      })
    })

    await pionReady

    // Wait a moment for RTP to start flowing
    await new Promise(r => setTimeout(r, 1000))

    // Write SDP for ffmpeg
    const codecSdp = videoCodec === 'VP9'
      ? `a=rtpmap:${videoPayloadType} VP9/90000`
      : `a=rtpmap:${videoPayloadType} H264/90000\na=fmtp:${videoPayloadType} packetization-mode=1;profile-level-id=42e01f`

    const audioSdp = hasAudio ? `\nm=audio ${audioPort} RTP/AVP 111\na=rtpmap:111 opus/48000/2\na=recvonly\na=rtcp:${audioRtcpPort}` : ''

    this.#sdpFile = `/tmp/nest-rtsp-${this.#name}.sdp`
    await writeFile(this.#sdpFile, `v=0\no=- 0 0 IN IP4 127.0.0.1\ns=NestRTSP\nc=IN IP4 127.0.0.1\nt=0 0\n\nm=video ${videoPort} RTP/AVP ${videoPayloadType}\n${codecSdp}\na=recvonly\na=rtcp:${videoRtcpPort}${audioSdp}\n`)

    // Launch ffmpeg — H.264 passthrough
    const dest = `rtsp://127.0.0.1:${this.#rtspPort}/${this.#name}`
    const args = [
      '-y', '-hide_banner', '-loglevel', 'warning',
      '-protocol_whitelist', 'file,crypto,data,udp,rtp',
      '-fflags', '+discardcorrupt+genpts',
      '-flags', 'low_delay',
      '-analyzeduration', '2000000',
      '-probesize', '2000000',
      '-reorder_queue_size', '100',
      '-max_delay', '500000',
      '-err_detect', 'ignore_err',
      '-i', this.#sdpFile,
      '-c:v', 'copy',
      ...(hasAudio ? ['-c:a:0', 'aac', '-b:a:0', '128k', '-af', 'aresample=async=1:first_pts=0'] : []),
      '-map', '0:v', ...(hasAudio ? ['-map', '0:a'] : []),
      '-rtsp_transport', 'tcp',
      '-f', 'rtsp', dest,
    ]

    log(`ffmpeg [${videoCodec} passthrough] → ${dest}`)
    const ff = spawn('ffmpeg', args, { stdio: ['pipe', 'pipe', 'pipe'] })
    this.#ffmpeg = ff
    ff.stdin?.on('error', () => {})
    ff.stderr?.on('data', (d) => {
      const msg = d.toString().trim()
      if (msg && !msg.includes('frames duplicated') && !msg.includes('deprecated'))
        log(`ffmpeg: ${msg}`)
    })
    ff.on('exit', (code) => {
      if (this.#ffmpeg === ff && this.#running && code !== 0) {
        log(`ffmpeg exited ${code}`)
        this.#retry()
      }
    })

    // Monitor pion-bridge
    pion.on('exit', (code) => {
      if (this.#pion === pion && this.#running) {
        log(`pion-bridge exited ${code}, retrying`)
        this.#retry()
      }
    })

    this.#failures = 0
    log('streaming (30fps Pion)')
    this.emit('streaming')
  }
}

/**
 * Chrome-backed camera stream — uses headless Chrome for WebRTC
 * to get full 16fps/1.3Mbps from Foyer, then pipes H.264 via
 * WebCodecs VideoEncoder → WebSocket → ffmpeg → RTSP.
 *
 * Falls back to JPEG capture if WebCodecs isn't available.
 */

import { createServer } from 'node:http'
import { WebSocketServer } from 'ws'
import { spawn } from 'node:child_process'
import { EventEmitter } from 'node:events'

const BROWSER_SCRIPT = (deviceId, wsPort, apiKey, fps) => `(async () => {
  const sapisid = document.cookie.match(/SAPISID=([^;]+)/)?.[1];
  if (!sapisid) return { error: 'Not logged in' };

  const ts = Math.floor(Date.now() / 1000);
  const hashBuf = await crypto.subtle.digest('SHA-1',
    new TextEncoder().encode(ts + ' ' + sapisid + ' ' + location.origin));
  const hash = Array.from(new Uint8Array(hashBuf)).map(b => b.toString(16).padStart(2, '0')).join('');
  const authHash = ts + '_' + hash;

  const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
  pc.addTransceiver('audio', { direction: 'sendrecv' });
  pc.addTransceiver('video', { direction: 'sendrecv' });
  pc.createDataChannel('dc', { id: 1 });
  await pc.setLocalDescription(await pc.createOffer());
  await new Promise(r => {
    if (pc.iceGatheringState === 'complete') return r();
    pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') r(); };
    setTimeout(r, 5000);
  });

  const resp = await fetch('https://googlehomefoyer-pa.clients6.google.com/v1/join_stream', {
    method: 'POST', credentials: 'include',
    headers: {
      'Authorization': 'SAPISIDHASH ' + authHash + ' SAPISID1PHASH ' + authHash + ' SAPISID3PHASH ' + authHash,
      'Content-Type': 'application/json',
      'x-goog-api-key': '${apiKey}',
      'x-goog-authuser': '0',
      'x-foyer-client-environment': 'CAc=',
    },
    body: JSON.stringify({ action: 'offer', deviceId: '${deviceId}', sdp: pc.localDescription.sdp, requestedVideoResolution: 3 }),
  });
  if (!resp.ok) return { error: 'Foyer ' + resp.status };
  const answer = await resp.json();
  if (!answer.sdp) return { error: 'No SDP' };

  await pc.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
  await new Promise(r => {
    if (pc.connectionState === 'connected') return r();
    pc.onconnectionstatechange = () => { if (pc.connectionState === 'connected') r(); };
    setTimeout(r, 15000);
  });

  // Wait for stable resolution
  let maxW = 0, maxH = 0, stable = 0;
  for (let i = 0; i < 30; i++) {
    await new Promise(r => setTimeout(r, 200));
    (await pc.getStats()).forEach(s => {
      if (s.type === 'inbound-rtp' && s.kind === 'video' && s.frameWidth) {
        if (s.frameWidth > maxW) { maxW = s.frameWidth; maxH = s.frameHeight; stable = 0; }
        else if (s.frameWidth === maxW) stable++;
      }
    });
    if (stable >= 3) break;
  }
  if (!maxW) return { error: 'No video' };

  const videoTrack = pc.getReceivers().find(r => r.track?.kind === 'video')?.track;
  if (!videoTrack) return { error: 'No video track' };

  const ws = new WebSocket('ws://127.0.0.1:${wsPort}');
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; setTimeout(j, 5000); });

  // Try WebCodecs H.264 encoder
  let encoderMode = 'jpeg';
  let selectedAccel = 'no-preference';
  if (typeof VideoEncoder !== 'undefined') {
    for (const accel of ['prefer-hardware', 'no-preference']) {
      try {
        const s = await VideoEncoder.isConfigSupported({
          codec: 'avc1.4D0028', width: maxW, height: maxH,
          bitrate: 4000000, framerate: ${fps},
          hardwareAcceleration: accel, avc: { format: 'annexb' },
        });
        if (s.supported) { encoderMode = 'webcodecs'; selectedAccel = accel; break; }
      } catch {}
    }
  }

  ws.send(JSON.stringify({ type: 'init', width: maxW, height: maxH, encoderMode }));

  if (encoderMode === 'webcodecs') {
    const encoder = new VideoEncoder({
      output: (chunk) => {
        if (ws.readyState !== 1) return;
        const buf = new ArrayBuffer(chunk.byteLength);
        chunk.copyTo(buf);
        ws.send(buf);
      },
      error: () => {},
    });
    encoder.configure({
      codec: 'avc1.4D0028', width: maxW, height: maxH,
      bitrate: 4000000, framerate: ${fps},
      hardwareAcceleration: selectedAccel, avc: { format: 'annexb' },
    });

    const video = document.createElement('video');
    video.srcObject = new MediaStream([videoTrack]);
    video.autoplay = true; video.muted = true;
    document.body.appendChild(video);

    let frameNum = 0;
    const keyInterval = ${fps} * 2;
    const capture = () => {
      if (ws.readyState !== 1 || !video.videoWidth) { video.requestVideoFrameCallback(capture); return; }
      const frame = new VideoFrame(video, { timestamp: frameNum * (1000000 / ${fps}) });
      encoder.encode(frame, { keyFrame: frameNum % keyInterval === 0 });
      frame.close();
      frameNum++;
      video.requestVideoFrameCallback(capture);
    };
    video.requestVideoFrameCallback(capture);
    window.__cleanup = () => { encoder.close(); ws.close(); pc.close(); };
  } else {
    const video = document.createElement('video');
    video.srcObject = new MediaStream([videoTrack]);
    video.autoplay = true; video.muted = true;
    document.body.appendChild(video);
    let canvas = new OffscreenCanvas(maxW, maxH);
    let ctx = canvas.getContext('2d');
    setInterval(async () => {
      if (ws.readyState !== 1 || !video.videoWidth) return;
      ctx.drawImage(video, 0, 0, maxW, maxH);
      const blob = await canvas.convertToBlob({ type: 'image/jpeg', quality: 0.7 });
      ws.send(await blob.arrayBuffer());
    }, ${Math.round(1000 / fps)});
    window.__cleanup = () => { ws.close(); pc.close(); };
  }

  return { width: maxW, height: maxH, encoderMode };
})()`

export class ChromeStream extends EventEmitter {
  #name
  #deviceId
  #context  // shared browser context
  #rtspPort
  #log
  #page = null
  #wsServer = null
  #httpServer = null
  #ffmpeg = null
  #running = false
  #failures = 0
  #retryTimer = null

  constructor({ name, deviceId, context, rtspPort, log }) {
    super()
    this.#name = name
    this.#deviceId = deviceId
    this.#context = context
    this.#rtspPort = rtspPort
    this.#log = log || console.log
  }

  get name() { return this.#name }

  async start() {
    this.#running = true
    this.#failures = 0
    await this.#connect()
  }

  async stop() {
    this.#running = false
    clearTimeout(this.#retryTimer)
    this.#cleanup()
  }

  #cleanup() {
    this.#ffmpeg?.kill('SIGTERM')
    this.#ffmpeg = null
    this.#wsServer?.close()
    this.#wsServer = null
    this.#httpServer?.close()
    this.#httpServer = null
    try { this.#page?.evaluate('window.__cleanup?.()').catch(() => {}) } catch {}
    try { this.#page?.close().catch(() => {}) } catch {}
    this.#page = null
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

    // WebSocket server for this camera
    const httpServer = createServer()
    const wsServer = new WebSocketServer({ server: httpServer })
    this.#httpServer = httpServer
    this.#wsServer = wsServer

    let encoderMode = 'jpeg'
    let ffmpegStarted = false

    wsServer.on('connection', (ws) => {
      log('frame pipeline connected')
      ws.binaryType = 'nodebuffer'
      ws.on('message', (data, isBinary) => {
        // JSON init message
        if (data[0] === 0x7b) {
          try {
            const msg = JSON.parse(data.toString())
            if (msg.type === 'init') {
              encoderMode = msg.encoderMode || 'jpeg'
              log(`${msg.width}x${msg.height} [${encoderMode}]`)
              this.#startFfmpeg(encoderMode)
              ffmpegStarted = true
            }
            return
          } catch {}
        }
        // Binary frame data → ffmpeg stdin
        if (ffmpegStarted && this.#ffmpeg) {
          const buf = Buffer.isBuffer(data) ? data : Buffer.from(data)
          if (buf.length > 0) {
            try {
              const stdin = this.#ffmpeg.stdin
              if (stdin && !stdin.destroyed && stdin.writable) {
                if (!this._byteCount) { this._byteCount = 0; this._logTimer = setInterval(() => { log(`${(this._byteCount/1024).toFixed(0)} KB piped`); }, 5000) }
                this._byteCount += buf.length
                stdin.write(buf, () => {})
              }
            } catch {}
          }
        }
      })
    })

    await new Promise(r => httpServer.listen(0, '127.0.0.1', r))
    const wsPort = httpServer.address().port

    // Open tab
    const page = await this.#context.newPage()
    this.#page = page
    await page.goto('https://home.google.com', { waitUntil: 'networkidle', timeout: 15000 }).catch(() => {})

    log(`connecting to ${this.#deviceId}`)
    const script = BROWSER_SCRIPT(this.#deviceId, wsPort, 'AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM', 24)
    const result = await page.evaluate(script)

    if (result.error) throw new Error(result.error)
    log(`streaming ${result.width}x${result.height} [${result.encoderMode}]`)
    this.#failures = 0
    this.emit('streaming')
  }

  #startFfmpeg(mode) {
    if (this.#ffmpeg) { this.#ffmpeg.kill('SIGTERM'); this.#ffmpeg = null }

    const dest = `rtsp://127.0.0.1:${this.#rtspPort}/${this.#name}`
    const inputFormat = mode === 'webcodecs' ? 'h264' : 'mjpeg'
    const videoCodec = mode === 'webcodecs' ? ['-c:v', 'copy'] : ['-c:v', 'h264_nvenc', '-preset', 'p4', '-profile:v', 'main', '-rc', 'cbr', '-b:v', '4000k']

    const args = [
      '-y', '-hide_banner', '-loglevel', 'warning',
      '-f', inputFormat, '-i', 'pipe:0',
      ...videoCodec,
      '-f', 'rtsp', '-rtsp_transport', 'tcp', dest,
    ]

    this.#log(`ffmpeg [${mode}] → ${dest}`)
    const ff = spawn('ffmpeg', args, { stdio: ['pipe', 'pipe', 'pipe'] })
    this.#ffmpeg = ff
    ff.stdin?.on('error', () => {})
    ff.stderr?.on('data', (d) => {
      const msg = d.toString().trim()
      if (msg && !msg.includes('frames duplicated') && !msg.includes('deprecated'))
        this.#log(`ffmpeg: ${msg}`)
    })
    ff.on('exit', (code) => {
      if (this.#ffmpeg === ff && this.#running && code !== 0) {
        this.#log(`ffmpeg exited ${code}`)
        this.#retry()
      }
    })
  }
}

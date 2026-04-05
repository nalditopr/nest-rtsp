# nest-rtsp

Direct 1080p 30fps RTSP streams from Google Nest cameras. Single 11MB binary.

No Chrome. No ffmpeg. No transcoding. No GPU needed.

## How it works

```
Google Foyer API
  ↓ WebRTC H.264 (30fps, 1.2Mbps)
nest-rtsp (Pion + gortsplib)
  ↓ built-in RTSP server
Your NVR (Frigate, etc.)
```

One process handles all cameras. WebRTC packets are forwarded directly to RTSP clients — zero decode, zero encode, zero copy beyond what the kernel requires.

## Quick start

```bash
# 1. Copy and edit config
cp config.example.yaml data/config.yaml

# 2. Add your cookies (see Cookie Setup below)

# 3. Run
docker compose up -d
# or: ./nest-rtsp -config data/config.yaml

# Streams at rtsp://localhost:8554/{camera_name}
```

## Resource usage

Per camera: **~1.5% CPU, 5MB RAM.** 8 cameras total: **11% CPU, 41MB RAM.**

No GPU required. Runs on a Raspberry Pi.

## Cookie setup

nest-rtsp authenticates with Google using browser session cookies. You need to extract them once, and refresh every few weeks when they expire.

### Extract cookies

1. Open Chrome and go to [home.google.com](https://home.google.com)
2. Log in if needed
3. Open DevTools (F12) → Application → Cookies → `https://home.google.com`
4. Create `data/cookies.json` with these cookie values:

```json
{
  "SID": "...",
  "HSID": "...",
  "SSID": "...",
  "APISID": "...",
  "SAPISID": "...",
  "__Secure-1PSID": "...",
  "__Secure-3PSID": "...",
  "__Secure-1PAPISID": "...",
  "__Secure-3PAPISID": "...",
  "NID": "...",
  "SIDCC": "...",
  "__Secure-1PSIDTS": "...",
  "__Secure-3PSIDTS": "...",
  "__Secure-1PSIDCC": "...",
  "__Secure-3PSIDCC": "..."
}
```

### Automated extraction (optional)

If you have Playwright installed:

```bash
npx playwright open --user-data-dir=data/chrome-profile https://home.google.com
# Log in, then Ctrl+C. Extract cookies with:
node -e "
const { chromium } = require('playwright');
(async () => {
  const ctx = await chromium.launchPersistentContext('data/chrome-profile', {headless:true});
  const page = await ctx.newPage();
  await page.goto('https://home.google.com');
  const cookies = await ctx.cookies('https://home.google.com');
  const kv = {};
  cookies.filter(c => c.domain === '.google.com').forEach(c => kv[c.name] = c.value);
  require('fs').writeFileSync('data/cookies.json', JSON.stringify(kv, null, 2));
  console.log('Saved', Object.keys(kv).length, 'cookies');
  await ctx.close();
})();
"
```

## Finding device IDs

1. Go to [home.google.com](https://home.google.com) in Chrome
2. Open DevTools → Network tab
3. Click on a camera to start its live view
4. Look for a request to `join_stream`
5. The `deviceId` field in the request body is what you need (e.g., `DEVICE_C74582B7127A396C`)

## Configuration

`data/config.yaml`:

```yaml
cookies_file: /data/cookies.json
rtsp_port: 8554

cameras:
  front_door:
    device_id: DEVICE_XXXXXXXXXXXXXXXX
    resolution: 3    # 0=low, 1=SD, 2=HD, 3=Full (1080p)
  backyard:
    device_id: DEVICE_YYYYYYYYYYYYYYYY
    resolution: 3
```

## Frigate integration

```yaml
cameras:
  front_door:
    ffmpeg:
      inputs:
        - path: rtsp://localhost:8554/front_door
          roles: [detect, record]
    detect:
      width: 1920
      height: 1080
```

## Logs

nest-rtsp logs codec, fps, bitrate, and frame count every 10 seconds:

```
[front_door] video/H264 — 30.0fps 1.14Mbps (600 frames, 2740 pkts)
[backyard]   video/H264 — 30.1fps 1.07Mbps (601 frames, 2511 pkts)
[garage]     video/H264 — 14.7fps 0.53Mbps (295 frames, 1210 pkts)
```

## Building

```bash
cd nest-rtsp-go
go build -ldflags="-s -w" -o nest-rtsp .
```

Or with Docker:

```bash
docker build -t nest-rtsp .
```

## Architecture

Single Go binary using:
- [Pion WebRTC](https://github.com/pion/webrtc) — WebRTC with Chrome-like SDP for full 30fps from Google
- [gortsplib](https://github.com/bluenviron/gortsplib) — Built-in RTSP server, no external dependencies
- SAPISIDHASH authentication — same auth as the Google Home web app

The key to getting 30fps (vs 10fps with naive WebRTC): registering all 11 Chrome header extensions including `transport-wide-cc` for bandwidth estimation. Without these, Google's server throttles to minimum quality.

## License

MIT

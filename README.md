# nest-rtsp

Direct RTSP streams from Google Nest cameras via the Foyer API.

**30fps 1080p H.264** with zero transcoding — just WebRTC to RTSP passthrough.

## How it works

```
Google Foyer API (SAPISIDHASH auth)
     ↓ WebRTC (H.264, 30fps, 1.2Mbps)
  Pion (Go WebRTC library)
     ↓ RTP/UDP
  ffmpeg -c:v copy (passthrough)
     ↓ RTSP/TCP
  MediaMTX
     ↓ RTSP
  Your NVR (Frigate, etc.)
```

No Chrome needed at runtime. No GPU needed. No transcoding.

## Quick start

### 1. Configure cameras

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` with your camera device IDs (see [Finding device IDs](#finding-device-ids)).

### 2. Set up cookies

You need Google session cookies from a logged-in browser. See [Cookie setup](#cookie-setup).

### 3. Run with Docker

```bash
docker compose up -d
```

Streams available at `rtsp://localhost:8554/{camera_name}`.

### 4. Connect your NVR

Example Frigate config:

```yaml
cameras:
  front_door:
    ffmpeg:
      inputs:
        - path: rtsp://nest-rtsp:8554/front_door
          input_args: preset-rtsp-restream
          roles: [detect, record]
    detect:
      width: 1920
      height: 1080
```

## Cookie setup

nest-rtsp needs Google session cookies to authenticate with the Foyer API. Cookies expire every few weeks and need refreshing.

### Option A: Bookmarklet (easiest)

1. Start nest-rtsp: `docker compose up -d`
2. Open `http://localhost:3000` in your browser
3. Drag the **"Send Cookies"** bookmarklet to your bookmarks bar
4. Go to [home.google.com](https://home.google.com) and log in
5. Click the bookmarklet — you should see "ok (valid)"

**Limitation:** The bookmarklet can only access non-httpOnly cookies. It works for auth but some cookies are missed.

### Option B: Persistent Chrome profile (recommended)

This method stores a full Chrome profile so cookies auto-refresh:

```bash
# First time — log in interactively
docker exec -it nest-rtsp npx playwright open \
  --user-data-dir=/data/chrome-profile \
  https://home.google.com

# Log in with your Google account, then close the browser.
# nest-rtsp will auto-extract cookies every 6 hours.
```

### Option C: Manual cookie file

Extract cookies from your browser and save as `data/cookies.json`:

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
  "SIDCC": "..."
}
```

To extract from Chrome:
1. Go to [home.google.com](https://home.google.com)
2. Open DevTools → Application → Cookies → `.google.com`
3. Copy each cookie name/value pair

## Finding device IDs

Device IDs look like `DEVICE_C74582B7127A396C`. To find yours:

1. Go to [home.google.com](https://home.google.com) in Chrome
2. Open DevTools → Network tab
3. Click on a camera to start its live view
4. Look for a request to `join_stream`
5. In the request body, find the `deviceId` field

Alternatively, check the Foyer API response at `http://localhost:3000/health` after setting up cookies.

## Configuration

### config.yaml

```yaml
cookies_file: /data/cookies.json
chrome_profile: /data/chrome-profile
cookie_refresh_hours: 6
rtsp_port: 8554
cookie_server_port: 3000
stream_mode: pion          # 'pion' (30fps) or 'direct' (10fps fallback)
api_key: AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM

cameras:
  front_door:
    device_id: DEVICE_XXXXXXXXXXXXXXXX
    resolution: 3          # 0=low, 1=SD, 2=HD, 3=Full (1080p)
  backyard:
    device_id: DEVICE_YYYYYYYYYYYYYYYY
    resolution: 3
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `STREAM_MODE` | `pion` | `pion` (30fps) or `direct` (10fps) |
| `COOKIES_FILE` | `/data/cookies.json` | Path to cookies |
| `CHROME_PROFILE` | `/data/chrome-profile` | Chrome profile dir |
| `COOKIE_REFRESH_HOURS` | `6` | Auto-refresh interval |
| `RTSP_PORT` | `8554` | RTSP output port |
| `COOKIE_SERVER_PORT` | `3000` | Cookie UI port |
| `CAMERAS` | (from config) | Inline: `name:DEVICE_ID:res,...` |

## Stream modes

### Pion (default, recommended)

Uses a Go binary ([Pion WebRTC](https://github.com/pion/webrtc)) that generates Chrome-like SDP offers with proper bandwidth estimation (transport-wide-cc). Google's server sends full quality: **30fps, 1.2Mbps, 1080p H.264**.

### Direct (fallback)

Uses [werift](https://github.com/nicklason/werift) (Node.js WebRTC). Simpler but Google throttles to **10fps, 0.06Mbps** because werift's SDP doesn't include the header extensions needed for bandwidth estimation.

## Running without Docker

```bash
# Install dependencies
npm install
cd pion-bridge && go build -o pion-bridge . && cd ..

# Download MediaMTX
wget https://github.com/bluenviron/mediamtx/releases/download/v1.9.3/mediamtx_v1.9.3_linux_amd64.tar.gz
tar xzf mediamtx_v1.9.3_linux_amd64.tar.gz

# Set up cookies (see above)
mkdir -p data
# ... copy cookies.json to data/

# Run MediaMTX
./mediamtx mediamtx.yml &

# Run nest-rtsp
cp config.example.yaml config.yaml
# ... edit config.yaml ...
node src/index.js
```

## Architecture

```
┌─────────────┐
│ config.yaml │ Camera device IDs + settings
└──────┬──────┘
       │
┌──────▼──────┐     ┌──────────────┐
│  nest-rtsp  │────▶│ pion-bridge  │ (one per camera)
│  (Node.js)  │     │    (Go)      │
└──────┬──────┘     └──────┬───────┘
       │                   │ WebRTC (H.264, 30fps)
       │                   ▼
       │            Google Foyer API
       │                   │
       │            ┌──────▼───────┐
       │            │  UDP RTP     │
       │            └──────┬───────┘
       │                   │
┌──────▼──────┐     ┌──────▼───────┐
│   ffmpeg    │◀────│ SDP file     │
│  -c:v copy  │     └──────────────┘
└──────┬──────┘
       │ RTSP/TCP
┌──────▼──────┐
│  MediaMTX   │ :8554
└──────┬──────┘
       │ RTSP
┌──────▼──────┐
│  Frigate /  │
│  Your NVR   │
└─────────────┘
```

## Resource usage

Measured with 8 Nest cameras at 1080p/30fps:

| Component | Per camera | 8 cameras |
|-----------|-----------|-----------|
| pion-bridge (Go) | 1.5% CPU, 18MB RAM | 12% CPU, 140MB RAM |
| ffmpeg (passthrough) | 1.5% CPU, 53MB RAM | 12% CPU, 424MB RAM |
| Node.js orchestrator | — | 0.1% CPU, 91MB RAM |
| MediaMTX | — | 0.9% CPU, 30MB RAM |
| **Total** | **~3% CPU, 71MB** | **~24% CPU, 685MB RAM** |

**No GPU required.** The H.264 stream is passed through without decoding or encoding. ffmpeg only muxes RTP to RTSP.

**Minimum hardware:** Any x86_64 Linux system with 1GB RAM and a network connection. Runs on a Raspberry Pi 4 (ARM64) if you cross-compile the Go binary. A $5/month VPS can handle 8 cameras.

**Network:** Each camera uses ~1.2 Mbps downstream from Google + ~1.2 Mbps to your NVR on the local network.

## License

MIT

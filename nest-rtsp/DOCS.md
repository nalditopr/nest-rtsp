# Nest RTSP - Home Assistant Add-on

Direct 1080p 30fps RTSP streams from Google Nest cameras.

## Setup

### 1. Add cameras

In the add-on configuration, add your cameras:

```yaml
cameras:
  - name: front_door
    device_id: DEVICE_XXXXXXXXXXXXXXXX
    resolution: 3
  - name: backyard
    device_id: DEVICE_YYYYYYYYYYYYYYYY
    resolution: 3
```

### 2. Add cookies

Paste your Google session cookies as JSON in the `cookies_json` field:

```json
{"SID":"...","HSID":"...","SAPISID":"...","__Secure-1PSID":"...","__Secure-3PSID":"..."}
```

See the [cookie setup guide](https://github.com/nalditopr/nest-rtsp#cookie-setup) for how to extract cookies from your browser.

### 3. Configure Frigate

Point Frigate at the RTSP streams:

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

## Finding device IDs

1. Go to [home.google.com](https://home.google.com)
2. Open DevTools → Network
3. Click a camera
4. Find `join_stream` request → `deviceId` field

## Refreshing cookies

Google cookies expire every few weeks. When streams stop, update the `cookies_json` field in the add-on configuration with fresh cookies.

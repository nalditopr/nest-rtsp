#!/usr/bin/env node
/**
 * nest-rtsp — Direct RTSP streams from Nest cameras via Google Home Foyer API.
 *
 * No Chrome. No cloud relay. Just WebRTC → RTSP.
 *
 * Usage:
 *   node src/index.js                          # uses ./config.yaml
 *   node src/index.js /path/to/config.yaml     # custom config
 *   CAMERAS='cam1:DEVICE_XXX,cam2:DEVICE_YYY' node src/index.js  # env-only
 */

import { readFileSync, existsSync } from 'node:fs'
import yaml from 'js-yaml'
import { FoyerClient, normalizeCookies } from './foyer.js'
import { CameraStream } from './stream.js'
import { ChromeStream } from './stream-chrome.js'
import { PionStream } from './stream-pion.js'
import { startCookieServer } from './cookie-server.js'
import { startCookieRefreshTimer, checkCookies } from './cookie-refresh.js'

// Load config
const configPath = process.argv[2] || './config.yaml'
let config = {}
if (existsSync(configPath)) {
  config = yaml.load(readFileSync(configPath, 'utf-8')) || {}
  console.log(`[config] Loaded ${configPath}`)
}

const cookiesPath = config.cookies_file || process.env.COOKIES_FILE || '/data/cookies.json'
const rtspPort = config.rtsp_port || parseInt(process.env.RTSP_PORT || '8554')
const apiKey = config.api_key || process.env.API_KEY || 'AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM'
const cookieServerPort = config.cookie_server_port || parseInt(process.env.COOKIE_SERVER_PORT || '3000')
const profileDir = config.chrome_profile || process.env.CHROME_PROFILE || '/data/chrome-profile'
const refreshIntervalHours = config.cookie_refresh_hours || parseInt(process.env.COOKIE_REFRESH_HOURS || '6')
// Stream mode: 'pion' (30fps, Go bridge) or 'direct' (werift, 10fps) or 'chrome' (experimental)
const streamMode = config.stream_mode || process.env.STREAM_MODE || 'pion'

// Parse cameras from config or env
let cameras = config.cameras || {}
if (Object.keys(cameras).length === 0 && process.env.CAMERAS) {
  for (const entry of process.env.CAMERAS.split(',')) {
    const [name, deviceId, res] = entry.trim().split(':')
    if (name && deviceId) cameras[name] = { device_id: deviceId, resolution: parseInt(res || '3') }
  }
}

if (Object.keys(cameras).length === 0) {
  console.log('[config] No cameras configured. Add cameras to config.yaml or set CAMERAS env var.')
  console.log('[config] Starting cookie management server only...')
}

// Stream management
const streams = new Map()

let browserContext = null

async function startStreams(cookies) {
  if (!cookies?.SAPISID) {
    console.log('[stream] No valid cookies.')
    return
  }

  // Pion mode handles auth internally via the Go binary
  if (streamMode !== 'pion') {
    const client = new FoyerClient(cookies, apiKey)
    const valid = await client.testAuth()
    if (!valid) {
      console.log('[stream] Cookie auth failed. Waiting for refresh...')
      return
    }
  }

  const cameraCount = Object.keys(cameras).length
  console.log(`[stream] Auth OK — starting ${cameraCount} cameras [${streamMode} mode]`)

  // Launch Chrome for chrome mode
  if (streamMode === 'chrome' && !browserContext) {
    try {
      const { chromium } = await import('playwright')
      const chromiumPath = process.env.CHROMIUM_PATH || '/home/localadmin/.cache/ms-playwright/chromium-1208/chrome-linux64/chrome'
      browserContext = await chromium.launchPersistentContext(profileDir, {
        headless: true,
        executablePath: chromiumPath,
        args: [
          '--no-sandbox',
          '--enable-features=WebRTC,Vulkan,VulkanFromANGLE,DefaultANGLEVulkan',
          '--use-angle=vulkan',
          '--enable-gpu',
          '--disable-software-rasterizer',
          '--autoplay-policy=no-user-gesture-required',
        ],
        chromiumSandbox: false,
        ignoreDefaultArgs: ['--use-angle=swiftshader-webgl', '--enable-unsafe-swiftshader', '--disable-gpu'],
      })
      console.log('[chrome] Browser launched')
    } catch (e) {
      console.log(`[chrome] Failed to launch: ${e.message}`)
      console.log('[chrome] Falling back to direct mode')
    }
  }

  for (const [name, cam] of Object.entries(cameras)) {
    const deviceId = cam.device_id
    if (!deviceId) {
      console.log(`[${name}] No device_id configured, skipping`)
      continue
    }

    let stream
    const logFn = (...args) => console.log(`[${name}]`, ...args)

    if (streamMode === 'pion') {
      stream = new PionStream({
        name,
        deviceId,
        resolution: cam.resolution || 3,
        cookiesPath,
        rtspPort,
        log: logFn,
      })
    } else if (streamMode === 'chrome' && browserContext) {
      stream = new ChromeStream({
        name,
        deviceId,
        context: browserContext,
        rtspPort,
        log: logFn,
      })
    } else {
      stream = new CameraStream({
        name,
        deviceId,
        resolution: cam.resolution || 3,
        client,
        rtspPort,
        log: logFn,
      })
    }

    streams.set(name, stream)
    stream.start()
    await new Promise(r => setTimeout(r, streamMode === 'chrome' ? 2000 : 500))
  }
}

async function restartStreams(cookies) {
  for (const [, stream] of streams) await stream.stop()
  streams.clear()
  await startStreams(cookies)
}

// Start cookie management UI (bookmarklet + API)
const cookieServer = startCookieServer(cookieServerPort, cookiesPath, (newCookies) => {
  console.log('[cookies] Updated via API — restarting streams...')
  restartStreams(newCookies)
})

// Start auto cookie refresh from Chrome profile (if playwright is available)
startCookieRefreshTimer({
  profileDir,
  cookiesPath,
  intervalMs: refreshIntervalHours * 60 * 60 * 1000,
  onRefresh: (freshCookies) => {
    console.log('[cookies] Auto-refreshed — restarting streams...')
    restartStreams(freshCookies)
  },
})

// Also try starting immediately from existing cookies file
if (existsSync(cookiesPath)) {
  // Wait a moment for the refresh timer's initial run to complete
  setTimeout(async () => {
    if (streams.size > 0) return // Already started by refresh
    try {
      const raw = JSON.parse(readFileSync(cookiesPath, 'utf-8'))
      const cookies = normalizeCookies(raw)
      await startStreams(cookies)
    } catch (e) {
      console.log(`[stream] Failed to load cookies: ${e.message}`)
    }
  }, 5000)
}

// Graceful shutdown
const shutdown = async () => {
  console.log('[shutdown] Stopping...')
  for (const [, stream] of streams) await stream.stop()
  try { await browserContext?.close() } catch {}
  cookieServer.close()
  process.exit(0)
}
process.on('SIGTERM', shutdown)
process.on('SIGINT', shutdown)

/**
 * Foyer API client — Google Home first-party API for camera streams.
 * Uses SAPISIDHASH authentication with browser cookies.
 */

import crypto from 'node:crypto'
import { readFileSync } from 'node:fs'
import axios from 'axios'

const FOYER_BASE = 'https://googlehomefoyer-pa.clients6.google.com'
const ORIGIN = 'https://home.google.com'

export class FoyerClient {
  #cookies
  #apiKey

  /**
   * @param {Record<string, string>} cookies - Google session cookies (key-value)
   * @param {string} [apiKey] - Google API key (default works for most users)
   */
  constructor(cookies, apiKey = 'AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM') {
    this.#cookies = cookies
    this.#apiKey = apiKey

    if (!cookies.SAPISID) {
      throw new Error('SAPISID cookie is required. Log in to home.google.com and extract cookies.')
    }
  }

  /**
   * Load cookies from a JSON file. Handles both formats:
   * - Simple key-value: {"SID": "...", "SAPISID": "..."}
   * - Playwright array: [{name: "SID", value: "...", domain: ".google.com"}, ...]
   */
  static fromFile(cookiesPath) {
    const raw = JSON.parse(readFileSync(cookiesPath, 'utf-8'))
    return new FoyerClient(normalizeCookies(raw))
  }

  #authHeaders() {
    const ts = Math.floor(Date.now() / 1000)
    const hash = crypto.createHash('sha1')
      .update(`${ts} ${this.#cookies.SAPISID} ${ORIGIN}`)
      .digest('hex')
    const sapisidhash = `${ts}_${hash}`

    return {
      'authorization': `SAPISIDHASH ${sapisidhash} SAPISID1PHASH ${sapisidhash} SAPISID3PHASH ${sapisidhash}`,
      'cookie': Object.entries(this.#cookies).map(([k, v]) => `${k}=${v}`).join('; '),
      'x-goog-api-key': this.#apiKey,
      'x-goog-authuser': '0',
      'x-foyer-client-environment': 'CAc=',
      'Content-Type': 'application/json',
      'Origin': ORIGIN,
      'Referer': `${ORIGIN}/`,
    }
  }

  /**
   * Join a camera stream via WebRTC.
   * @param {string} deviceId - e.g. "DEVICE_C74582B7127A396C"
   * @param {string} sdpOffer - WebRTC SDP offer
   * @param {number} [resolution=3] - 0=low, 1=SD, 2=HD, 3=Full
   * @returns {Promise<{sdp: string, mediaStreamId: string}>}
   */
  async joinStream(deviceId, sdpOffer, resolution = 3) {
    const resp = await axios.post(`${FOYER_BASE}/v1/join_stream`, {
      action: 'offer',
      deviceId,
      sdp: sdpOffer,
      requestedVideoResolution: resolution,
    }, {
      headers: this.#authHeaders(),
      timeout: 15000,
    })
    return resp.data
  }

  /** Test if cookies are still valid. */
  async testAuth() {
    try {
      await axios.post(
        `${FOYER_BASE}/v1/join_stream`,
        { action: 'offer', deviceId: 'DEVICE_TEST', sdp: 'v=0', requestedVideoResolution: 0 },
        { headers: this.#authHeaders(), timeout: 10000 },
      )
      return true // shouldn't happen with bad SDP
    } catch (e) {
      // 400 = bad SDP (auth works), 404 = device not found (auth works)
      // 401/403 = auth failed
      const status = e.response?.status
      return status === 400 || status === 404
    }
  }
}

/**
 * Normalize cookies from various formats to simple key-value.
 * Filters for .google.com domain when Playwright array format is detected.
 */
export function normalizeCookies(raw) {
  if (Array.isArray(raw)) {
    const kv = {}
    const googleOnly = raw.filter(c => c.domain === '.google.com')
    const source = googleOnly.length > 0 ? googleOnly : raw
    for (const c of source) {
      if (c.name && c.value) kv[c.name] = c.value
    }
    return kv
  }
  return raw
}


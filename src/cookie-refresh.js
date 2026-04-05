/**
 * Cookie refresh — headlessly opens Chrome with persistent profile,
 * navigates to home.google.com, extracts fresh cookies, saves to JSON.
 *
 * Runs on a timer (default every 6 hours) and on startup.
 * If cookies are invalid, logs a message to re-login manually.
 *
 * Manual re-login (only needed every few weeks):
 *   npx playwright open --user-data-dir=/data/chrome-profile https://home.google.com
 */

import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { normalizeCookies, FoyerClient } from './foyer.js'

const DEFAULT_PROFILE_DIR = '/data/chrome-profile'
const DEFAULT_CHROMIUM = process.env.CHROMIUM_PATH || 'chromium'

/**
 * Extract fresh cookies from the persistent Chrome profile.
 * @param {object} opts
 * @param {string} opts.profileDir - Chrome profile directory
 * @param {string} opts.cookiesPath - Output cookies.json path
 * @param {string} [opts.chromiumPath] - Path to Chromium binary
 * @returns {Promise<Record<string, string>|null>} Normalized cookies or null on failure
 */
export async function refreshCookies({ profileDir, cookiesPath, chromiumPath }) {
  let chromium
  try {
    chromium = (await import('playwright')).chromium
  } catch {
    console.log('[cookies] playwright not installed — skipping auto-refresh')
    console.log('[cookies] Install: npm install playwright && npx playwright install chromium')
    return null
  }

  console.log('[cookies] Refreshing from Chrome profile...')
  let ctx
  try {
    ctx = await chromium.launchPersistentContext(profileDir || DEFAULT_PROFILE_DIR, {
      headless: true,
      executablePath: chromiumPath || DEFAULT_CHROMIUM,
      args: ['--no-sandbox', '--disable-dev-shm-usage'],
    })

    const page = await ctx.newPage()
    await page.goto('https://home.google.com', { waitUntil: 'networkidle', timeout: 20000 }).catch(() => {})

    // Check if we're actually logged in
    const url = page.url()
    if (url.includes('accounts.google.com') || url.includes('welcome')) {
      console.log('[cookies] NOT LOGGED IN — manual login required:')
      console.log('[cookies]   npx playwright open --user-data-dir=' + (profileDir || DEFAULT_PROFILE_DIR) + ' https://home.google.com')
      await ctx.close()
      return null
    }

    // Extract cookies for google.com
    const allCookies = await ctx.cookies('https://home.google.com')
    await ctx.close()
    ctx = null

    const cookies = normalizeCookies(allCookies)
    if (!cookies.SAPISID) {
      console.log('[cookies] Extracted cookies but no SAPISID found')
      return null
    }

    // Validate
    const client = new FoyerClient(cookies)
    const valid = await client.testAuth()
    if (!valid) {
      console.log('[cookies] Extracted cookies are invalid — session may have expired')
      console.log('[cookies] Manual re-login required:')
      console.log('[cookies]   npx playwright open --user-data-dir=' + (profileDir || DEFAULT_PROFILE_DIR) + ' https://home.google.com')
      return null
    }

    // Save
    writeFileSync(cookiesPath, JSON.stringify(cookies, null, 2))
    console.log(`[cookies] Refreshed — ${Object.keys(cookies).length} cookies saved (auth valid)`)
    return cookies
  } catch (err) {
    console.log(`[cookies] Refresh failed: ${err.message}`)
    if (ctx) await ctx.close().catch(() => {})
    return null
  }
}

/**
 * Start a periodic cookie refresh timer.
 * @param {object} opts
 * @param {string} opts.profileDir - Chrome profile directory
 * @param {string} opts.cookiesPath - Output cookies.json path
 * @param {number} [opts.intervalMs=21600000] - Refresh interval (default 6 hours)
 * @param {function} [opts.onRefresh] - Callback with fresh cookies
 * @returns {ReturnType<typeof setInterval>}
 */
export function startCookieRefreshTimer({ profileDir, cookiesPath, intervalMs = 6 * 60 * 60 * 1000, onRefresh }) {
  const doRefresh = async () => {
    const cookies = await refreshCookies({ profileDir, cookiesPath })
    if (cookies && onRefresh) onRefresh(cookies)
  }

  // Initial refresh on startup
  doRefresh()

  // Periodic refresh
  const timer = setInterval(doRefresh, intervalMs)
  console.log(`[cookies] Auto-refresh every ${intervalMs / 3600000}h`)
  return timer
}

/**
 * Check if existing cookies are still valid.
 */
export async function checkCookies(cookiesPath) {
  if (!existsSync(cookiesPath)) return { valid: false, reason: 'no file' }
  try {
    const raw = JSON.parse(readFileSync(cookiesPath, 'utf-8'))
    const cookies = normalizeCookies(raw)
    if (!cookies.SAPISID) return { valid: false, reason: 'no SAPISID' }
    const client = new FoyerClient(cookies)
    const valid = await client.testAuth()
    return { valid, reason: valid ? 'ok' : 'auth failed' }
  } catch (e) {
    return { valid: false, reason: e.message }
  }
}

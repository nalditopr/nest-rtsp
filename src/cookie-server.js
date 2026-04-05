/**
 * Tiny HTTP server for cookie management.
 *
 * Endpoints:
 *   GET  /           — Status page + bookmarklet
 *   POST /cookies    — Receive cookies (from bookmarklet or API)
 *   GET  /health     — Cookie validity check
 *
 * The bookmarklet runs on home.google.com and POSTs all cookies
 * to this server with one click. No browser extension needed.
 */

import { createServer } from 'node:http'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { normalizeCookies, FoyerClient } from './foyer.js'

export function startCookieServer(port, cookiesPath, onCookiesUpdated) {
  const server = createServer(async (req, res) => {
    // CORS for bookmarklet
    res.setHeader('Access-Control-Allow-Origin', 'https://home.google.com')
    res.setHeader('Access-Control-Allow-Methods', 'POST, GET, OPTIONS')
    res.setHeader('Access-Control-Allow-Headers', 'Content-Type')

    if (req.method === 'OPTIONS') {
      res.writeHead(204)
      res.end()
      return
    }

    if (req.method === 'GET' && req.url === '/') {
      res.writeHead(200, { 'Content-Type': 'text/html' })
      res.end(statusPage(port, cookiesPath))
      return
    }

    if (req.method === 'GET' && req.url === '/health') {
      try {
        const cookies = loadCookies(cookiesPath)
        const client = new FoyerClient(cookies)
        const valid = await client.testAuth()
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ status: valid ? 'ok' : 'expired', cookies: !!cookies.SAPISID }))
      } catch (e) {
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ status: 'error', message: e.message }))
      }
      return
    }

    if (req.method === 'POST' && req.url === '/cookies') {
      let body = ''
      req.on('data', (chunk) => { body += chunk })
      req.on('end', async () => {
        try {
          const raw = JSON.parse(body)
          const cookies = normalizeCookies(raw.cookies || raw)
          if (!cookies.SAPISID) throw new Error('No SAPISID cookie found')

          writeFileSync(cookiesPath, JSON.stringify(cookies, null, 2))
          console.log(`[cookies] Updated — ${Object.keys(cookies).length} cookies saved`)

          // Validate
          const client = new FoyerClient(cookies)
          const valid = await client.testAuth()
          console.log(`[cookies] Auth test: ${valid ? 'VALID' : 'FAILED'}`)

          if (valid && onCookiesUpdated) onCookiesUpdated(cookies)

          res.writeHead(200, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify({ status: 'ok', valid, count: Object.keys(cookies).length }))
        } catch (e) {
          res.writeHead(400, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify({ status: 'error', message: e.message }))
        }
      })
      return
    }

    res.writeHead(404)
    res.end('Not found')
  })

  server.listen(port, '0.0.0.0', () => {
    console.log(`[cookies] Management UI: http://localhost:${port}`)
  })

  return server
}

function loadCookies(path) {
  if (!existsSync(path)) return {}
  return normalizeCookies(JSON.parse(readFileSync(path, 'utf-8')))
}

function statusPage(port, cookiesPath) {
  const hasCookies = existsSync(cookiesPath)
  const bookmarklet = `javascript:void(fetch('http://${getHost()}:${port}/cookies',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({cookies:document.cookie.split(';').map(c=>{const[n,...v]=c.trim().split('=');return{name:n,value:v.join('=')}}).reduce((a,c)=>(a[c.name]=c.value,a),{})})}).then(r=>r.json()).then(d=>alert('nest-rtsp: '+d.status+(d.valid?' (valid)':' (invalid)'))).catch(e=>alert('Error: '+e.message)))`

  return `<!DOCTYPE html>
<html><head><title>nest-rtsp</title>
<style>body{font-family:system-ui;max-width:600px;margin:40px auto;padding:0 20px}
.status{padding:12px;border-radius:8px;margin:16px 0}
.ok{background:#d4edda;color:#155724}.warn{background:#fff3cd;color:#856404}
.err{background:#f8d7da;color:#721c24}
a.bookmarklet{display:inline-block;padding:10px 20px;background:#4285f4;color:white;
text-decoration:none;border-radius:6px;font-weight:bold;margin:8px 0}
code{background:#f4f4f4;padding:2px 6px;border-radius:3px}</style></head>
<body>
<h1>nest-rtsp</h1>
<p>Direct RTSP streams from Nest cameras — no Chrome, no cloud relay.</p>

<div class="status ${hasCookies ? 'ok' : 'warn'}">
  ${hasCookies ? 'Cookies loaded. Check <a href="/health">/health</a> to verify.' : 'No cookies yet. Use the bookmarklet below to set up.'}
</div>

<h2>Setup</h2>
<ol>
  <li>Go to <a href="https://home.google.com" target="_blank">home.google.com</a> and log in</li>
  <li>Drag this to your bookmarks bar: <a class="bookmarklet" href="${bookmarklet}">Send Cookies to nest-rtsp</a></li>
  <li>Click the bookmarklet while on home.google.com</li>
  <li>You should see "nest-rtsp: ok (valid)"</li>
</ol>

<h2>Limitations</h2>
<p>The bookmarklet can only access non-httpOnly cookies (like SAPISID). For full cookie access, use the CLI method:</p>
<pre>docker exec nest-rtsp node src/extract-cookies.js</pre>
<p>Or mount a <code>cookies.json</code> file extracted from your browser.</p>

<h2>API</h2>
<ul>
  <li><code>GET /health</code> — Check cookie validity</li>
  <li><code>POST /cookies</code> — Update cookies (JSON body)</li>
</ul>
</body></html>`
}

function getHost() {
  const { networkInterfaces } = require('node:os')
  const nets = networkInterfaces()
  for (const name of Object.keys(nets)) {
    for (const net of nets[name]) {
      if (net.family === 'IPv4' && !net.internal) return net.address
    }
  }
  return 'localhost'
}

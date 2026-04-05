/**
 * Simple UDP port picker — finds an available port in the ephemeral range.
 */

import { createSocket } from 'node:dgram'

let nextPort = 10000

export async function pickPort() {
  for (let i = 0; i < 100; i++) {
    const port = nextPort++
    if (nextPort > 20000) nextPort = 10000
    try {
      await new Promise((resolve, reject) => {
        const sock = createSocket('udp4')
        sock.on('error', reject)
        sock.bind(port, '0.0.0.0', () => {
          sock.close()
          resolve()
        })
      })
      return port
    } catch {
      continue
    }
  }
  throw new Error('No available UDP ports in range 10000-20000')
}

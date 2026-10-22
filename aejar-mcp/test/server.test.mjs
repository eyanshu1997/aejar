// Drives server.mjs the way Claude Code does: MCP JSON-RPC over stdio. A fake
// aejard records calls; events written to the channel socket must come out
// as notifications/claude/channel.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import http from 'node:http'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import fs from 'node:fs'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))

function fakeAejard() {
  const calls = []
  const srv = http.createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      calls.push({ method: req.method, url: req.url, auth: req.headers.authorization, body: body ? JSON.parse(body) : null })
      res.setHeader('content-type', 'application/json')
      if (req.url === '/v1/approvals') return res.end(JSON.stringify({ id: 'a-test1234', state: 'APPROVAL_STATE_PENDING' }))
      res.end('{}')
    })
  })
  return new Promise((r) => srv.listen(0, '127.0.0.1', () => r({ srv, calls, url: `http://127.0.0.1:${srv.address().port}` })))
}

function startServer(env) {
  const child = spawn(process.execPath, [path.join(here, '..', 'server.mjs')], { env: { ...process.env, ...env }, stdio: ['pipe', 'pipe', 'pipe'] })
  const messages = []
  let buf = ''
  const waiters = []
  child.stdout.on('data', (d) => {
    buf += d
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const msg = JSON.parse(buf.slice(0, i))
      buf = buf.slice(i + 1)
      messages.push(msg)
      for (const w of [...waiters]) if (w.match(msg)) { waiters.splice(waiters.indexOf(w), 1); w.resolve(msg) }
    }
  })
  const send = (msg) => child.stdin.write(JSON.stringify(msg) + '\n')
  const waitFor = (match) => {
    const hit = messages.find(match)
    if (hit) return Promise.resolve(hit)
    return new Promise((resolve, reject) => {
      waiters.push({ match, resolve })
      setTimeout(() => reject(new Error('timeout waiting for message')), 5000)
    })
  }
  return { child, send, waitFor }
}

async function waitForFile(p) {
  for (let i = 0; i < 100; i++) {
    if (fs.existsSync(p)) return
    await new Promise((r) => setTimeout(r, 50))
  }
  throw new Error('socket never appeared')
}

test('channel events and approval tools', async () => {
  const aejard = await fakeAejard()
  const sock = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'aejar-mcp-')), 'channel.sock')
  const s = startServer({ AEJAR_URL: aejard.url, AEJAR_SESSION_TOKEN: 'sess-token', AEJAR_CHANNEL_SOCKET: sock })
  try {
    s.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'test', version: '0' } } })
    const init = await s.waitFor((m) => m.id === 1)
    assert.deepEqual(init.result.capabilities.experimental, { 'claude/channel': {} })
    s.send({ jsonrpc: '2.0', method: 'notifications/initialized' })

    // Channel delivery.
    await waitForFile(sock)
    await new Promise((resolve, reject) => {
      const c = net.createConnection(sock, () => {
        c.end(JSON.stringify({ id: 'e-1', kind: 'comment', content: 'please split step 3', meta: { doc: 'plan.html', 'bad-key': 'x' } }) + '\n')
        resolve()
      })
      c.on('error', reject)
    })
    const note = await s.waitFor((m) => m.method === 'notifications/claude/channel')
    assert.equal(note.params.content, 'please split step 3')
    assert.deepEqual(note.params.meta, { event_id: 'e-1', kind: 'comment', doc: 'plan.html' })

    // Readiness is not this process's call (it cannot see registration).
    assert.ok(!aejard.calls.some((c) => c.url === '/hooks/claude/channel-ready'))

    // request_approval forwards the exact tool input.
    const input = { to: 'x@example.com', body: 'hi' }
    s.send({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'request_approval', arguments: { summary: 'send it', tool_name: 'mcp__claude_ai_Gmail__send_message', tool_input: input } } })
    const res = await s.waitFor((m) => m.id === 2)
    assert.match(res.result.content[0].text, /a-test1234 is pending/)
    const call = aejard.calls.find((c) => c.url === '/v1/approvals')
    assert.equal(call.auth, 'Bearer sess-token')
    assert.equal(call.body.class, 'APPROVAL_CLASS_ACTION')
    assert.equal(call.body.toolName, 'mcp__claude_ai_Gmail__send_message')
    assert.deepEqual(call.body.payload, input)
  } finally {
    s.child.kill()
    aejard.srv.close()
  }
})

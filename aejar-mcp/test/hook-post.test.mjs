// hook-post is the PreToolUse gate's transport: it must pass aejard's answer
// through, and block (exit 2) whenever aejard cannot answer.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { spawn } from 'node:child_process'
import http from 'node:http'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const hookPost = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'images', 'session', 'hook-post.mjs')

function run(route, url, input = '{"tool_name":"Bash"}') {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [hookPost, route], { env: { ...process.env, AEJAR_URL: url, AEJAR_SESSION_TOKEN: 'tok' } })
    let out = '', err = ''
    child.stdout.on('data', (d) => (out += d))
    child.stderr.on('data', (d) => (err += d))
    child.on('exit', (code) => resolve({ code, out, err }))
    child.stdin.end(input)
  })
}

function server(status, body) {
  const seen = []
  const srv = http.createServer((req, res) => {
    let b = ''
    req.on('data', (c) => (b += c))
    req.on('end', () => { seen.push({ url: req.url, auth: req.headers.authorization, body: b }); res.statusCode = status; res.end(body) })
  })
  return new Promise((r) => srv.listen(0, '127.0.0.1', () => r({ srv, seen, url: `http://127.0.0.1:${srv.address().port}` })))
}

test('passes the decision through', async () => {
  const deny = JSON.stringify({ hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny' } })
  const s = await server(200, deny)
  const r = await run('pre-tool', s.url)
  s.srv.close()
  assert.equal(r.code, 0)
  assert.equal(r.out, deny)
  assert.equal(s.seen[0].url, '/hooks/claude/pre-tool')
  assert.equal(s.seen[0].auth, 'Bearer tok')
  assert.equal(s.seen[0].body, '{"tool_name":"Bash"}')
})

test('pre-tool blocks when aejard is unreachable', async () => {
  const r = await run('pre-tool', 'http://127.0.0.1:1')
  assert.equal(r.code, 2)
  assert.match(r.err, /blocked/)
})

test('pre-tool blocks when aejard errors', async () => {
  const s = await server(500, '{"message":"boom"}')
  const r = await run('pre-tool', s.url)
  s.srv.close()
  assert.equal(r.code, 2)
})

test('other events never block', async () => {
  const r = await run('post-tool', 'http://127.0.0.1:1')
  assert.equal(r.code, 0)
})

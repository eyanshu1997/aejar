// Forwards a Claude Code hook event (JSON on stdin) to aejard and prints
// aejard's answer for Claude Code to apply.
//
// For pre-tool the gate must fail closed: if aejard cannot be reached or
// answers with an error, exit 2, which makes Claude Code block the tool call.
// Other events never block the session.
const route = process.argv[2]
const failClosed = route === 'pre-tool'

function fail(msg) {
  if (failClosed) {
    process.stderr.write(`aejar gate unavailable (${msg}); this tool call is blocked. Do not retry it; tell the owner.\n`)
    process.exit(2)
  }
  process.stderr.write(`hook-post ${route}: ${msg}\n`)
  process.exit(0)
}

let body = ''
process.stdin.setEncoding('utf8')
process.stdin.on('data', (c) => (body += c))
process.stdin.on('end', async () => {
  const timer = setTimeout(() => fail('timeout'), 20000)
  try {
    const res = await fetch(`${process.env.AEJAR_URL}/hooks/claude/${route}`, {
      method: 'POST',
      headers: { authorization: `Bearer ${process.env.AEJAR_SESSION_TOKEN}`, 'content-type': 'application/json' },
      body: body || '{}',
    })
    const text = await res.text()
    clearTimeout(timer)
    if (!res.ok) fail(`aejard ${res.status}: ${text.slice(0, 200)}`)
    process.stdout.write(text)
    process.exit(0)
  } catch (err) {
    fail(err.message)
  }
})

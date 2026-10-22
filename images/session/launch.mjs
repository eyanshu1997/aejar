// Runs claude with the arguments the spawner generated for this session,
// inheriting tmux's terminal. Failures are also written to the container's
// log (PID 1's stderr), since the tmux pane disappears with the session.
import { spawn } from 'node:child_process'
import fs from 'node:fs'

function containerLog(msg) {
  try { fs.appendFileSync('/proc/1/fd/2', `[aejar] ${msg}\n`) } catch {}
  process.stderr.write(`[aejar] ${msg}\n`)
}

let args
try {
  ;({ args } = JSON.parse(fs.readFileSync('/etc/aejar/launch.json', 'utf8')))
} catch (err) {
  containerLog(`cannot read /etc/aejar/launch.json: ${err.message}`)
  process.exit(1)
}
const child = spawn('claude', args, { stdio: 'inherit', cwd: '/work' })
child.on('error', (err) => { containerLog(`cannot start claude: ${err.message}`); process.exit(1) })
child.on('exit', (code, signal) => {
  containerLog(`claude exited (code ${code}, signal ${signal})`)
  process.exit(code ?? (signal ? 1 : 0))
})
for (const sig of ['SIGTERM', 'SIGINT', 'SIGHUP']) process.on(sig, () => child.kill(sig))

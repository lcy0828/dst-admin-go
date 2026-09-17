#!/usr/bin/env node
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'

const [image, seconds = '180'] = process.argv.slice(2)
const duration = Number(seconds)
assert(image && Number.isFinite(duration) && duration >= 1 && duration <= 600, 'usage: smoke-agent-image.mjs IMAGE [SECONDS]')
const run = args => execFileSync('docker', args, { encoding: 'utf8', timeout: 30000 }).trim()
const name = `dst-agent-smoke-${process.pid}-${Date.now()}`
let created = false
try {
  const version = run(['run', '--rm', '--entrypoint', '/usr/local/bin/dst-admin-agent', image, '-version'])
  assert.match(version, /^\d+\.\d+\.\d+/)
  // No host paths, real Controller, game processes, or Docker socket are used.
  // The unavailable loopback endpoint exercises the Agent's reconnect startup.
  run(['run', '--detach', '--name', name,
    '--env', 'DST_ADMIN_AGENT_SERVER_URL=ws://127.0.0.1:1/agent',
    '--env', `DST_ADMIN_AGENT_SECURITY_KEY=${randomBytes(32).toString('hex')}`, image])
  created = true
  const deadline = Date.now() + duration * 1000
  while (Date.now() < deadline) {
    assert.equal(run(['inspect', '--format', '{{.State.Running}}', name]), 'true', 'Agent exited during startup')
    await new Promise(resolve => setTimeout(resolve, Math.min(2000, deadline - Date.now())))
  }
  run(['exec', name, 'test', '-s', '/var/lib/dst-admin-agent/agent.conf'])
  run(['stop', '--time', '20', name])
  assert.equal(run(['inspect', '--format', '{{.State.ExitCode}}', name]), '0', 'Agent did not stop cleanly')
  console.log(`Agent ${version}: startup, reconnect, configuration, ${duration}s uptime and graceful shutdown passed`)
} finally {
  if (created) run(['rm', '--force', '--volumes', name])
}

#!/usr/bin/env node
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from 'node:fs/promises'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const options = {}
for (let i = 2; i < process.argv.length; i += 2) {
  if (!['--binary', '--image', '--kind', '--duration', '--frontend-commit'].includes(process.argv[i]) || !process.argv[i + 1]) throw new Error('usage: smoke-package.mjs (--binary PATH | --image TAG --kind all-in-one|control-plane) [--duration SECONDS] [--frontend-commit SHA]')
  options[process.argv[i].slice(2)] = process.argv[i + 1]
}
assert(Boolean(options.binary) !== Boolean(options.image), 'choose a binary or image')
const duration = Number(options.duration || 15)
assert(Number.isFinite(duration) && duration >= 1 && duration <= 600, 'invalid duration')
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
const run = (cmd, args) => execFileSync(cmd, args, { encoding: 'utf8', timeout: 30000 }).trim()
const stage = await realpath(await mkdtemp(path.join(os.tmpdir(), 'dst-package-smoke-')))
const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('DST_ADMIN_') && key !== 'PORT'))
let processHandle, container, baseURL, log = '', exited = false
try {
  let metadata
  if (options.binary) {
    const binary = path.resolve(options.binary)
    metadata = JSON.parse(execFileSync(binary, ['-version'], { cwd: stage, env: environment, encoding: 'utf8', timeout: 30000 }))
    const template = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../systemd/local.conf.example')
    const config = (await readFile(template, 'utf8')).replaceAll('/var/lib/dst-admin', path.join(stage, 'control')).replaceAll('/opt/dst', path.join(stage, 'data'))
    for (const name of ['control', 'data/server', 'data/saves', 'data/backups', 'data/maps', 'data/workshop/steamapps/workshop/content/322330']) await mkdir(path.join(stage, name), { recursive: true })
    await writeFile(path.join(stage, 'app.conf'), config, { mode: 0o600 })
    const socket = net.createServer()
    await new Promise((resolve, reject) => { socket.once('error', reject); socket.listen(0, '127.0.0.1', resolve) })
    const port = socket.address().port
    await new Promise(resolve => socket.close(resolve))
    baseURL = `http://127.0.0.1:${port}`
    processHandle = spawn(binary, ['-addr', `127.0.0.1:${port}`], {
      cwd: stage, env: { ...environment, GIN_MODE: 'release', DST_ADMIN_CONFIG: path.join(stage, 'app.conf'), DST_ADMIN_PACKAGING: 'control_plane', DST_ADMIN_LOCAL_EXECUTOR_ENABLED: 'false', DST_ADMIN_FLEET_CONTROLLER_ENABLED: 'true', DST_ADMIN_FLEET_MEMBER_ENABLED: 'false' },
      stdio: ['ignore', 'pipe', 'pipe']
    })
    processHandle.on('error', error => { exited = true; log += String(error) })
    processHandle.on('exit', () => { exited = true })
    for (const stream of [processHandle.stdout, processHandle.stderr]) stream.on('data', chunk => { log = (log + chunk).slice(-200000) })
  } else {
    assert(['all-in-one', 'control-plane'].includes(options.kind), 'invalid web image kind')
    metadata = JSON.parse(run('docker', ['run', '--rm', '--entrypoint', '/usr/local/bin/dst-admin', options.image, '-version']))
    const port = options.kind === 'all-in-one' ? '8080' : '8000'
    container = `dst-package-smoke-${process.pid}-${Date.now()}`
    run('docker', ['run', '--detach', '--name', container, '--publish', `127.0.0.1::${port}`, '--env', 'DST_ADMIN_BOOTSTRAP_DST=false', options.image])
    const address = run('docker', ['port', container, `${port}/tcp`])
    assert(/^127\.0\.0\.1:\d+$/.test(address), 'unexpected Docker port mapping')
    baseURL = `http://${address}`
  }
  assert.equal(metadata.embeddedWebUI, true)
  assert.match(metadata.frontendCommit, /^[a-f0-9]{40}$/)
  if (options['frontend-commit']) assert.equal(metadata.frontendCommit, options['frontend-commit'])
  const get = (resource, extra = {}) => fetch(baseURL + resource, { redirect: 'error', signal: AbortSignal.timeout(4000), ...extra })
  const deadline = Date.now() + 60000
  while (true) {
    assert(!exited, 'service exited during startup')
    try { if ((await get('/api/v2/auth/session')).ok) break } catch {}
    assert(Date.now() < deadline, 'service startup timed out')
    await delay(500)
  }
  const started = Date.now()
  const root = await get('/')
  assert.equal(root.status, 200)
  assert.match(root.headers.get('content-type') || '', /text\/html/)
  const html = await root.text()
  assert.match(root.headers.get('cache-control') || '', /no-cache/)
  const asset = html.match(/(?:src|href)="(\/assets\/[^" ]+\.js)"/)?.[1]
  assert(asset, 'frontend JavaScript asset is missing')
  for (const route of ['/setup', '/rooms', '/index.html']) {
    const response = await get(route)
    assert.equal(response.status, 200, route)
    assert.equal(await response.text(), html, route)
  }
  const script = await get(asset)
  assert.equal(script.status, 200)
  assert.match(script.headers.get('cache-control') || '', /immutable/)
  assert((await script.text()).length > 100)
  const head = await get('/setup', { method: 'HEAD' })
  assert.equal(head.status, 200)
  assert.equal(await head.text(), '')
  assert.equal((await get('/assets/nonexistent.js')).status, 404)
  while (Date.now() - started < duration * 1000) {
    assert(!exited, 'service exited during stability check')
    const session = await get('/api/v2/auth/session')
    assert.equal(session.status, 200)
    await session.json()
    assert.equal((await get('/setup')).status, 200)
    await delay(Math.min(1000, duration * 1000 - (Date.now() - started)))
  }
  console.log(`Package smoke passed: embedded frontend ${metadata.frontendCommit}; UI, assets, API and ${duration}s uptime`)
} catch (error) {
  if (container) { try { log = run('docker', ['logs', '--tail', '100', container]) } catch {} }
  console.error(log)
  throw error
} finally {
  if (processHandle && !exited) {
    processHandle.kill('SIGTERM')
    const deadline = Date.now() + 25000
    while (!exited && Date.now() < deadline) await delay(100)
    if (!exited) {
      processHandle.kill('SIGKILL')
      await new Promise(resolve => processHandle.once('exit', resolve))
    }
  }
  if (container) run('docker', ['rm', '--force', '--volumes', container])
  await rm(stage, { recursive: true, force: true })
}

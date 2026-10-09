#!/usr/bin/env node
// Every path is generated under an isolated temporary directory. No operator
// data, Docker socket, real game installation or existing Controller is used.
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'

const options = {}
for (let index = 2; index < process.argv.length; index += 2) {
  assert(['--controller', '--binary', '--image', '--bundle', '--read-only'].includes(process.argv[index]) && process.argv[index + 1], 'invalid Agent smoke option')
  options[process.argv[index].slice(2)] = process.argv[index + 1]
}
assert(options.controller && options.bundle && Boolean(options.binary) !== Boolean(options.image), 'choose --controller CONTROLLER --bundle ARCHIVE and --binary AGENT or --image IMAGE')
const run = (command, args, extra = {}) => execFileSync(command, args, { encoding: 'utf8', timeout: 60000, ...extra }).trim()
const stage = await realpath(await mkdtemp(path.join(os.tmpdir(), 'dst-agent-update-')))
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('DST_ADMIN_')))
const key = randomBytes(32).toString('base64')
let controller, agent, room, container, cookie = '', csrf = '', log = '', agentID, roomPID, agentArgs, baseURL, agentConfig, dataRoot
const children = []
const readFixture = (hostPath, containerPath) => container
  ? execFileSync('docker', ['exec', '--user', '10000:10000', container, 'cat', containerPath], { timeout: 60000 })
  : readFile(hostPath)
function start(binary, args, env) {
  const child = spawn(binary, args, { cwd: stage, env: { ...cleanEnv, GIN_MODE: 'release', ...env }, stdio: ['ignore', 'pipe', 'pipe'] })
  children.push(child)
  for (const stream of [child.stdout, child.stderr]) stream.on('data', data => { log = (log + data).slice(-80000) })
  child.on('error', error => { log += error.message })
  return child
}
async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode) return
  await new Promise(resolve => { const timer = setTimeout(() => child.kill('SIGKILL'), 25000); child.once('exit', () => { clearTimeout(timer); resolve() }); child.kill('SIGTERM') })
}
async function waitFor(load, timeout = 90000) {
  const deadline = Date.now() + timeout; let error
  while (Date.now() < deadline) {
    try { const value = await load(); if (value) return value } catch (failure) { error = failure }
    await new Promise(resolve => setTimeout(resolve, 300))
  }
  throw error || Error('Agent update timed out')
}
try {
  const listener = net.createServer(); await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve))
  const port = listener.address().port; await new Promise(resolve => listener.close(resolve)); baseURL = `http://127.0.0.1:${port}`
  const controlRoot = path.join(stage, 'control'); dataRoot = path.join(stage, 'agent')
  await mkdir(controlRoot); await mkdir(dataRoot, { mode: 0o700 })
  for (const directory of ['server', 'saves', 'backups', 'maps', 'workshop/steamapps/workshop/content/322330']) await mkdir(path.join(stage, 'game', directory), { recursive: true })
  const template = (await readFile('deploy/systemd/local.conf.example', 'utf8')).replaceAll('/var/lib/dst-admin', controlRoot).replaceAll('/opt/dst', path.join(stage, 'game'))
  const controllerConfig = path.join(controlRoot, 'app.conf'); await writeFile(controllerConfig, template, { mode: 0o600 })
  controller = start(path.resolve(options.controller), ['-addr', options.image ? `0.0.0.0:${port}` : `127.0.0.1:${port}`], { DST_ADMIN_CONFIG: controllerConfig, DST_ADMIN_AGENT_SECURITY_KEY: key, DST_ADMIN_FLEET_CONTROLLER_ENABLED: 'true', DST_ADMIN_LOCAL_EXECUTOR_ENABLED: 'false' })
  async function api(resource, input) {
    const response = await fetch(baseURL + '/api/v2' + resource, { signal: AbortSignal.timeout(35000), headers: { ...(cookie ? { Cookie: cookie } : {}), ...(input ? { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, 'Idempotency-Key': randomBytes(16).toString('hex') } : {}) }, ...(input ? { method: 'POST', body: JSON.stringify(input) } : {}) })
    if (response.headers.get('set-cookie')) cookie = response.headers.get('set-cookie').split(';')[0]
    const value = await response.json(); assert(response.ok, `${response.status}: ${JSON.stringify(value)}`); return value.data
  }
  await waitFor(() => api('/auth/session'))
  console.log('Isolated management API started.')
  csrf = (await api('/auth/setup', { username: 'agent-update-smoke', password: 'AgentUpdateSmoke123!' })).csrfToken
  const archive = path.resolve(options.bundle), extracted = path.join(stage, 'bundle'); await mkdir(extracted)
  run('tar', ['-xzf', archive, '-C', extracted])
  const manifest = JSON.parse(await readFile(path.join(extracted, 'manifest.json')))
  if (!/^v\d+\.\d+\.\d+$/.test(manifest.version)) { console.log('Agent lifecycle requires a stable build; package validation still applies.'); process.exitCode = 0 }
  else {
    assert.equal(manifest.kind, 'agent')
    agentConfig = path.join(dataRoot, 'agent.conf')
    const config = `[agent]\nSERVER_URL = ws://${options.image ? 'host.docker.internal' : '127.0.0.1'}:${port}/agent\nSECURITY_KEY = ${key}\n`
    await writeFile(agentConfig, config, { mode: 0o600 })
    const save = path.join(dataRoot, 'save-sentinel'); await writeFile(save, 'preserve saved worlds')
    const agentRoot = path.join(dataRoot, 'software-updates')
    if (options.binary) {
      const metadata = JSON.parse(run(path.resolve(options.binary), ['-build-info']))
      assert.match(metadata.version, /^v\d+\.\d+\.\d+$/)
      options.startAgent = () => { agent = start(path.resolve(options.binary), ['-config', agentConfig, '-state', path.join(dataRoot, 'runtime-state.json')], { DST_ADMIN_AGENT_UPDATE_DIR: agentRoot }); return agent }
      options.startAgent()
      room = spawn('sleep', ['600']); roomPID = room.pid
    } else {
      // Match the official runtime UID so its private-file checks can chmod
      // the generated configuration on Linux hosts with real UID boundaries.
      run('docker', ['run', '--rm', '--user', '0', '--entrypoint', '/bin/chown', '--mount', `type=bind,src=${dataRoot},dst=/fixture`, options.image, '-R', '10000:10000', '/fixture'])
      container = `dst-agent-update-${process.pid}-${Date.now()}`
      agentArgs = ['run', '--detach', '--platform', 'linux/amd64', '--name', container, '--add-host', 'host.docker.internal:host-gateway', '--mount', `type=bind,src=${dataRoot},dst=/var/lib/dst-admin-agent`, '--tmpfs', '/tmp:mode=1777,size=64m', '--tmpfs', '/run:mode=0755,size=16m']
      if (options['read-only'] === 'true') agentArgs.push('--read-only')
      agentArgs.push(options.image); run('docker', agentArgs)
      run('docker', ['exec', container, 'tmux', 'new-session', '-d', '-s', 'update-smoke-room', '/bin/sleep', '600'])
      roomPID = run('docker', ['exec', container, 'tmux', 'display-message', '-p', '-t', 'update-smoke-room', '#{pane_pid}'])
    }
    async function connectedAgent() {
      const result = await api('/agents'); const value = result.items?.find(item => item.status === 'online' && item.capabilities?.includes('agent.software-update.v1'))
      if (value) agentID = value.id
      return value
    }
    await waitFor(connectedAgent)
    console.log('Agent connected over its authenticated control channel.')
    const initial = await waitFor(async () => { const value = await api(`/agents/${agentID}/software`); return value.ready ? value : null })
    const launcherPID = agent?.pid
    const statePath = path.join(agentRoot, 'state.json'), store = '/var/lib/dst-admin-agent/software-updates'
    const readStore = async () => JSON.parse(container ? run('docker', ['exec', container, 'cat', store + '/state.json']) : await readFile(statePath, 'utf8'))
    const state = await readStore(), releaseID = randomBytes(16).toString('hex'), operationID = randomBytes(16).toString('hex')
    state.operation = { id: operationID, releaseId: releaseID, version: manifest.version, phase: 'prepared', progress: 100, startedAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
    if (container) {
      run('docker', ['cp', archive, container + ':' + store + '/.fixture.tar.gz'])
      run('docker', ['exec', container, 'mkdir', '-p', store + '/releases/' + releaseID])
      run('docker', ['exec', container, 'tar', '-xzf', store + '/.fixture.tar.gz', '-C', store + '/releases/' + releaseID])
      run('docker', ['exec', container, 'rm', store + '/.fixture.tar.gz'])
      run('docker', ['exec', '--interactive', container, 'sh', '-c', 'cat > "$1.fixture" && mv "$1.fixture" "$1"', 'fixture', store + '/state.json'], { input: JSON.stringify(state) })
    } else {
      await cp(extracted, path.join(agentRoot, 'releases', releaseID), { recursive: true })
      await writeFile(statePath + '.fixture', JSON.stringify(state), { mode: 0o600 }); run('mv', [statePath + '.fixture', statePath])
    }
    const readConfig = () => readFixture(agentConfig, '/var/lib/dst-admin-agent/agent.conf')
    const readSave = async () => (await readFixture(save, '/var/lib/dst-admin-agent/save-sentinel')).toString('utf8')
    const before = await readConfig()
    async function waitJob(job) {
      const value = await waitFor(async () => { const value = await api(`/jobs/${job.id}`); return ['succeeded', 'failed', 'canceled'].includes(value.status) ? value : null }, 120000)
      assert.equal(value.status, 'succeeded', JSON.stringify(value))
      return value
    }
    await waitJob(await api(`/agents/${agentID}/software/actions/update`, { version: manifest.version, confirmation: manifest.version, operationId: operationID }))
    const applied = await api(`/agents/${agentID}/software`)
    assert.equal(applied.current.version, manifest.version); assert.equal(applied.ready, true)
    assert.equal((await readStore()).current.id, releaseID)
    assert.deepEqual(await readConfig(), before); assert.equal(await readSave(), 'preserve saved worlds')
    if (agent) assert.equal(agent.pid, launcherPID, 'stable Agent launcher PID changed')
    if (room) assert.equal(room.exitCode, null, 'simulated game process stopped')
    if (container) assert.equal(run('docker', ['exec', container, 'tmux', 'display-message', '-p', '-t', 'update-smoke-room', '#{pane_pid}']), roomPID)
    if (agent) { await stop(agent); options.startAgent() }
    else { run('docker', ['rm', '--force', container]); run('docker', agentArgs) }
    await waitFor(connectedAgent)
    await waitFor(async () => { const value = await api(`/agents/${agentID}/software`); return value.ready && value.current.version === manifest.version ? value : null })
    assert.equal((await readStore()).current.id, releaseID, 'program did not survive recreation')
    assert.deepEqual(await readConfig(), before); assert.equal(await readSave(), 'preserve saved worlds')
    console.log('Agent update smoke passed: authenticated Controller/Agent channel, program apply, reconnection, stable launcher and simulated room, persisted version after recreation, preserved configuration and save bytes.')
  }
} catch (error) {
  if (container) { try { log += run('docker', ['logs', container]) } catch { /* Preserve original error. */ } }
  console.error(log.replaceAll(key, '<redacted>')); throw error
} finally {
  if (room?.exitCode === null) room.kill('SIGTERM')
  if (container) { try { run('docker', ['rm', '--force', container]) } catch { /* Already removed. */ } }
  await stop(agent); await stop(controller)
  if (options.image && dataRoot) run('docker', ['run', '--rm', '--user', '0', '--entrypoint', '/bin/chown', '--mount', `type=bind,src=${dataRoot},dst=/fixture`, options.image, '-R', `${process.getuid()}:${process.getgid()}`, '/fixture'])
  await rm(stage, { recursive: true, force: true })
}

#!/usr/bin/env node
// Isolated integration test: never accepts an operator's data directory.
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { createHash } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, realpath, rm, stat, writeFile } from 'node:fs/promises'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const options = {}
for (let index = 2; index < process.argv.length; index += 2) {
  const name = process.argv[index]
  assert(['--binary', '--bundle', '--image', '--kind', '--read-only'].includes(name) && process.argv[index + 1], 'invalid smoke test option')
  options[name.slice(2)] = process.argv[index + 1]
}
assert(Boolean(options.binary) !== Boolean(options.image), 'choose --binary or --image')
assert(options.bundle, '--bundle is required')
const run = (cmd, args, extra = {}) => execFileSync(cmd, args, { encoding: 'utf8', timeout: 60000, ...extra }).trim()
const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
const stage = await realpath(await mkdtemp(path.join(os.tmpdir(), 'dst-online-update-')))
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('DST_ADMIN_')))
let child, container, root, baseURL, metadata, log = '', cookie, csrf, room, roomPID
let exited = false
const deadline = () => Date.now() + 90000

try {
  if (options.binary) {
    metadata = JSON.parse(run(path.resolve(options.binary), ['-version'], { env: cleanEnv }))
  } else {
    assert(['all-in-one', 'control-plane'].includes(options.kind), 'invalid image kind')
    metadata = JSON.parse(run('docker', ['run', '--rm', '--entrypoint', '/usr/local/bin/dst-admin', options.image, '-version']))
  }
  if (!/^v\d+\.\d+\.\d+$/.test(metadata.version)) {
    console.log('Online update lifecycle test requires a stable release build; package validation still applies.')
    process.exitCode = 0
  } else {
    const bundle = path.resolve(options.bundle)
    const extracted = path.join(stage, 'bundle'); await mkdir(extracted)
    run('tar', ['-xzf', bundle, '-C', extracted])
    const manifest = JSON.parse(await readFile(path.join(extracted, 'manifest.json')))
    assert.equal(manifest.version, metadata.version)
    if (options.binary) {
      const template = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../systemd/local.conf.example')
      const config = (await readFile(template, 'utf8')).replaceAll('/var/lib/dst-admin', path.join(stage, 'control')).replaceAll('/opt/dst', path.join(stage, 'data'))
      for (const directory of ['control', 'data/server', 'data/saves', 'data/backups', 'data/maps', 'data/workshop/steamapps/workshop/content/322330']) await mkdir(path.join(stage, directory), { recursive: true })
      await writeFile(path.join(stage, 'app.conf'), config, { mode: 0o600 })
      root = path.join(stage, 'software-updates')
      const listener = net.createServer(); await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve))
      const address = `127.0.0.1:${listener.address().port}`; await new Promise(resolve => listener.close(resolve))
      baseURL = `http://${address}`
      const start = () => {
        exited = false
        child = spawn(path.resolve(options.binary), ['-addr', address], { cwd: stage, env: { ...cleanEnv, GIN_MODE: 'release', DST_ADMIN_CONFIG: path.join(stage, 'app.conf'), DST_ADMIN_UPDATE_DIR: root }, stdio: ['ignore', 'pipe', 'pipe'] })
        child.on('exit', () => { exited = true }); child.on('error', error => { exited = true; log += error })
        for (const stream of [child.stdout, child.stderr]) stream.on('data', data => { log = (log + data).slice(-100000) })
      }
      options.start = start; start()
      room = spawn('sleep', ['600']); roomPID = room.pid
    } else {
      const allInOne = options.kind === 'all-in-one', mount = allInOne ? '/opt/dst' : '/var/lib/dst-admin', port = allInOne ? '8080' : '8000'
      root = path.join(stage, 'data', allInOne ? 'control/software-updates' : 'software-updates')
      await mkdir(path.join(stage, 'data'), { recursive: true })
      container = `dst-online-update-${process.pid}-${Date.now()}`
      const args = ['run', '--detach', '--platform', 'linux/amd64', '--name', container, '--publish', `127.0.0.1::${port}`, '--mount', `type=bind,src=${path.join(stage, 'data')},dst=${mount}`, '--env', 'DST_ADMIN_BOOTSTRAP_DST=false', '--tmpfs', '/tmp:mode=1777,size=64m', '--tmpfs', '/run:mode=0755,size=16m']
      if (!allInOne) run('chmod', ['777', path.join(stage, 'data')])
      if (options['read-only'] === 'true') args.push('--read-only')
      args.push(options.image); run('docker', args)
      const getURL = () => { baseURL = 'http://' + run('docker', ['port', container, `${port}/tcp`]) }
      options.recreate = () => { run('docker', ['rm', '--force', container]); run('docker', args); getURL() }
      getURL()
      if (allInOne) {
        options.startRoom = () => {
          run('docker', ['exec', '--user', '10000:10000', container, 'tmux', 'new-session', '-d', '-s', 'update-test-room', '/bin/sleep', '600'])
          roomPID = run('docker', ['exec', '--user', '10000:10000', container, 'tmux', 'display-message', '-p', '-t', 'update-test-room', '#{pane_pid}'])
        }
      }
    }
    const fetchJSON = async (resource, input) => {
      const response = await fetch(baseURL + '/api/v2' + resource, { signal: AbortSignal.timeout(5000), headers: { ...(cookie ? { Cookie: cookie } : {}), ...(input ? { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf || '', 'Idempotency-Key': randomBytes(16).toString('hex') } : {}) }, ...(input ? { method: 'POST', body: JSON.stringify(input) } : {}) })
      if (response.headers.get('set-cookie')) cookie = response.headers.get('set-cookie').split(';')[0]
      const envelope = await response.json(); assert(response.ok, `${response.status}: ${JSON.stringify(envelope)}`)
      return { value: envelope.data, response }
    }
    async function waitFor(load) {
      const until = deadline(); let failure
      while (Date.now() < until) {
        assert(!exited, 'management launcher exited')
        try { const value = await load(); if (value) return value } catch (error) { failure = error }
        await delay(250)
      }
      throw failure || Error('update lifecycle timed out')
    }
    await waitFor(async () => (await fetchJSON('/auth/session')).value)
    const auth = await fetchJSON('/auth/setup', { username: 'update-smoke', password: 'UpdateSmoke123!' }); csrf = auth.value.csrfToken
    const initial = await waitFor(async () => { const result = await fetchJSON('/system/software'); return result.value.ready ? result : null }); assert.equal(initial.value.supported, true)
    options.startRoom?.()
    const bootID = initial.response.headers.get('x-dst-admin-boot-id')
    const launcherPID = child?.pid
    const configPath = options.binary ? path.join(stage, 'app.conf') : path.join(stage, 'data', options.kind === 'all-in-one' ? 'control/app.conf' : 'app.conf')
    const config = await readFile(configPath)
    const save = path.join(stage, options.binary ? 'data/saves/existing-save' : 'data/existing-save')
    await writeFile(save, 'preserve save bytes')
    const statePath = path.join(root, 'state.json')
    const store = options.kind === 'all-in-one' ? '/opt/dst/control/software-updates' : '/var/lib/dst-admin/software-updates'
    const uid = options.kind === 'all-in-one' ? '10000:10000' : '10001:10001'
    const readStore = async () => JSON.parse(container ? run('docker',['exec','--user',uid,container,'cat',store+'/state.json']) : await readFile(statePath,'utf8'))
    const state = await readStore()
    const id = randomBytes(16).toString('hex'), release = randomBytes(16).toString('hex')
    state.operation = { id, releaseId: release, version: manifest.version, phase: 'prepared', progress: 100, startedAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
    if (container) {
      // Publish from inside the container, like the actual updater. Host-side
      // large file copies on Docker Desktop can expose stale inode sizes through
      // VirtioFS even after the host copy has completed.
      const archivePath = store+'/.smoke-bundle.tar.gz', releasePath = store+'/releases/'+release
      run('docker',['cp',bundle,container+':'+archivePath])
      run('docker',['exec','--user','0',container,'mkdir','-p',releasePath])
      run('docker',['exec','--user','0',container,'tar','-xzf',archivePath,'-C',releasePath])
      run('docker',['exec','--user','0',container,'chown','-R',uid,releasePath])
      run('docker',['exec','--user','0',container,'rm',archivePath])
      run('docker',['exec','--interactive','--user',uid,container,'sh','-c','cat > "$1.fixture" && mv "$1.fixture" "$1"','fixture',store+'/state.json'],{input:JSON.stringify(state)})
    } else {
      await cp(extracted, path.join(root, 'releases', release), { recursive: true })
      await writeFile(statePath + '.fixture', JSON.stringify(state), { mode: 0o600 })
      // Atomic publication avoids in-flight readers seeing partial JSON.
      run('mv', [statePath + '.fixture', statePath])
    }
    const stagedStatus = (await fetchJSON('/system/software')).value
    assert.equal(stagedStatus.operation.id,id)
    assert.equal(stagedStatus.operation.releaseId,release)
    assert.equal(stagedStatus.platform,manifest.platform)
    const result = await fetchJSON('/system/software/actions/apply', { operationId: id, confirmation: 'APPLY SOFTWARE UPDATE' }); assert.equal(result.value.phase, 'restarting')
    const applied = await waitFor(async () => { const result = await fetchJSON('/system/software'); return result.value.ready && result.value.operation?.phase === 'succeeded' ? result : null })
    assert.notEqual(applied.response.headers.get('x-dst-admin-boot-id'), bootID)
    assert.equal(applied.value.current.frontendCommit, metadata.frontendCommit)
    assert.equal((await readStore()).current.id, release)
    assert.equal(await readFile(save, 'utf8'), 'preserve save bytes')
    assert.deepEqual(await readFile(configPath), config)
    if (child) assert.equal(child.pid, launcherPID, 'launcher PID changed during management restart')
    if (room) { assert.equal(room.pid, roomPID); assert.equal(room.exitCode, null) }
    if (options.image && roomPID) assert.equal(run('docker', ['exec', '--user', '10000:10000', container, 'tmux', 'display-message', '-p', '-t', 'update-test-room', '#{pane_pid}']), roomPID, 'room process changed')
    // A full container/launcher recreation must select persisted updated code.
    if (child) {
      await new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(Error('launcher shutdown timed out')), 30000)
        child.once('exit', () => { clearTimeout(timeout); resolve() }); child.kill('SIGTERM')
      })
      options.start()
    }
    else options.recreate()
    const restored = await waitFor(async () => { const result = await fetchJSON('/system/software'); return result.value.ready && result.value.operation?.phase === 'succeeded' ? result.value : null })
    assert.equal(restored.current.version, metadata.version)
    assert.equal((await readStore()).current.id, release)
    assert.equal(await readFile(save, 'utf8'), 'preserve save bytes')
    // Reject a corrupted trial after admission, then restore the exact committed
    // program. Pausing only the isolated launcher makes preflight deterministic.
    if (container) options.startRoom?.()
    const failedID = randomBytes(16).toString('hex'), failedRelease = randomBytes(16).toString('hex')
    const failedState = await readStore()
    failedState.operation = { id: failedID, releaseId: failedRelease, version: manifest.version, phase: 'prepared', progress: 100, startedAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
    if (container) {
      run('docker', ['exec', '--user', uid, container, 'cp', '-a', store + '/releases/' + release, store + '/releases/' + failedRelease])
      run('docker', ['exec', '--interactive', '--user', uid, container, 'sh', '-c', 'cat > "$1.fixture" && mv "$1.fixture" "$1"', 'fixture', store + '/state.json'], { input: JSON.stringify(failedState) })
    } else {
      await cp(extracted, path.join(root, 'releases', failedRelease), { recursive: true })
      await writeFile(statePath + '.fixture', JSON.stringify(failedState), { mode: 0o600 }); run('mv', [statePath + '.fixture', statePath])
    }
    const marker = JSON.parse(container ? run('docker', ['exec', '--user', uid, container, 'cat', store + '/ready.json']) : await readFile(path.join(root, 'ready.json'), 'utf8'))
    const signalLauncher = signal => container ? run('docker', ['exec', '--user', uid, container, 'sh', '-c', 'kill -"$1" "$2"', 'signal', signal, String(marker.parentPid)]) : child.kill('SIG' + signal)
    signalLauncher('STOP')
    try {
      await fetchJSON('/system/software/actions/apply', { operationId: failedID, confirmation: 'APPLY SOFTWARE UPDATE' })
      if (container) run('docker', ['exec', '--user', uid, container, 'sh', '-c', 'printf broken > "$1"', 'fixture', store + '/releases/' + failedRelease + '/dst-admin'])
      else await writeFile(path.join(root, 'releases', failedRelease, 'dst-admin'), 'broken')
    } finally { signalLauncher('CONT') }
    await waitFor(async () => { const result = await fetchJSON('/system/software'); return result.value.ready && result.value.operation?.phase === 'rolled_back' ? result.value : null })
    assert.equal((await readStore()).current.id, release, 'failed update did not restore the committed program')
    assert.equal(await readFile(save, 'utf8'), 'preserve save bytes')
    assert.deepEqual(await readFile(configPath), config)
    console.log('Online update smoke passed: authenticated apply, embedded UI, stable launcher/room PID, persisted program after recreation, automatic recovery, preserved settings and save sentinel.')
  }
} catch (error) {
  if (root) {
    try {
      const state = JSON.parse(await readFile(path.join(root,'state.json')))
      console.error('Isolated update state:',JSON.stringify(state))
      if (state.operation?.releaseId) {
        const directory = path.join(root,'releases',state.operation.releaseId)
        console.error('Isolated update manifest:',await readFile(path.join(directory,'manifest.json'),'utf8'))
        for (const name of ['dst-admin','dst-map-renderer','mod-local-setup']) {
          const file=path.join(directory,name), info=await stat(file)
          console.error(name,JSON.stringify({size:info.size,mode:info.mode.toString(8),sha256:createHash('sha256').update(await readFile(file)).digest('hex')}))
        }
        if (container) {
          const store=options.kind==='all-in-one'?'/opt/dst/control/software-updates':'/var/lib/dst-admin/software-updates'
          console.error('Container filesystem types:',run('docker',['exec','--user','0',container,'stat','-c','%n %F %s %a',store,store+'/state.json',store+'/releases/'+state.operation.releaseId,...['dst-admin','manifest.json'].map(name=>store+'/releases/'+state.operation.releaseId+'/'+name)]))
        }
      }
    } catch (diagnosticError) { console.error('Cannot read isolated update diagnostics:',diagnosticError.message) }
  }
  if (container) { try { log = run('docker', ['logs', '--tail', '80', container]) } catch {} }
  console.error(log); throw error
} finally {
  if (child && !exited) {
    child.kill('SIGTERM'); const until = Date.now()+30000
    while (!exited && Date.now()<until) await delay(100)
    if (!exited) child.kill('SIGKILL')
  }
  if (room) room.kill('SIGTERM')
  if (container) { try { run('docker', ['rm', '--force', container]) } catch {} }
  await rm(stage, { recursive: true, force: true })
}

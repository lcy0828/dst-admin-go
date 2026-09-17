import { execFileSync } from 'node:child_process'
import { cp, lstat, mkdir, mkdtemp, readFile, rm } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

export const backendRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
export const frontendRepository = 'https://github.com/lcy0828/dst-admin-vue.git'
const sourcePaths = ['go.mod', 'go.sum', 'main.go', 'agent', 'cmd', 'controller', 'cron', 'internal', 'models', 'pkg', 'routers', 'server', 'service', 'shared', 'template', 'tmux', 'deploy']

export const run = (command, args, cwd = backendRoot) => execFileSync(command, args, { cwd, encoding: 'utf8' }).trim()
export const revision = root => ({
  commit: run('git', ['rev-parse', 'HEAD'], root),
  dirty: run('git', ['status', '--porcelain'], root) !== ''
})

// Copy only build inputs. Working configuration, databases, saves, internal
// notes, credentials and unrelated downloads never enter a Docker context.
export async function copyBackend(destination) {
  const files = run('git', ['ls-files', '--cached', '--others', '--exclude-standard', '-z', '--', ...sourcePaths]).split('\0').filter(Boolean)
  for (const name of new Set(files)) {
    if (name.startsWith('agent/conf/') || name.startsWith('internal/webui/dist/') || name.endsWith('/.env')) continue
    const source = path.join(backendRoot, name)
    let info
    try { info = await lstat(source) } catch (error) { if (error.code === 'ENOENT') continue; throw error }
    if (!info.isFile()) throw new Error(`build input must be a regular file: ${name}`)
    const target = path.join(destination, name)
    await mkdir(path.dirname(target), { recursive: true })
    await cp(source, target)
  }
}

export async function withSources(options, action) {
  const stage = await mkdtemp(path.join(os.tmpdir(), 'dst-release-'))
  try {
    const backend = path.join(stage, 'backend')
    await mkdir(backend)
    await copyBackend(backend)
    const backendSource = revision(backendRoot)
    let frontend, frontendSource
    if (options.web !== false) {
      let checkout = options.frontend && path.resolve(options.frontend)
      const repository = options['frontend-repository'] || frontendRepository
      const ref = options['frontend-ref'] || 'master'
      if (!checkout) {
        checkout = path.join(stage, 'frontend-checkout')
        await mkdir(checkout)
        run('git', ['init', '--quiet'], checkout)
        run('git', ['remote', 'add', 'origin', repository], checkout)
        // Resolve once. All binaries and images in this invocation use this SHA.
        execFileSync('git', ['fetch', '--depth=1', 'origin', ref], { cwd: checkout, stdio: 'inherit', env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } })
        run('git', ['checkout', '--quiet', '--detach', 'FETCH_HEAD'], checkout)
      }
      frontendSource = { repository, ref, commit: run('git', ['rev-parse', 'HEAD'], checkout) }
      frontend = path.join(stage, 'frontend')
      await mkdir(frontend)
      const inputs = run('git', ['ls-tree', '--name-only', 'HEAD'], checkout).split('\n').filter(name =>
        !['docs', 'design-system', '.github', 'AGENTS.md', 'README.md', 'README.en.md'].includes(name) && !name.startsWith('.env'))
      const archive = execFileSync('git', ['archive', '--format=tar', frontendSource.commit, '--', ...inputs], { cwd: checkout, maxBuffer: 128 * 1024 * 1024 })
      execFileSync('tar', ['-xf', '-', '-C', frontend], { input: archive })
      // Docker receives stage as its context. Remove the temporary clone so
      // Git metadata, checkout credentials and excluded docs cannot be sent.
      if (!options.frontend) await rm(checkout, { recursive: true, force: true })
      await readFile(path.join(frontend, 'package-lock.json'))
      console.log(`Frontend: ${frontendSource.commit} (${ref})`)
    }
    return await action({ stage, backend, frontend, backendSource, frontendSource })
  } finally {
    await rm(stage, { recursive: true, force: true })
  }
}

export async function buildFrontend({ backend, frontend }) {
  execFileSync('npm', ['ci'], { cwd: frontend, stdio: 'inherit' })
  execFileSync('npm', ['run', 'build'], { cwd: frontend, stdio: 'inherit' })
  await readFile(path.join(frontend, 'dist', 'index.html'))
  await cp(path.join(frontend, 'dist'), path.join(backend, 'internal/webui/dist'), { recursive: true })
}

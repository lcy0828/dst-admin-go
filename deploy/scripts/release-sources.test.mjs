import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { buildFrontend } from './release-sources.mjs'

test('frontend packaging rejects stale embedded assets instead of merging builds', async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'dst-webui-build-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const frontend = path.join(root, 'frontend')
  const backend = path.join(root, 'backend')
  const embedded = path.join(backend, 'internal/webui/dist')
  await mkdir(path.join(frontend, 'dist'), { recursive: true })
  await writeFile(path.join(frontend, 'dist/index.html'), 'new frontend')
  await mkdir(embedded, { recursive: true })
  await writeFile(path.join(embedded, 'stale.js'), 'old frontend')

  await assert.rejects(
    buildFrontend({ backend, frontend, frontendPrebuilt: true }),
    /embedded frontend assets already exist/
  )
  assert.equal(await readFile(path.join(embedded, 'stale.js'), 'utf8'), 'old frontend')

  await rm(embedded, { recursive: true })
  await buildFrontend({ backend, frontend, frontendPrebuilt: true })
  assert.equal(await readFile(path.join(embedded, 'index.html'), 'utf8'), 'new frontend')
})

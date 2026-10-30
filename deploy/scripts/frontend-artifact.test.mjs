import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, symlink, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { copyFrontendArtifact, writeFrontendArtifact } from './frontend-artifact.mjs'

const commit = 'a'.repeat(40)
async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'frontend-artifact-test-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const dist = path.join(root, 'original')
  await mkdir(path.join(dist, 'assets'), { recursive: true })
  await writeFile(path.join(dist, 'index.html'), '<script src="/assets/app.js"></script>')
  await writeFile(path.join(dist, 'assets/app.js'), 'console.log("fixture")')
  const artifact = path.join(root, 'artifact')
  await writeFrontendArtifact(dist, artifact, commit)
  return { root, artifact, destination: path.join(root, 'copied') }
}

test('the same verified frontend bytes can be reused on another platform', async t => {
  const { artifact, destination } = await fixture(t)
  await copyFrontendArtifact(artifact, destination, commit)
  assert.equal(await readFile(path.join(destination, 'assets/app.js'), 'utf8'), 'console.log("fixture")')
})

test('rejects a frontend from another commit', async t => {
  const { artifact, destination } = await fixture(t)
  await assert.rejects(copyFrontendArtifact(artifact, destination, 'b'.repeat(40)), /commit mismatch/)
})

for (const change of ['changed', 'added', 'removed', 'symlink']) {
  test(`rejects ${change} frontend assets before copying`, async t => {
    const { root, artifact, destination } = await fixture(t)
    const script = path.join(artifact, 'dist/assets/app.js')
    if (change === 'changed') await writeFile(script, 'changed')
    if (change === 'added') await writeFile(path.join(artifact, 'dist/extra.js'), 'added')
    if (change === 'removed') await rm(script)
    if (change === 'symlink') { await rm(script); await symlink(path.join(root, 'original/assets/app.js'), script) }
    await assert.rejects(copyFrontendArtifact(artifact, destination, commit), /integrity|symlinks/)
    await assert.rejects(readFile(path.join(destination, 'index.html')), { code: 'ENOENT' })
  })
}

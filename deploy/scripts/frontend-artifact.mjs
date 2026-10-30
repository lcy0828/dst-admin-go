import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { cp, mkdir, readFile, readdir, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

async function inventory(root, relative = '', files = {}) {
  for (const entry of (await readdir(path.join(root, relative), { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name, 'en'))) {
    const name = path.posix.join(relative, entry.name)
    assert(!entry.isSymbolicLink(), 'frontend artifact must not contain symlinks')
    if (entry.isDirectory()) await inventory(root, name, files)
    else {
      assert(entry.isFile(), 'frontend artifact must contain regular files')
      assert(Object.keys(files).length < 10000, 'frontend artifact has too many files')
      const data = await readFile(path.join(root, name))
      files[name] = { size: data.length, sha256: createHash('sha256').update(data).digest('hex') }
    }
  }
  return files
}

export async function writeFrontendArtifact(dist, output, commit) {
  assert(/^[a-f0-9]{40}$/.test(commit), 'frontend commit must be an exact SHA')
  const files = await inventory(dist)
  assert(files['index.html'], 'frontend index.html is missing')
  await mkdir(output, { recursive: true })
  await cp(dist, path.join(output, 'dist'), { recursive: true, errorOnExist: true, force: false })
  await writeFile(path.join(output, 'manifest.json'), JSON.stringify({ protocol: 1, commit, files }) + '\n', { flag: 'wx' })
}

export async function copyFrontendArtifact(artifact, destination, expectedCommit) {
  const manifest = JSON.parse(await readFile(path.join(artifact, 'manifest.json'), 'utf8'))
  assert.equal(manifest.protocol, 1, 'unsupported frontend artifact')
  assert.match(expectedCommit, /^[a-f0-9]{40}$/)
  assert.equal(manifest.commit, expectedCommit, 'frontend artifact commit mismatch')
  const files = await inventory(path.join(artifact, 'dist'))
  assert(files['index.html'], 'frontend index.html is missing')
  assert.deepEqual(files, manifest.files, 'frontend artifact integrity check failed')
  await cp(path.join(artifact, 'dist'), destination, { recursive: true, errorOnExist: true, force: false })
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const [dist, output, commit, ...extra] = process.argv.slice(2)
  assert(dist && output && commit && !extra.length, 'usage: frontend-artifact.mjs DIST OUTPUT FRONTEND_SHA')
  await writeFrontendArtifact(dist, output, commit)
}

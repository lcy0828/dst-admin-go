#!/usr/bin/env node
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readdir, readFile, stat, writeFile } from 'node:fs/promises'
import path from 'node:path'
const [root, version, notesFile] = process.argv.slice(2)
assert(root && /^v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/.test(version || '') && notesFile, 'usage: write-release-index.mjs RELEASE_DIR VERSION NOTES_FILE')
const repository = 'lcy0828/dst-admin-go', assets = []
for (const name of (await readdir(root)).sort()) {
  if (!/^dst-admin-(?:update-(linux-amd64|darwin-arm64)|agent-update-(linux-amd64|linux-arm64|darwin-arm64|darwin-amd64|windows-amd64))\.tar\.gz(?:\.sha256)?$/.test(name)) continue
  const file = path.join(root, name)
  assets.push({ name, size: (await stat(file)).size, digest: 'sha256:' + createHash('sha256').update(await readFile(file)).digest('hex'), browser_download_url: `https://github.com/${repository}/releases/download/${version}/${name}` })
}
assert.equal(assets.length, 14, 'Management and all five Agent platform bundles and checksums are required')
await writeFile(path.join(root, 'dst-admin-release.json'), JSON.stringify({ tag_name: version, name: version,
  body: await readFile(notesFile, 'utf8'), html_url: `https://github.com/${repository}/releases/tag/${version}`,
  published_at: new Date().toISOString(), draft: false, prerelease: version.includes('-'), assets
}, null, 2) + '\n')

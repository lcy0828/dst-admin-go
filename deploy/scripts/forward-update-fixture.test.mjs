import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import { nextFixtureVersion } from './forward-update-fixture.mjs'

test('lifecycle fixtures always advance a stable protocol version', () => {
  assert.equal(nextFixtureVersion('v1.3.16'), 'v1.3.17')
  assert.equal(nextFixtureVersion('v1.9.4294967295'), 'v1.10.0')
  assert.equal(nextFixtureVersion('preview-abc'), null)
  assert.equal(nextFixtureVersion('v01.2.3'), null)
  assert.throws(() => nextFixtureVersion('v1.0.4294967296'))
  assert.throws(() => nextFixtureVersion('v4294967295.4294967295.4294967295'))
})

test('synthetic forward bundles are isolated from public release assets', async () => {
  const native = await readFile(new URL('../../.github/workflows/native-package.yml', import.meta.url), 'utf8')
  const publish = await readFile(new URL('../../.github/workflows/package.yml', import.meta.url), 'utf8')
  assert.match(native, /name: online-update-fixtures-/)
  assert.match(native, /path: \.codex-build\/update-fixtures\/\*\.tar\.gz/)
  assert.match(native, /--bundle "\.codex-build\/update-fixtures\/dst-admin-update-/)
  assert.match(native, /--bundle "\.codex-build\/update-fixtures\/dst-admin-agent-update-/)
  assert.match(publish, /pattern: dst-admin-\*/)
  assert.match(publish, /release\/\*\.tar\.gz/)
})

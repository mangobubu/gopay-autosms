import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const source = await readFile(
  new URL('./pages/HeroSmsPurchasePage.vue', import.meta.url),
  'utf8',
)

test('duration selection reloads the exact catalog while preserving existing request guards', () => {
  assert.match(
    source,
    /async function onDurationChange\(\): Promise<void> \{\s*await loadCatalog\(\{\s*service: form\.service,\s*country: form\.country,\s*verificationType: form\.verificationType,\s*durationHours: form\.durationHours,\s*\}, true\)\s*\}/,
  )
  assert.match(
    source,
    /<el-select v-model="form\.durationHours"[^>]*@change="onDurationChange"/,
  )
  assert.match(source, /const version = \+\+catalogVersion/)
  assert.match(source, /if \(disposed \|\| version !== catalogVersion\) return/)
})

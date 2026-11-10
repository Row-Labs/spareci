// Save and restore through the real @actions/cache client, exactly as
// actions/cache, setup-node, setup-go etc. do inside a job. The Go test sets
// ACTIONS_RESULTS_URL to a spareci listener and runs this in a temp dir.
//
// Covers both upload paths of the Azure Blob SDK: a single PUT for small
// archives and staged blocks + block list above 128 MB.
import * as cache from '@actions/cache'
import { createHash, randomBytes } from 'node:crypto'
import { readFileSync, rmSync, writeFileSync } from 'node:fs'

const run = randomBytes(4).toString('hex')
const sha = (f) => createHash('sha256').update(readFileSync(f)).digest('hex')
const fail = (msg) => {
  console.error(`FAIL: ${msg}`)
  process.exit(1)
}

const cases = [
  { name: 'small', bytes: 64 * 1024 },
  { name: 'large', bytes: 150 * 1024 * 1024 }, // incompressible, so > 128 MB archive: block upload
]

for (const c of cases) {
  writeFileSync(c.name, randomBytes(c.bytes))
  c.sum = sha(c.name)
  c.key = `compat-${c.name}-${run}`
  const id = await cache.saveCache([c.name], c.key)
  if (!(id > 0)) fail(`save ${c.name}: got cache id ${id}`)
  console.log(`saved ${c.name} as entry ${id}`)
}

if ((await cache.saveCache(['small'], cases[0].key)) !== -1) fail('saving an existing key should be refused (-1)')

for (const c of cases) rmSync(c.name)

// Restore small through a restore-key prefix, large by exact key.
const small = await cache.restoreCache(['small'], `compat-small-${run}-newer`, [`compat-small-${run}`])
if (small !== cases[0].key) fail(`restore small via restore key: matched ${small}`)
const large = await cache.restoreCache(['large'], cases[1].key)
if (large !== cases[1].key) fail(`restore large: matched ${large}`)
for (const c of cases) if (sha(c.name) !== c.sum) fail(`${c.name} restored with different content`)

if ((await cache.restoreCache(['small'], `absent-${run}`)) !== undefined) fail('miss should return undefined')

console.log('PASS')

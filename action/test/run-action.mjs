// Runs the action's main or post step as a child process, speaking the
// runner's file protocol, so a test can watch a post step fail without that
// failure turning its own job red.
//
//   node run-action.mjs main <state-file> [input=value ...]
//   node run-action.mjs post <state-file> [input=value ...]
//
// main records the action's saved state in <state-file>; post hands it back
// as STATE_* variables, as the runner does. The exit code is the step's.
// Unlike the runner, it does not apply action.yml's defaults: pass every
// boolean input.
import { spawnSync } from 'node:child_process'
import * as fs from 'node:fs'
import * as path from 'node:path'

const [phase, stateFile, ...inputs] = process.argv.slice(2)
const env = { ...process.env }
for (const kv of inputs) {
  const i = kv.indexOf('=')
  env[`INPUT_${kv.slice(0, i).toUpperCase()}`] = kv.slice(i + 1)
}
for (const k of Object.keys(env)) if (k.startsWith('STATE_')) delete env[k]

if (phase === 'main') {
  // @actions/core appends to these and requires them to exist.
  env.GITHUB_STATE = stateFile
  env.GITHUB_ENV = `${stateFile}.env`
  env.GITHUB_OUTPUT = `${stateFile}.output`
  for (const f of [env.GITHUB_STATE, env.GITHUB_ENV, env.GITHUB_OUTPUT]) fs.writeFileSync(f, '')
} else if (phase === 'post') {
  // Entries are `name<<delimiter`, the value's lines, then `delimiter`.
  const lines = fs.readFileSync(stateFile, 'utf8').split('\n')
  for (let i = 0; i < lines.length; i++) {
    const sep = lines[i].indexOf('<<')
    if (sep < 0) continue
    const name = lines[i].slice(0, sep)
    const delimiter = lines[i].slice(sep + 2)
    const value = []
    while (++i < lines.length && lines[i] !== delimiter) value.push(lines[i])
    env[`STATE_${name}`] = value.join('\n')
  }
} else {
  console.error(`unknown phase: ${phase}`)
  process.exit(2)
}

const dist = path.join(import.meta.dirname, '..', 'dist', 'index.cjs')
const r = spawnSync(process.execPath, [dist], { env, stdio: 'inherit' })
process.exit(r.status ?? 1)

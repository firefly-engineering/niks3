// niks3 GitHub Action: configures a Nix substituter and pushes built store
// paths to a niks3 binary cache via the niks3-hook upload daemon.
//
// Forked from Mic92/niks3-action v1.1.0 (MIT, see LICENSE). This copy lives
// in the niks3 repo and is released with its tags, so the client it
// downloads is always the release it was tagged with.
//
// All GitHub-specific orchestration lives here. niks3 itself only provides
// the upload daemon (niks3-hook serve), the post-build-hook client
// (niks3-hook send), the one-shot push CLI (niks3 push), and the server's
// /api/cache-config endpoint.

import * as core from '@actions/core'
import * as tc from '@actions/tool-cache'
import { execFileSync, spawn, spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import * as fs from 'node:fs'
import * as os from 'node:os'
import * as path from 'node:path'

// Baked at bundle time via esbuild --define from action/NIKS3_VERSION. The
// release workflow refuses a tag that differs from it, so
// `firefly-engineering/niks3/action@<tag>` downloads the client from the
// <tag> release of the same repo.
declare const NIKS3_VERSION: string

const RELEASE_REPO = 'firefly-engineering/niks3'

// GitHub Actions' OIDC issuer — a well-known constant.
const GITHUB_ISSUER = 'https://token.actions.githubusercontent.com'

const isPost = !!core.getState('isPost')

async function main(): Promise<void> {
  if (isPost) {
    await post()
  } else {
    core.saveState('isPost', 'true')
    await setup()
  }
}

// ---------------------------------------------------------------------------
// Main step
// ---------------------------------------------------------------------------

interface CacheConfig {
  substituter_url: string
  public_keys: string[]
  oidc_audience: string
}

interface ResolvedConfig {
  serverURL: string
  substituter: string
  publicKeys: string[]
  audience: string
  netrc: string
  skipPush: boolean
  debug: boolean
}

async function setup(): Promise<void> {
  const binDir = await resolveBinDir()
  const workDir = path.join(process.env.RUNNER_TEMP ?? os.tmpdir(), 'niks3')
  fs.mkdirSync(workDir, { recursive: true })

  const cfg = await resolveConfig()

  writeNixConf(workDir, cfg)
  if (cfg.netrc) await configureNetrc(workDir, cfg)

  const mode = await pickMode(cfg)
  core.info(`Push mode: ${mode}`)

  if (mode !== 'none') await preflightWrite(workDir, cfg)

  switch (mode) {
    case 'daemon':
      await startDaemon(binDir, workDir, cfg)
      break
    case 'storescan':
      writeStoreSnapshot(path.join(workDir, 'store-pre'))
      break
    case 'none':
      break
  }

  core.saveState('mode', mode)
  core.saveState('workDir', workDir)
  core.saveState('binDir', binDir)
  core.saveState('serverURL', cfg.serverURL)
  core.saveState('audience', cfg.audience)
  core.saveState('debug', String(cfg.debug))
}

async function resolveConfig(): Promise<ResolvedConfig> {
  const serverURL = core.getInput('server-url', { required: true })

  const fetched = await fetchCacheConfig(serverURL)

  // The substituter override exists to point pulls at a CDN/mirror in front
  // of the cache. Public keys and OIDC audience are tied to the server and
  // have no sensible override.
  const substituter = core.getInput('substituter') || fetched?.substituter_url || ''

  return {
    serverURL,
    substituter,
    publicKeys: fetched?.public_keys ?? [],
    audience: fetched?.oidc_audience ?? '',
    netrc: resolveNetrc(substituter),
    skipPush: core.getBooleanInput('skip-push'),
    debug: core.getBooleanInput('debug'),
  }
}

async function fetchCacheConfig(serverURL: string): Promise<CacheConfig | null> {
  const u = new URL('/api/cache-config', serverURL)

  if (process.env.FORGEJO_SERVER_URL) {
    // Forgejo's OIDC issuer URL
    // https://forgejo.org/docs/next/user/actions/security-openid-connect/#standard-claims
    u.searchParams.set('issuer', `${process.env.FORGEJO_SERVER_URL}/api/actions`)
  } else {
    u.searchParams.set('issuer', GITHUB_ISSUER)
  }

  // Configurable so slow-to-start servers have time to boot.
  const timeoutMs = positiveIntInput('cache-config-timeout', 15) * 1000
  const retries = positiveIntInput('cache-config-retries', 3)

  let lastErr: unknown
  let retryAfterMs: number | null = null
  for (let attempt = 0; attempt <= retries; attempt++) {
    if (attempt > 0) {
      const delay = retryAfterMs ?? Math.min(1000 * 2 ** (attempt - 1), 30000)
      core.info(
        `retrying cache-config in ${delay / 1000}s (attempt ${attempt + 1}/${retries + 1})`,
      )
      await new Promise((r) => setTimeout(r, delay))
    }
    retryAfterMs = null
    try {
      const res = await fetch(u, { signal: AbortSignal.timeout(timeoutMs) })
      if (res.ok) return (await res.json()) as CacheConfig
      lastErr = new Error(`server returned ${res.status}`)
      // Client errors (except 429) won't resolve on retry.
      if (res.status < 500 && res.status !== 429) break
      retryAfterMs = parseRetryAfter(res.headers.get('retry-after'))
    } catch (err) {
      lastErr = err
    }
  }
  core.warning(`could not fetch cache-config from server: ${lastErr}`)
  return null
}

function parseRetryAfter(value: string | null): number | null {
  if (!value) return null
  const secs = Number(value)
  if (Number.isFinite(secs) && secs >= 0) return Math.min(secs * 1000, 60000)
  const date = Date.parse(value)
  if (Number.isNaN(date)) return null
  return Math.min(Math.max(date - Date.now(), 0), 60000)
}

function positiveIntInput(name: string, fallback: number): number {
  const raw = core.getInput(name)
  if (raw === '') return fallback
  const n = Number.parseInt(raw, 10)
  if (!Number.isFinite(n) || n < 0) {
    throw new Error(`input '${name}' must be a non-negative integer, got '${raw}'`)
  }
  return n
}

// writeNixConf drops a nix.conf snippet and registers it via
// NIX_USER_CONF_FILES so subsequent `nix` invocations pick it up without a
// daemon restart. The post-build-hook line is added by startDaemon once
// daemon mode is confirmed.
function writeNixConf(workDir: string, cfg: ResolvedConfig): void {
  let body = ''
  if (cfg.substituter) body += `extra-substituters = ${cfg.substituter}\n`
  if (cfg.publicKeys.length > 0)
    body += `extra-trusted-public-keys = ${cfg.publicKeys.join(' ')}\n`

  const confPath = path.join(workDir, 'nix.conf')
  // World-readable: the nix daemon reads it as a different user.
  fs.writeFileSync(confPath, body, { mode: 0o644 })

  // Prepend to the existing search path so we don't mask ~/.config/nix/nix.conf.
  const existing = process.env.NIX_USER_CONF_FILES ?? defaultUserConfFiles()
  core.exportVariable('NIX_USER_CONF_FILES', `${confPath}:${existing}`)
}

function defaultUserConfFiles(): string {
  const home = process.env.XDG_CONFIG_HOME ?? path.join(os.homedir(), '.config')
  const dirs = (process.env.XDG_CONFIG_DIRS ?? '/etc/xdg').split(':')
  return [home, ...dirs].map((d) => path.join(d, 'nix', 'nix.conf')).join(':')
}

// resolveNetrc turns the credential inputs into netrc entries for the
// substituter's read path: either `netrc` verbatim, or one
// `machine <substituter host> login .. password ..` line built from
// netrc-login and netrc-password. Empty when no credential is given.
function resolveNetrc(substituter: string): string {
  const raw = core.getInput('netrc')
  const login = core.getInput('netrc-login')
  const password = core.getInput('netrc-password')
  if (raw) core.setSecret(raw)
  if (password) core.setSecret(password)

  if (raw && (login || password)) {
    throw new Error("set either 'netrc' or 'netrc-login'/'netrc-password', not both")
  }
  if (raw) return raw.trim() + '\n'
  if (!login && !password) return ''
  if (!login || !password) {
    throw new Error("'netrc-login' and 'netrc-password' must be set together")
  }
  for (const [name, v] of [['netrc-login', login], ['netrc-password', password]]) {
    if (/\s/.test(v)) throw new Error(`input '${name}' must not contain whitespace`)
  }
  if (!substituter) {
    throw new Error('a netrc credential needs a substituter; the server returned none')
  }
  return `machine ${new URL(substituter).hostname} login ${login} password ${password}\n`
}

// configureNetrc makes the daemon send the credential when it substitutes.
//   Determinate: determinate-nixd owns netrc-file and sets it after the
//     nix.custom.conf include, so the entries go in as an additional netrc
//     source in /etc/determinate/config.json and the daemon is restarted.
//     https://docs.determinate.systems/determinate-nix/determinate-nixd/
//   Upstream Nix: netrc-file in our nix.conf snippet. A trusted client
//     forwards it to the daemon; the existing netrc is carried over because
//     the setting replaces it rather than adding to it.
async function configureNetrc(workDir: string, cfg: ResolvedConfig): Promise<void> {
  if (isDeterminate()) {
    configureDeterminateNetrc(cfg.netrc)
    await waitForDaemon()
  } else {
    const file = path.join(workDir, 'netrc')
    fs.writeFileSync(file, existingNetrc() + cfg.netrc, { mode: 0o600 })
    fs.appendFileSync(path.join(workDir, 'nix.conf'), `netrc-file = ${file}\n`)
    core.info(`netrc-file set to ${file}`)
  }
  await checkCredential(cfg)
}

function isDeterminate(): boolean {
  try {
    return execFileSync('nix', ['--version'], { encoding: 'utf8', timeout: 10000 }).includes(
      'Determinate Nix',
    )
  } catch {
    return false
  }
}

const DETERMINATE_CONFIG = '/etc/determinate/config.json'
const DETERMINATE_NETRC = '/etc/determinate/niks3.netrc'

function configureDeterminateNetrc(netrc: string): void {
  sudo(['install', '-d', '-m', '0755', path.dirname(DETERMINATE_CONFIG)])
  // Created 0600 before the secret goes in; tee keeps the mode.
  sudo(['install', '-m', '0600', '/dev/null', DETERMINATE_NETRC])
  sudo(['tee', DETERMINATE_NETRC], netrc)

  let config: Record<string, any> = {}
  if (fs.existsSync(DETERMINATE_CONFIG)) {
    config = JSON.parse(sudo(['cat', DETERMINATE_CONFIG]) || '{}')
  }
  const auth = (config.authentication ??= {})
  const sources: string[] = (auth.additionalNetrcSources ??= [])
  if (!sources.includes(DETERMINATE_NETRC)) sources.push(DETERMINATE_NETRC)
  sudo(['tee', DETERMINATE_CONFIG], JSON.stringify(config, null, 2) + '\n')
  core.info(`Added ${DETERMINATE_NETRC} to authentication.additionalNetrcSources`)

  if (os.platform() === 'darwin') {
    sudo(['launchctl', 'kickstart', '-k', 'system/systems.determinate.nix-daemon'])
  } else {
    sudo(['systemctl', 'restart', 'nix-daemon.service'])
  }
  core.info('Restarted the Determinate Nix daemon')
}

// waitForDaemon blocks until the restarted daemon accepts connections.
async function waitForDaemon(): Promise<void> {
  const deadline = Date.now() + 30000
  for (;;) {
    const r = spawnSync('nix', ['store', 'info', '--extra-experimental-features', 'nix-command'], {
      stdio: 'ignore',
      timeout: 10000,
    })
    if (r.status === 0) return
    if (Date.now() > deadline) throw new Error('nix daemon did not come back within 30s after restart')
    await sleep(500)
  }
}

// existingNetrc returns the contents of the netrc-file Nix is configured
// with, so pointing netrc-file at ours does not drop its entries.
function existingNetrc(): string {
  let file = ''
  try {
    file = execFileSync('nix', ['config', 'show', 'netrc-file'], { encoding: 'utf8', timeout: 10000 }).trim()
  } catch {
    return ''
  }
  if (!file || !fs.existsSync(file)) return ''
  let body: string
  try {
    body = fs.readFileSync(file, 'utf8')
  } catch {
    body = sudo(['cat', file])
  }
  return body.endsWith('\n') || body === '' ? body : body + '\n'
}

// checkCredential fetches nix-cache-info with the credential so a rejected
// or mistyped one is reported here rather than as a silently cold cache.
async function checkCredential(cfg: ResolvedConfig): Promise<void> {
  const host = new URL(cfg.substituter).hostname
  const entry = netrcEntry(cfg.netrc, host)
  if (!entry) {
    core.warning(`the netrc credential has no entry for ${host}; reads from the cache stay anonymous`)
    return
  }
  const auth = Buffer.from(`${entry.login}:${entry.password}`).toString('base64')
  try {
    const res = await fetch(new URL('nix-cache-info', cfg.substituter.replace(/\/?$/, '/')), {
      headers: { Authorization: `Basic ${auth}` },
      signal: AbortSignal.timeout(15000),
    })
    if (res.ok) core.info(`Credential for ${host} accepted`)
    else core.warning(`${host} rejected the netrc credential: HTTP ${res.status}`)
  } catch (err) {
    core.warning(`could not check the netrc credential against ${host}: ${err}`)
  }
}

// netrcEntry returns login/password of the `machine <host>` entry (or of
// `default`), walking netrc's whitespace-separated tokens.
interface NetrcEntry {
  login: string
  password: string
}

function netrcEntry(netrc: string, host: string): NetrcEntry | null {
  const tokens = netrc.split(/\s+/).filter(Boolean)
  let current: NetrcEntry | null = null
  let fallback: NetrcEntry | null = null
  let match: NetrcEntry | null = null
  for (let i = 0; i < tokens.length; i++) {
    switch (tokens[i]) {
      case 'machine':
        current = { login: '', password: '' }
        if (tokens[++i] === host) match ??= current
        break
      case 'default':
        current = { login: '', password: '' }
        fallback ??= current
        break
      case 'login':
        if (current) current.login = tokens[++i] ?? ''
        break
      case 'password':
        if (current) current.password = tokens[++i] ?? ''
        break
    }
  }
  return match ?? fallback
}

// sudo runs a command as root without prompting, feeding input on stdin,
// and returns its stdout.
function sudo(args: string[], input?: string): string {
  const cmd = process.getuid?.() === 0 ? args : ['sudo', '-n', ...args]
  const r = spawnSync(cmd[0], cmd.slice(1), {
    input,
    encoding: 'utf8',
    stdio: ['pipe', 'pipe', 'inherit'],
    timeout: 60000,
  })
  if (r.status !== 0) throw new Error(`${cmd.join(' ')} exited ${r.status ?? r.signal}`)
  return r.stdout
}

// pickMode decides daemon vs storescan vs none.
//   none:      skip-push set, OR no OIDC available (fork PR), OR no audience
//   storescan: OIDC available but runner user can't set post-build-hook
//   daemon:    OIDC available and user is trusted (the happy path)
async function pickMode(cfg: ResolvedConfig): Promise<'daemon' | 'storescan' | 'none'> {
  if (cfg.skipPush) {
    core.info('skip-push set; configuring substituter only')
    return 'none'
  }

  if (!process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN) {
    core.info('No OIDC token available (fork PR or missing id-token:write); configuring substituter only')
    return 'none'
  }

  if (!cfg.audience) {
    core.warning(
      `no OIDC audience configured — configure an OIDC provider with issuer ${GITHUB_ISSUER} on the server`,
    )
    return 'none'
  }

  if (!isTrustedUser()) {
    core.warning(
      "runner user is not in Nix trusted-users; falling back to store-scan push (intermediate derivations won't be cached on build failure)",
    )
    return 'storescan'
  }

  return 'daemon'
}

// isTrustedUser reports whether the current user can set post-build-hook.
// A trusted user (or a writable store, i.e. single-user Nix) is required for
// the hook to fire. Mirrors Nix's own check.
function isTrustedUser(): boolean {
  // Single-user install: store is directly writable, no daemon, hooks
  // run unconditionally.
  try {
    fs.accessSync('/nix/store', fs.constants.W_OK)
    return true
  } catch {
    /* multi-user; check trusted-users */
  }

  const username = os.userInfo().username
  let trusted: string[]
  try {
    const out = execFileSync('nix', ['config', 'show'], { encoding: 'utf8', timeout: 10000 })
    const line = out.split('\n').find((l) => l.startsWith('trusted-users = '))
    trusted = line ? line.slice('trusted-users = '.length).trim().split(/\s+/) : []
  } catch {
    return false
  }

  return trusted.includes(username) || trusted.includes('*')
}

// preflightWrite mints this job's OIDC token with the same script the
// uploader uses and asks the server to authorize a write with it, so a token
// no rule accepts fails the job here, before anything is built, instead of
// every upload being rejected in the background. The token's claims are
// logged because the server's 401 names no reason: the mismatch is visible
// only by comparing iss/sub/aud with the server's rules.
async function preflightWrite(workDir: string, cfg: ResolvedConfig): Promise<void> {
  const script = writeTokenScript(workDir, cfg.audience)
  const [cmd, ...args] = script.split(' ')
  const { token } = JSON.parse(execFileSync(cmd, args, { encoding: 'utf8', timeout: 30000 })) as {
    token: string
  }
  const claims = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString()) as Record<
    string,
    unknown
  >
  const who = `iss=${claims.iss} sub=${claims.sub} aud=${claims.aud}`
  core.info(`OIDC token: ${who}`)

  let res: Response
  try {
    res = await fetch(new URL('/api/objects/present', cfg.serverURL), {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ keys: [] }),
      signal: AbortSignal.timeout(15000),
    })
  } catch (err) {
    core.warning(`could not check write access against ${cfg.serverURL}: ${err}`)
    return
  }
  if (res.ok) {
    core.info('Server accepts this token for writes')
    return
  }
  const body = (await res.text()).trim()
  if (res.status === 401 || res.status === 403) {
    throw new Error(
      `${cfg.serverURL} refused this job's OIDC token for writes (HTTP ${res.status}: ${body}). ` +
        `Token: ${who}. The server's OIDC rules must match these claims; ` +
        `set skip-push: true for jobs that should not write.`,
    )
  }
  core.warning(`write access check against ${cfg.serverURL} returned HTTP ${res.status}: ${body}`)
}

// startDaemon writes the post-build-hook shim, the OIDC token script, and
// forks `niks3-hook serve` detached in its own process group so the runner's
// step-end cleanup doesn't take it down early.
async function startDaemon(binDir: string, workDir: string, cfg: ResolvedConfig): Promise<void> {
  const hookBin = path.join(binDir, 'niks3-hook')
  const socket = socketPath(workDir)
  const tokenScript = writeTokenScript(workDir, cfg.audience)

  // post-build-hook runs with a stripped env (only DRV_PATH + OUT_PATHS per
  // `man nix.conf`); the shim bakes in absolute paths resolved now.
  const shim = path.join(workDir, 'post-build-hook')
  fs.writeFileSync(shim, `#!/bin/sh\nexec ${q(hookBin)} send --socket ${q(socket)}\n`, { mode: 0o755 })

  // Append the hook line to the nix.conf we already wrote.
  fs.appendFileSync(path.join(workDir, 'nix.conf'), `post-build-hook = ${shim}\n`)

  const dbPath = path.join(workDir, 'queue.db')
  const logPath = path.join(workDir, 'daemon.log')
  const statsPath = path.join(workDir, 'stats.json')
  const logFD = fs.openSync(logPath, 'w')

  const args = [
    'serve',
    '--socket', socket,
    '--db-path', dbPath,
    '--server-url', cfg.serverURL,
    '--auth-token-script', tokenScript,
    '--idle-exit-timeout', '0',
    '--stats-file', statsPath,
  ]
  if (cfg.debug) args.push('--debug')

  // Resolve the binary up front so a missing file is a synchronous error
  // here, not an async 'error' event after spawn returns.
  fs.accessSync(hookBin, fs.constants.X_OK)

  const child = spawn(hookBin, args, {
    detached: true,
    stdio: ['ignore', logFD, logFD],
  })
  // Spawn-time errors (race after the access check) crash the process if
  // unhandled. Log and let the post step's missing-pid warning explain.
  child.on('error', (e) => core.warning(`niks3-hook serve spawn error: ${e}`))
  child.unref()
  fs.closeSync(logFD)

  if (!child.pid) throw new Error('failed to start niks3-hook serve: no pid')

  // serve binds the socket late (after sqlite + `nix eval`); send swallows
  // connect errors. Block until the socket exists so fast builds aren't lost.
  const deadline = Date.now() + 10000
  while (!fs.existsSync(socket)) {
    try { process.kill(child.pid, 0) } catch { throw new Error('niks3-hook serve exited before binding socket') }
    if (Date.now() > deadline) throw new Error(`niks3-hook serve did not bind ${socket} within 10s`)
    await sleep(50)
  }

  core.info(`niks3-hook serve started (pid ${child.pid}, socket ${socket})`)
  core.saveState('daemonPid', String(child.pid))
  core.saveState('daemonLog', logPath)
  core.saveState('daemonStats', statsPath)
}

// writeTokenScript drops a self-contained node script that prints
// {"token","expires_at"} JSON. niks3-hook serve runs it via
// --auth-token-script; the audience is baked in at write time because the
// daemon runs detached and inherits a stripped env.
function writeTokenScript(workDir: string, audience: string): string {
  const reqURL = process.env.ACTIONS_ID_TOKEN_REQUEST_URL ?? ''
  const reqToken = process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN ?? ''

  const file = path.join(workDir, 'fetch-oidc-token.mjs')
  fs.writeFileSync(
    file,
    `const u = new URL(${JSON.stringify(reqURL)})
u.searchParams.set('audience', ${JSON.stringify(audience)})
const res = await fetch(u, { headers: { Authorization: 'Bearer ' + ${JSON.stringify(reqToken)} } })
if (!res.ok) { process.stderr.write('OIDC endpoint returned ' + res.status + '\\n'); process.exit(1) }
const { value } = await res.json()
const payload = JSON.parse(Buffer.from(value.split('.')[1], 'base64url').toString())
const out = { token: value }
if (payload.exp) out.expires_at = new Date(payload.exp * 1000).toISOString()
process.stdout.write(JSON.stringify(out))
`,
    { mode: 0o600 },
  )

  return `${process.execPath} ${file}`
}

// writeStoreSnapshot lists store paths one-per-line. Used in storescan mode.
function writeStoreSnapshot(file: string): void {
  fs.writeFileSync(file, listStorePaths().join('\n') + '\n')
}

// listStorePaths returns absolute store paths, filtering out non-path entries
// like .links/ and .lock files.
function listStorePaths(): string[] {
  const dir = '/nix/store'
  return fs
    .readdirSync(dir)
    .filter((n) => !n.startsWith('.') && n.length > 32 && n[32] === '-')
    .map((n) => path.join(dir, n))
    .sort()
}

// socketPath: Unix socket paths are limited to 104 (darwin) / 108 (linux)
// bytes including the null terminator. Fall back to os.tmpdir() if too long.
function socketPath(workDir: string): string {
  // sockaddr_un.sun_path is 104 bytes on darwin, 108 on linux — including
  // the null terminator, so the path itself must be shorter by one.
  const limit = (os.platform() === 'darwin' ? 104 : 108) - 1
  const candidate = path.join(workDir, 'daemon.sock')
  return candidate.length <= limit ? candidate : path.join(os.tmpdir(), 'niks3-daemon.sock')
}

// ---------------------------------------------------------------------------
// Post step
// ---------------------------------------------------------------------------

async function post(): Promise<void> {
  const mode = core.getState('mode')

  switch (mode) {
    case 'daemon':
      await stopDaemon()
      break
    case 'storescan':
      pushStoreDiff()
      break
    default:
      // 'none' or empty (setup failed before saving state): nothing to do.
      break
  }
}

// stopDaemon SIGTERMs the niks3-hook daemon, waits for it to drain, and
// fails the job unless everything the hook handed it reached the server.
async function stopDaemon(): Promise<void> {
  const pid = parseInt(core.getState('daemonPid') || '0', 10)
  if (!pid) {
    core.warning('niks3-hook daemon pid not recorded; nothing to stop')
    return
  }

  const timeoutSec = parseInt(core.getInput('drain-timeout') || '600', 10)
  const logPath = core.getState('daemonLog')

  try {
    process.kill(pid, 'SIGTERM')
  } catch {
    // Already dead; its stats file (or lack of one) tells what happened.
  }

  const deadline = Date.now() + timeoutSec * 1000
  let lastBeat = Date.now()
  while (alive(pid)) {
    if (Date.now() > deadline) {
      try {
        process.kill(-pid, 'SIGKILL') // negative pid = process group
      } catch {
        /* already gone */
      }
      dumpLog(logPath)
      throw new Error(`niks3: upload daemon did not drain within ${timeoutSec}s; killed it`)
    }
    // Heartbeat so the runner's no-output watchdog doesn't kill the job.
    if (Date.now() - lastBeat > 30000) {
      core.info('waiting for upload daemon to drain...')
      lastBeat = Date.now()
    }
    await sleep(500)
  }

  reportUploads(core.getState('daemonStats'), logPath)
}

interface DaemonStats {
  received: number
  pushed: number
  failed: number
  remaining: number
  last_error?: string
}

// reportUploads turns the daemon's stats file into the step's outcome: a
// notice when every built path was uploaded, a failure when any is left in
// the queue or none went up, so rejected uploads cannot pass silently.
function reportUploads(statsPath: string, logPath: string): void {
  let stats: DaemonStats
  try {
    stats = JSON.parse(fs.readFileSync(statsPath, 'utf8')) as DaemonStats
  } catch (err) {
    dumpLog(logPath)
    throw new Error(`niks3: upload daemon exited without writing ${statsPath}: ${err}`)
  }

  const summary = `${stats.received} built paths received, ${stats.pushed} uploaded, ${stats.remaining} not uploaded`
  const lastError = stats.last_error ? `; last error: ${stats.last_error.trim()}` : ''
  if (stats.remaining !== 0 || (stats.received > 0 && stats.pushed === 0)) {
    dumpLog(logPath)
    throw new Error(`niks3: uploads failed (${summary}, ${stats.failed} failed attempts${lastError})`)
  }
  if (stats.failed > 0) {
    core.warning(`niks3: ${stats.failed} upload attempts failed before succeeding on retry${lastError}`)
  }
  core.notice(`niks3: ${summary}`)
}

function dumpLog(logPath: string): void {
  let lines: string[]
  try {
    lines = fs.readFileSync(logPath, 'utf8').trimEnd().split('\n')
  } catch {
    return
  }
  core.startGroup(`niks3-hook daemon log (last 200 of ${lines.length} lines)`)
  core.info(lines.slice(-200).join('\n'))
  core.endGroup()
}

// alive reports whether pid is still running. kill(pid, 0) alone is
// insufficient: it succeeds for unreaped zombies, which occur on container
// runners whose PID 1 is not a reaping init (act/Forgejo docker, #18).
function alive(pid: number): boolean {
  try {
    process.kill(pid, 0)
  } catch {
    return false
  }
  if (process.platform === 'linux') {
    try {
      const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8')
      // /proc/pid/stat: "pid (comm) S ..." — comm may contain ')', so the
      // state char is the first char after the last ')'.
      const state = stat.slice(stat.lastIndexOf(')') + 2, stat.lastIndexOf(')') + 3)
      if (state === 'Z') return false
    } catch {
      return false // raced with exit
    }
  }
  return true
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// pushStoreDiff lists store paths added since setup's snapshot and pushes
// them in one shot via `niks3 push`.
function pushStoreDiff(): void {
  const workDir = core.getState('workDir')
  const binDir = core.getState('binDir')
  const before = new Set(
    fs
      .readFileSync(path.join(workDir, 'store-pre'), 'utf8')
      .split('\n')
      .filter(Boolean),
  )
  const added = listStorePaths().filter((p) => !before.has(p))

  if (added.length === 0) {
    core.notice('niks3: no new store paths to push')
    return
  }

  core.startGroup(`niks3: pushing ${added.length} paths`)
  try {
    const tokenScript = writeTokenScript(workDir, core.getState('audience'))
    const args = [
      'push',
      '--server-url', core.getState('serverURL'),
      '--auth-token-script', tokenScript,
    ]
    if (core.getState('debug') === 'true') args.push('--debug')
    args.push(...added)

    const r = spawnSync(path.join(binDir, 'niks3'), args, { stdio: 'inherit' })
    if (r.status !== 0) throw new Error(`niks3: push of ${added.length} paths exited ${r.status}`)
    core.notice(`niks3: pushed ${added.length} paths`)
  } finally {
    core.endGroup()
  }
}

// ---------------------------------------------------------------------------
// Binary resolution
// ---------------------------------------------------------------------------

async function resolveBinDir(): Promise<string> {
  const override = core.getInput('niks3-bin')
  if (override) {
    core.info(`Using niks3 binary from input: ${override}`)
    return path.dirname(override)
  }

  const plat = platformTuple()
  const cached = tc.find('niks3', NIKS3_VERSION, plat)
  if (cached) {
    core.info(`Found cached niks3 ${NIKS3_VERSION} (${plat})`)
    return cached
  }

  const base = `https://github.com/${RELEASE_REPO}/releases/download/${NIKS3_VERSION}`
  const archive = `niks3_${plat}.tar.gz`
  core.info(`Downloading niks3 ${NIKS3_VERSION} from ${base}/${archive}`)

  const tarball = await tc.downloadTool(`${base}/${archive}`)
  const checksums = fs.readFileSync(await tc.downloadTool(`${base}/checksums.txt`), 'utf8')
  verifyChecksum(tarball, archive, checksums)

  const extracted = await tc.extractTar(tarball)
  return tc.cacheDir(extracted, 'niks3', NIKS3_VERSION, plat)
}

// verifyChecksum checks file against its entry in goreleaser's checksums.txt
// ("<sha256>  <name>" per line) and throws on a missing entry or mismatch.
function verifyChecksum(file: string, name: string, checksums: string): void {
  const entry = checksums
    .split('\n')
    .map((l) => l.trim().split(/\s+/))
    .find((fields) => fields.length === 2 && fields[1] === name)
  if (!entry) throw new Error(`checksums.txt of ${NIKS3_VERSION} has no entry for ${name}`)

  const actual = createHash('sha256').update(fs.readFileSync(file)).digest('hex')
  if (actual !== entry[0]) {
    throw new Error(`checksum mismatch for ${name}: expected ${entry[0]}, got ${actual}`)
  }
  core.info(`Verified ${name} against checksums.txt (sha256 ${actual})`)
}

// platformTuple returns the goreleaser archive suffix (e.g. "Linux_x86_64").
function platformTuple(): string {
  const sys: Record<string, string> = { linux: 'Linux', darwin: 'Darwin' }
  const arch: Record<string, string> = { x64: 'x86_64', arm64: 'arm64' }
  const s = sys[os.platform()]
  const a = arch[os.arch()]
  if (!s || !a) throw new Error(`unsupported platform: ${os.platform()}/${os.arch()}`)
  return `${s}_${a}`
}

// q shell-quotes a single argument for the post-build-hook shim.
function q(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`
}

main().catch((err: Error) => {
  core.setFailed(err.message)
})

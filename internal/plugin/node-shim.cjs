// The Bun APIs magpie's host.js uses, on Node: what host.js reads off the
// Bun global (inspect, $, spawn, spawnSync, resolveSync), the child_process
// stdio and proxy guard Node has no reason for Bun to need, and a fetch
// that takes the {proxy} option Bun's fetch has. magpie loads this with
// --require before the host, so the host's own patching, its
// `const bunFetch = globalThis.fetch`, and every plugin's imports all see
// it in place. Nothing here runs when magpie runs plugins on Bun.

const cp = require("node:child_process")
const fs = require("node:fs")
const { createRequire } = require("node:module")
const path = require("node:path")
const { inspect } = require("node:util")

// host.js installs the guard once it is up; until it is, children start
// as plain Node would start them.
const guard = (o) => (typeof globalThis.__magpieGuard === "function" ? globalThis.__magpieGuard(o ?? {}) : o)

// Wrap a child_process entry point so every program a plugin starts has
// the host's stdin and stdout kept from it and magpie's proxy in its
// environment, the way host.js does on Bun by patching Bun.spawn: Bun
// starts every child_process program through Bun.spawn, Node doesn't, so
// each entry point is wrapped where it is. The options object is the
// first plain object after the command, skipping the args array and any
// callback; a run given none gets an empty guarded one.
const raw = { spawn: cp.spawn, spawnSync: cp.spawnSync }
function wrap(fn) {
  return function (...args) {
    if (typeof globalThis.__magpieGuard !== "function") return fn.apply(this, args)
    for (let i = 1; i < args.length; i++) {
      const a = args[i]
      if (a === undefined || a === null) continue
      if (Array.isArray(a) || typeof a === "function" || typeof a !== "object") continue
      args[i] = guard(a)
      return fn.apply(this, args)
    }
    args.push(guard({}))
    return fn.apply(this, args)
  }
}
for (const name of ["spawn", "spawnSync", "exec", "execSync", "execFile", "execFileSync", "fork"]) {
  cp[name] = wrap(cp[name])
}

// Bun.$, as host.js hands it to plugins: a template tag whose command the
// shell runs, interpolated values quoted so they never split, awaited for
// the output of the run. quiet, cwd, env, timeout and nothrow chain the
// way Bun's do; a run whose exit code isn't 0 throws with the output on
// it, unless nothrow was asked.
function shq(v) {
  if (v === undefined || v === null) v = ""
  if (ArrayBuffer.isView(v)) v = new TextDecoder().decode(v)
  if (Array.isArray(v)) return v.map(shq).join(" ")
  return "'" + String(v).replaceAll("'", `'\\''`) + "'"
}
function compile(strings, values) {
  if (typeof strings === "string") return strings
  let cmd = strings[0]
  for (let i = 0; i < values.length; i++) cmd += shq(values[i]) + (strings[i + 1] ?? "")
  return cmd
}
function shellRun(cmd, o) {
  o = o || {}
  const self = (strings, ...values) => shellRun(compile(strings, values), o)
  const done = new Promise((resolve, reject) => {
    const child = raw.spawn(cmd, { ...o, shell: true, stdio: ["ignore", "pipe", "pipe"] })
    let so = "", se = ""
    child.stdout.on("data", (d) => (so += d))
    child.stderr.on("data", (d) => (se += d))
    child.on("error", reject)
    let killer
    if (o.timeout > 0) killer = setTimeout(() => child.kill(), o.timeout)
    child.on("close", (code) => {
      if (killer) clearTimeout(killer)
      const r = {
        exitCode: code ?? 0, code: code ?? 0, signal: code === null ? "SIGTERM" : null,
        stdout: so, stderr: se, text: () => so, json: () => JSON.parse(so), lines: () => so.split("\n"),
      }
      // the options a chain set after the run started still apply to it,
      // as they do on Bun's ShellPromise: the close handler reads them now
      if (r.exitCode !== 0 && !o.nothrow) reject(Object.assign(new Error(`Failed with exit code: ${r.exitCode}`), r))
      else resolve(r)
    })
  })
  self.then = (ok, bad) => done.then(ok, bad)
  self.catch = (bad) => done.catch(bad)
  self.finally = (fn) => done.finally(fn)
  self.quiet = () => ((o.quiet = true), self)
  self.nothrow = () => ((o.nothrow = true), self)
  self.throws = () => ((o.nothrow = false), self)
  self.raw = () => self
  self.cwd = (p) => ((o.cwd = p), self)
  self.env = (e) => ((o.env = { ...o.env, ...e }), self)
  self.timeout = (ms) => ((o.timeout = ms), self)
  self.stdin = (v) => ((o.stdin = v), self)
  self.kill = () => self
  self.abort = () => self
  return self
}
const $ = (a, ...v) => {
  // $({cwd, env, ...})`cmd` first calls the tag with one options object
  if (a && typeof a === "object" && !Array.isArray(a) && !a.raw && v.length === 0) {
    const o = a
    return (strings, ...vals) => shellRun(compile(strings, vals), o)
  }
  return shellRun(compile(a, v), undefined)
}

// Bun.resolveSync(name, from): Node's resolver asked from a directory or
// a file, which Bun allows either way.
function resolveSync(name, from) {
  let base = from
  try {
    if (fs.statSync(from).isDirectory()) base = path.join(from, "noop.js")
  } catch {}
  return createRequire(path.resolve(base)).resolve(name)
}

globalThis.Bun = {
  version: process.versions.node,
  inspect: (x) => inspect(x, { colors: false, depth: 4 }),
  resolveSync,
  $,
  // Bun's array form. host.js guards these two itself, the way it does on
  // real Bun, so they hand out the raw spawn here.
  spawn: (at, o) => (Array.isArray(at) ? raw.spawn(at[0], at.slice(1), o) : raw.spawn(at, o)),
  spawnSync: (at, o) => (Array.isArray(at) ? raw.spawnSync(at[0], at.slice(1), o) : raw.spawnSync(at, o)),
}

// fetch taking the {proxy} option Bun's fetch has, through undici's
// ProxyAgent as the dispatcher. Node 22's own fetch is undici's, but the
// module to build dispatchers with isn't exported, so magpie downloads
// the matching undici and names it in MAGPIE_SHIM_UNDICI; a Node new
// enough to have node:undici needs neither. Without either, a fetch asked
// of a proxy is refused rather than sent direct: a vendor reached only
// through its proxy hangs a connection that goes around it, and a
// provider's own proxy setting would be quietly ignored, which magpie
// never does.
let undici
try {
  undici = require("node:undici")
} catch {}
if (!undici && process.env.MAGPIE_SHIM_UNDICI) {
  try {
    undici = require(process.env.MAGPIE_SHIM_UNDICI)
  } catch {}
}
if (undici?.fetch && undici?.ProxyAgent) {
  const ufetch = undici.fetch
  const agents = new Map()
  globalThis.fetch = function (input, init) {
    const i = init ? { ...init } : {}
    const p = i.proxy
    delete i.proxy
    if (p && !i.dispatcher) {
      let a = agents.get(p)
      if (!a) agents.set(p, (a = new undici.ProxyAgent(p)))
      i.dispatcher = a
    }
    return ufetch(input, i)
  }
} else {
  const plain = globalThis.fetch
  globalThis.fetch = function (input, init) {
    if (init?.proxy) return Promise.reject(new Error(`the plugin host's Node has no undici to route this fetch through ${init.proxy}`))
    return plain(input, init)
  }
}

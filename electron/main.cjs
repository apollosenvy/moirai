// Moirai Electron main.
//
// Spawns/attaches to the moirai daemon at 127.0.0.1:5984, opens a single
// BrowserWindow loading the renderer bundle, and proxies HTTP through
// the bridge so the renderer never speaks the network directly.
//
// Three IPC channels:
//   ipcMain.handle('daemon-call', ...)   -> HTTP proxy
//   ipcMain.handle('daemon-spawn', ...)  -> attach-or-spawn
//   webContents.send('daemon-status', ...) -> periodic /status broadcast

const { app, BrowserWindow, ipcMain, shell } = require('electron')
const path = require('node:path')
const fs = require('node:fs')
const http = require('node:http')
const { spawn } = require('node:child_process')

// ---------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------

const DAEMON_HOST = '127.0.0.1'
const DAEMON_PORT = 5984
const DEV = process.env.MOIRAI_DEV === '1'
const DEV_RENDERER_URL = 'http://localhost:5173'

// Resolve the moirai daemon binary. Operator override wins; otherwise
// look at the dev layout (sibling of electron/) and the install path.
function resolveDaemonBinary() {
  const candidates = []
  if (process.env.MOIRAI_BIN) {
    candidates.push(process.env.MOIRAI_BIN)
  }
  candidates.push(path.resolve(__dirname, '..', 'agent-router'))
  candidates.push('/usr/local/bin/moirai')
  for (const c of candidates) {
    try {
      const st = fs.statSync(c)
      if (st.isFile()) return c
    } catch {
      /* not present, continue */
    }
  }
  return null
}

// ---------------------------------------------------------------------
// Daemon HTTP client (no third-party deps; node:http is enough)
// ---------------------------------------------------------------------

function daemonCall({ method, path: reqPath, body }) {
  return new Promise((resolve) => {
    const payload = body === undefined || body === null ? null : Buffer.from(JSON.stringify(body))
    const headers = { Accept: 'application/json' }
    if (payload) {
      headers['Content-Type'] = 'application/json'
      headers['Content-Length'] = payload.length
    }
    const req = http.request(
      {
        host: DAEMON_HOST,
        port: DAEMON_PORT,
        method: method || 'GET',
        path: reqPath || '/',
        headers,
        // 30s ceiling on a single daemon call. /submit can be slow when
        // the daemon is mid-swap; the renderer's poll loop tolerates a
        // 30s blackout per tick.
        timeout: 30000,
      },
      (res) => {
        const chunks = []
        res.on('data', (c) => chunks.push(c))
        res.on('end', () => {
          resolve({
            status: res.statusCode || 0,
            body: Buffer.concat(chunks).toString('utf8'),
          })
        })
      },
    )
    req.on('error', (err) => {
      resolve({ status: 0, body: '', error: err.message })
    })
    req.on('timeout', () => {
      req.destroy(new Error('request timed out after 30s'))
    })
    if (payload) req.write(payload)
    req.end()
  })
}

// ---------------------------------------------------------------------
// Daemon lifecycle
// ---------------------------------------------------------------------

let daemonChild = null

async function isDaemonAlive() {
  const res = await daemonCall({ method: 'GET', path: '/health' })
  return res.status === 200
}

async function spawnDaemon() {
  const bin = resolveDaemonBinary()
  if (!bin) {
    return {
      ok: false,
      error: 'moirai binary not found; set MOIRAI_BIN or place agent-router beside electron/',
    }
  }

  // Attach if it's already up. Daemon enforces single-instance via a
  // pidfile, so spawning a second one will fail; cheaper to skip.
  if (await isDaemonAlive()) {
    return { ok: true, attached: true }
  }

  // Spawn detached so the daemon outlives the Electron process. stdio
  // ignored so we don't accumulate buffers; the daemon writes its own
  // log under ~/.local/share/agent-router/logs/.
  try {
    const child = spawn(bin, ['daemon'], {
      detached: true,
      stdio: 'ignore',
    })
    child.unref()
    daemonChild = child
  } catch (err) {
    return { ok: false, error: `spawn failed: ${err.message}` }
  }

  // Poll /health for up to 40s. The daemon waits for slot configs and
  // the kernel-anvil profile probe before answering ready, but /health
  // is up almost immediately.
  const deadline = Date.now() + 40000
  while (Date.now() < deadline) {
    if (await isDaemonAlive()) {
      return { ok: true, attached: false, pid: daemonChild?.pid }
    }
    await new Promise((r) => setTimeout(r, 500))
  }
  return { ok: false, error: 'daemon did not become healthy within 40s' }
}

// ---------------------------------------------------------------------
// Status broadcast
// ---------------------------------------------------------------------

function startStatusPump(win) {
  let stopped = false

  const tick = async () => {
    if (stopped) return
    const res = await daemonCall({ method: 'GET', path: '/status' })
    if (res.status === 200) {
      try {
        const parsed = JSON.parse(res.body)
        win.webContents.send('daemon-status', { connected: true, ...parsed })
      } catch {
        win.webContents.send('daemon-status', {
          connected: false,
          error: 'malformed /status body',
        })
      }
    } else {
      win.webContents.send('daemon-status', {
        connected: false,
        error: res.error || `status ${res.status}`,
      })
    }
  }

  // Fire once immediately, then every 1.5s -- matches the cadence the
  // Phos C++ runtime used so the renderer's existing throttling and
  // change-detection logic carries over unchanged.
  tick()
  const id = setInterval(tick, 1500)

  win.on('closed', () => {
    stopped = true
    clearInterval(id)
  })
}

// ---------------------------------------------------------------------
// Window
// ---------------------------------------------------------------------

let mainWindow = null

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1600,
    height: 1000,
    backgroundColor: '#0a0a0a',
    webPreferences: {
      preload: path.join(__dirname, 'preload.cjs'),
      contextIsolation: true,
      sandbox: false, // preload uses ipcRenderer
      nodeIntegration: false,
      // Disable web security only in dev so vite's HMR works without
      // CORS pain. Production loads file:// where it's irrelevant.
      webSecurity: !DEV,
    },
  })

  if (DEV) {
    mainWindow.loadURL(DEV_RENDERER_URL)
    mainWindow.webContents.openDevTools({ mode: 'detach' })
  } else {
    mainWindow.loadFile(path.join(__dirname, 'renderer', 'dist', 'index.html'))
  }

  // Open external links in the OS browser, never inside Electron.
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (url.startsWith('http://') || url.startsWith('https://')) {
      shell.openExternal(url)
    }
    return { action: 'deny' }
  })

  startStatusPump(mainWindow)
}

// ---------------------------------------------------------------------
// IPC routes
// ---------------------------------------------------------------------

ipcMain.handle('daemon-call', async (_event, payload) => {
  if (!payload || typeof payload !== 'object') {
    return { status: 0, body: '', error: 'payload must be an object' }
  }
  return daemonCall(payload)
})

ipcMain.handle('daemon-spawn', async () => {
  return spawnDaemon()
})

// ---------------------------------------------------------------------
// App lifecycle
// ---------------------------------------------------------------------

app.whenReady().then(createWindow)

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') {
    app.quit()
  }
})

app.on('activate', () => {
  if (BrowserWindow.getAllWindows().length === 0) createWindow()
})

// Note: we deliberately do NOT kill the daemon on app quit. The daemon
// is a system-level service; the user may have other tools attached
// (charon, the CLI, a different shell), and a UI close should not yank
// the rug out from under those. The user can `pkill -f "agent-router
// daemon"` if they want it gone.

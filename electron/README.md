# Moirai Electron app

Desktop UI for the Moirai daemon. React + Vite + Zustand renderer in
`renderer/`, Electron main + preload at the top level.

## Layout

```
electron/
├── main.cjs          # main process: window, daemon child, IPC routes
├── preload.cjs       # contextBridge: exposes window.moirai.{invoke,on,off}
├── package.json      # electron + electron-builder
└── renderer/
    ├── package.json  # react, vite, vitest, zustand
    ├── vite.config.ts
    ├── index.html
    └── src/          # the entire UI
```

## Bridge channels

The preload exposes a `window.moirai` object with three method shapes
that the renderer (`renderer/src/lib/daemonClient.ts`,
`renderer/src/App.tsx`, etc.) already calls:

| Method | Direction | Purpose |
|--------|-----------|---------|
| `moirai.invoke('daemon-call', {method, path, body})` | renderer → main | HTTP proxy to the daemon at `127.0.0.1:5984`. Returns `{status, body, error?}`. |
| `moirai.invoke('daemon-spawn')` | renderer → main | Attach to a running daemon, or spawn `agent-router daemon` if none is alive. Returns `{ok, pid?, error?}`. |
| `moirai.on('daemon-status', handler)` | main → renderer | Periodic `/status` broadcast every ~1.5s. Payload is the parsed JSON, plus `{connected: bool}`. Handler unsub by calling the returned `off` function. |

Every other request — slot patches, task submits, task list, slot list,
trace fetches — goes through `daemon-call`. The renderer never speaks
HTTP directly; it always goes through the bridge so the Electron main
process can handle daemon lifecycle, error normalisation, and future
features like log streaming.

## Daemon binary

The main process looks for the daemon binary in this order:

1. `$MOIRAI_BIN` (operator override)
2. `../agent-router` (sibling of `electron/`, repo dev layout)
3. `/usr/local/bin/moirai`

If none are found, `daemon-spawn` returns `{ok: false, error: 'moirai
binary not found; set MOIRAI_BIN or place agent-router beside
electron/'}`. The offline panel surfaces the error string directly.

## Dev workflow

```bash
# Build the moirai daemon (sibling of electron/)
go build -o agent-router ./cmd/moirai

# Renderer deps
cd electron/renderer
npm install

# Electron deps
cd ..
npm install

# Run dev (vite on :5173, electron loads it, hot reload)
MOIRAI_DEV=1 npm run dev

# Production build (renderer dist + electron-builder package)
npm run build
```

## Production build

`npm run build` produces:

- `renderer/dist/`: the static React bundle
- `dist-app/`: Linux AppImage and `.deb` (configured under
  `package.json#build.linux`)

The AppImage runs standalone; place a `agent-router` binary next to it
(or in `/usr/local/bin/moirai`) and the app spawns/attaches on launch.

## Tests

```bash
cd renderer
npm run test
```

Renderer tests live in `renderer/src/` colocated with the components
they cover (`*.test.ts(x)`) plus integration suites under
`renderer/src/__tests__/`. Tests stub `window.moirai` directly so the
Electron bridge doesn't need to be live.

## Origin note

This app is a port of the Phos-embedded UI we run internally. The
React code is byte-identical except for one rename
(`window.phos` → `window.moirai`); the Electron `main.cjs` and
`preload.cjs` provide the same bridge surface that the Phos C++ runtime
provided. Two binaries, same UI.

// Moirai Electron preload.
//
// Exposes a `window.moirai` object that mirrors the legacy `window.phos`
// surface so the existing renderer code in renderer/src/ runs unchanged:
//
//   moirai.invoke(channel, payload)  -> Promise<reply>
//   moirai.on(channel, handler)      -> off()
//   moirai.off(channel, handler)
//
// The two supported `invoke` channels are 'daemon-call' (HTTP proxy) and
// 'daemon-spawn' (attach-or-spawn the moirai daemon). The single supported
// `on` channel is 'daemon-status' (periodic /status broadcast from main).
//
// contextIsolation is on; never expose `ipcRenderer` itself.

const { contextBridge, ipcRenderer } = require('electron')

const ALLOWED_INVOKE_CHANNELS = new Set(['daemon-call', 'daemon-spawn'])
const ALLOWED_EVENT_CHANNELS = new Set(['daemon-status'])

// Map each renderer-side handler to its ipcRenderer wrapper so off() can
// remove the actual listener registered on ipcRenderer rather than the
// renderer-supplied function (which ipcRenderer never saw directly).
const handlerWrappers = new WeakMap()

contextBridge.exposeInMainWorld('moirai', {
  invoke(channel, payload) {
    if (!ALLOWED_INVOKE_CHANNELS.has(channel)) {
      return Promise.reject(new Error(`channel not allowed: ${channel}`))
    }
    return ipcRenderer.invoke(channel, payload)
  },

  on(channel, handler) {
    if (!ALLOWED_EVENT_CHANNELS.has(channel)) {
      throw new Error(`channel not allowed: ${channel}`)
    }
    if (typeof handler !== 'function') {
      throw new Error('handler must be a function')
    }
    const wrapped = (_event, payload) => handler(payload)
    handlerWrappers.set(handler, wrapped)
    ipcRenderer.on(channel, wrapped)
    return () => {
      const w = handlerWrappers.get(handler)
      if (w) {
        ipcRenderer.removeListener(channel, w)
        handlerWrappers.delete(handler)
      }
    }
  },

  off(channel, handler) {
    if (!ALLOWED_EVENT_CHANNELS.has(channel)) return
    const w = handlerWrappers.get(handler)
    if (w) {
      ipcRenderer.removeListener(channel, w)
      handlerWrappers.delete(handler)
    }
  },
})

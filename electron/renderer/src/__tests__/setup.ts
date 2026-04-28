import '@testing-library/jest-dom'

// Stub the Phos bridge so components that call window.moirai.invoke
// during render (or inside effect hooks) don't explode under jsdom.
if (typeof window !== 'undefined') {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  ;(window as any).moirai = (window as any).moirai ?? {
    invoke: async () => ({ status: 0, body: '' }),
    // Match the live contract: bridge.on returns an unsubscribe Function.
    // The previous stub returned undefined, which masked a regression
    // where StrictMode double-mount would leave duplicate listeners.
    on: () => () => {},
    off: () => {},
  }
}

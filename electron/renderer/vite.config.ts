import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Vite config for the Moirai Electron renderer.
//
// base './' so the built bundle loads correctly under file:// when
// Electron does win.loadFile(...). With an absolute base ('/'), the
// asset URLs would resolve against the filesystem root, not the
// renderer/dist/ directory.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    strictPort: true,
  },
})

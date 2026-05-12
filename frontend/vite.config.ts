import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      '/api/ws': { target: 'ws://localhost:3000', ws: true },
      '/api': 'http://localhost:3000',
    },
  },
  optimizeDeps: {
    include: ['@babylonjs/core'],
    // Babylon's modular ESM build does internal `await import()` calls.
    // The dep optimizer (Rolldown in Vite 8) would otherwise split those
    // into separate chunks that race against namespace initialization,
    // crashing with "Cannot read properties of undefined (reading
    // 'MatrixTrackPrecisionChange')". Mirror the production manualChunks
    // workaround to force all Babylon code into a single chunk.
    rolldownOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/@babylonjs/')) {
            return 'babylon'
          }
        },
      },
    },
  },
  build: {
    // Keep all Babylon.js code in a single chunk. Babylon's modular
    // build uses internal `await import()` calls that the bundler
    // would otherwise split into separate chunks; those chunks register
    // onto namespaces that may not be initialized yet at load time,
    // producing runtime errors like "Cannot set properties of undefined
    // (setting 'DumpData')". Forcing a single babylon chunk eliminates
    // the race entirely.
    rolldownOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/@babylonjs/')) {
            return 'babylon'
          }
        },
      },
    },
  },
})

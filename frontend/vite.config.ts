import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      '/api': 'http://localhost:3000',
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

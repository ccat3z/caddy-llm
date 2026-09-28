import path from 'path'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  // Relative asset URLs so the embedded UI works under any mount prefix
  // (llm_tracer_api serves it at {api-prefix}/ui/).
  base: './',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    // Proxy the traces API to a local caddy-llm during development:
    //   caddy-llm run --config examples/traced-translation.json
    proxy: {
      '/llm': 'http://127.0.0.1:8080',
    },
  },
})

import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

const apiProxyTarget = 'http://127.0.0.1:8080'
const apiPublicOrigin = 'http://localhost:8080'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: fileURLToPath(new URL('../internal/webui/dist', import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': {
        target: apiProxyTarget,
        changeOrigin: true,
        configure(proxy) {
          proxy.on('proxyReq', (proxyRequest, request) => {
            // Vite serves the browser from :5173, while the backend validates
            // unsafe requests against its configured :8080 public origin.
            // Rewrite only the forwarded request; production CSRF checks stay
            // strict and the browser never talks cross-origin to the API.
            if (request.headers.origin) {
              proxyRequest.setHeader('origin', apiPublicOrigin)
            }
          })
        },
      },
    },
  },
})

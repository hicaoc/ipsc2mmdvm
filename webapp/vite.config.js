import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

const devTarget = 'http://js.nrlptt.com:9201'

export default defineConfig({
  plugins: [vue()],
  server: {
    host: '0.0.0.0',
    port: 5173,
    proxy: {
      '/api': {
        target: devTarget,
        changeOrigin: true
      },
      '/ws': {
        target: devTarget,
        changeOrigin: true,
        ws: true
      }
    }
  },
  build: {
    outDir: resolve(__dirname, '../internal/web/dist'),
    emptyOutDir: true
  }
})

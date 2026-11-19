import { defineConfig } from 'vite'

// 本地开发：call-server 跑在 8080，页面通过 Vite 代理访问接口和信令
const backend = process.env.BACKEND ?? 'http://localhost:8080'

export default defineConfig({
  server: {
    proxy: {
      '/api': backend,
      '/ws': { target: backend.replace(/^http/, 'ws'), ws: true },
    },
  },
  build: {
    outDir: 'dist',
    target: 'es2022',
    // livekit-client 本身约 500KB，单页应用不再拆包
    chunkSizeWarningLimit: 800,
  },
})

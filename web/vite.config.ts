import { defineConfig } from 'vite'

// 本地开发：call-server 跑在 8080。页面挂在 /nexcall/，代理去掉前缀后再转发。
const backend = process.env.BACKEND ?? 'http://localhost:8080'

export default defineConfig({
  base: '/nexcall/',
  server: {
    proxy: {
      '/nexcall/api': {
        target: backend,
        rewrite: (path) => path.replace(/^\/nexcall/, ''),
      },
      '/nexcall/ws': {
        target: backend.replace(/^http/, 'ws'),
        ws: true,
        rewrite: (path) => path.replace(/^\/nexcall/, ''),
      },
    },
  },
  build: {
    outDir: 'dist',
    target: 'es2022',
    chunkSizeWarningLimit: 800,
  },
})

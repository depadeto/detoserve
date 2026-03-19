import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      '/api/clusters': 'http://localhost:8085',
      '/api': 'http://localhost:8086',
    },
  },
})

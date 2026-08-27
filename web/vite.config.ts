import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig(({ mode }) => {
  // Load .env from repo root (../) so a single root .env works for gateway + web.
  // Vite also loads web/.env by default — root takes precedence via envDir.
  const rootEnv = loadEnv(mode, '..', '')
  const webEnv = loadEnv(mode, '.', '')
  const env: Record<string, string> = { ...rootEnv, ...webEnv } // web/.env can override root if present

  // PUBLIC_URL is the Cloudflare Tunnel hostname — also usable as gateway target
  const gatewayUrl =
    env.VITE_GATEWAY_URL ||
    env.VITE_PUBLIC_URL ||
    env.VITE_API_URL ||
    env.PUBLIC_URL ||
    (env.PORT ? `http://localhost:${env.PORT}` : 'http://localhost:8080')
  const vitePort = parseInt(env.VITE_PORT || '5173', 10)

  return {
    plugins: [react()],
    envDir: '..',
    server: {
      port: vitePort,
      proxy: {
        '/api': gatewayUrl,
        '/v1': gatewayUrl,
        '/health': gatewayUrl,
        '/ready': gatewayUrl,
        '/metrics': gatewayUrl,
      },
    },
    build: {
      outDir: '../cmd/gateway/web',
      emptyOutDir: true,
    },
  }
})

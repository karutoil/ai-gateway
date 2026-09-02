#!/usr/bin/env node
/**
 * Post-build guard: the production bundle must never contain an absolute
 * dev API URL (localhost / 127.0.0.1 on a VITE_* port). This exact bug
 * shipped once: VITE_GATEWAY_URL from the root .env (dev proxy config)
 * leaked into import.meta.env at build time, so the served dashboard tried
 * to call http://localhost:8787 and CSP blocked every request.
 *
 * The dashboard must always call the gateway same-origin with relative
 * /api paths. If this check fails: check .env.production — it must set
 * VITE_GATEWAY_URL= (empty) to override the dev value.
 */
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

// vite.config.ts sets outDir to ../cmd/gateway/web
const assetsDir = new URL('../../cmd/gateway/web/assets', import.meta.url).pathname
const FORBIDDEN = [/https?:\/\/(localhost|127\.0\.0\.1)(:\d+)?\/(api|v1|health|ready)/]

let failures = []
for (const f of readdirSync(assetsDir)) {
  if (!f.endsWith('.js') && !f.endsWith('.css')) continue
  const content = readFileSync(join(assetsDir, f), 'utf8')
  for (const re of FORBIDDEN) {
    if (re.test(content)) {
      failures.push(`${f}: contains forbidden absolute dev URL matching ${re}`)
    }
  }
}
if (failures.length) {
  console.error('BUNDLE CHECK FAILED — dev API URL baked into production assets:')
  for (const f of failures) console.error('  ' + f)
  console.error('\nFix: ensure .env.production sets VITE_GATEWAY_URL= (empty).')
  process.exit(1)
}
console.log('bundle check ok: no absolute dev API URLs in production assets')

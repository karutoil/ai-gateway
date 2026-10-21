/**
 * Attachment intake for the Playground composer: file → downscaled image data
 * URL, or text extraction, or a name-only manifest entry for binaries.
 *
 * Browser APIs (FileReader, canvas, Image) only — no React.
 */
import { fmtBytes, uid } from './format'
import type { AttachKind, Attachment } from './types'

export const IMAGE_ACCEPT = 'image/*'
export const MAX_FILES = 8
export const MAX_IMAGE_EDGE = 1568
export const MAX_TEXT_BYTES = 256 * 1024
export const MAX_IMAGE_BYTES = 5 * 1024 * 1024

const TEXT_MIME = /^(text\/|application\/(json|xml|x-yaml|javascript|typescript|toml|sql|graphql|sh|x-sh|x-python|x-httpd-php))/
const TEXT_EXT = /\.(txt|md|markdown|json|jsonl|ya?ml|toml|ini|cfg|conf|env|csv|tsv|log|xml|html?|css|scss|less|jsx?|tsx?|mjs|cjs|py|rb|go|rs|java|kt|swift|c|cc|cpp|h|hpp|cs|php|sh|bash|zsh|sql|graphql|gql|proto|tf|hcl|dockerfile|makefile|gitignore|patch|diff)$/i

/** Downscale an image to a sane vision size, re-encoding only when needed. */
export async function prepareImage(file: File): Promise<Attachment> {
  const base: Attachment = { id: uid(), name: file.name, size: file.size, mime: file.type, kind: 'image' }
  const dataUrl = await new Promise<string>((resolve, reject) => {
    const fr = new FileReader()
    fr.onload = () => resolve(String(fr.result))
    fr.onerror = () => reject(new Error(`could not read ${file.name}`))
    fr.readAsDataURL(file)
  })
  if (file.type === 'image/gif' || file.type === 'image/svg+xml') {
    // Canvas re-encoding would destroy animation / vector data.
    base.dataUrl = dataUrl
    if (file.size > MAX_IMAGE_BYTES) base.note = 'large — may be rejected upstream'
    return base
  }
  try {
    const img = await new Promise<HTMLImageElement>((resolve, reject) => {
      const el = new Image()
      el.onload = () => resolve(el)
      el.onerror = () => reject(new Error('decode failed'))
      el.src = dataUrl
    })
    const longest = Math.max(img.naturalWidth, img.naturalHeight)
    const needsScale = longest > MAX_IMAGE_EDGE
    const needsShrink = dataUrl.length * 0.75 > 900 * 1024
    if (!needsScale && !needsShrink) {
      base.dataUrl = dataUrl
      return base
    }
    const scale = needsScale ? MAX_IMAGE_EDGE / longest : 1
    const canvas = document.createElement('canvas')
    canvas.width = Math.max(1, Math.round(img.naturalWidth * scale))
    canvas.height = Math.max(1, Math.round(img.naturalHeight * scale))
    const ctx = canvas.getContext('2d')
    if (!ctx) { base.dataUrl = dataUrl; return base }
    ctx.drawImage(img, 0, 0, canvas.width, canvas.height)
    const out = canvas.toDataURL('image/jpeg', 0.86)
    base.dataUrl = out.length < dataUrl.length ? out : dataUrl
    if (base.dataUrl !== dataUrl) {
      base.note = `resized to ${canvas.width}×${canvas.height}`
      base.mime = 'image/jpeg'
      base.size = Math.round(base.dataUrl.length * 0.75)
    }
    return base
  } catch {
    base.dataUrl = dataUrl
    return base
  }
}

export async function prepareFile(file: File): Promise<Attachment> {
  if (file.type.startsWith('image/')) return prepareImage(file)
  const kind: AttachKind = TEXT_MIME.test(file.type) || TEXT_EXT.test(file.name) ? 'text' : 'other'
  const base: Attachment = { id: uid(), name: file.name, size: file.size, mime: file.type || 'application/octet-stream', kind }
  if (kind === 'text') {
    const raw = await file.text()
    if (raw.length > MAX_TEXT_BYTES) {
      base.text = raw.slice(0, MAX_TEXT_BYTES)
      base.note = `truncated at ${fmtBytes(MAX_TEXT_BYTES)}`
    } else {
      base.text = raw
    }
  } else if (file.size > 0) {
    base.note = 'binary — name only'
  }
  return base
}

/**
 * Endpoint table for the Playground. Shared by the page and the message
 * components (the endpoint chip on every assistant turn).
 */
import type { IconName } from '../../components/ui'
import type { Mode } from '../../lib/playground/types'

export const MODES: { id: Mode; label: string; short: string; path: string; icon: IconName; blurb: string }[] = [
  { id: 'openai', label: 'Chat Completions', short: 'chat', path: '/v1/chat/completions', icon: 'messageSquare', blurb: 'OpenAI-compatible chat with full sampling control.' },
  { id: 'anthropic', label: 'Anthropic Messages', short: 'messages', path: '/v1/messages', icon: 'sparkles', blurb: 'Native Messages shape with system + thinking.' },
  { id: 'responses', label: 'Responses', short: 'responses', path: '/v1/responses', icon: 'arrowRight', blurb: 'Responses API with instructions and stored turns.' },
  { id: 'systemone', label: 'Decisions', short: 'decisions', path: '/v1/systemone', icon: 'route', blurb: 'Typed probabilistic answers over a state.' },
  { id: 'images', label: 'Images', short: 'images', path: '/v1/images/generations', icon: 'image', blurb: 'Image generation and editing.' },
  { id: 'video', label: 'Video', short: 'video', path: '/v1/videos', icon: 'play', blurb: 'Async video jobs, polled to completion.' },
]

export const modeOf = (id: Mode) => MODES.find(m => m.id === id) || MODES[0]

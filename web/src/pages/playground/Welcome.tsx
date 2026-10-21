/**
 * Empty-state / welcome pane for the Playground: endpoint blurb, template
 * prompts and the "no models" banner.
 */
import { Icon } from '../../components/ui'
import type { Mode } from '../../lib/playground/types'
import { modeOf } from './constants'

const TEMPLATES: { icon: any; title: string; hint: string; prompt: string; mode?: Mode }[] = [
  { icon: 'messageSquare', title: 'Quick question', hint: 'Ask normally and inspect usage', prompt: 'In two sentences, what does an LLM gateway do?' },
  { icon: 'sparkles', title: 'Structured output', hint: 'Validate JSON shaping', prompt: 'Return a JSON object with name, version, capabilities and status for an LLM gateway.' },
  { icon: 'image', title: 'Generate media', hint: 'Try an image prompt', prompt: 'An editorial product photograph of a copper mechanical keyboard on a dark stone desk, soft side light.', mode: 'images' },
  { icon: 'route', title: 'Routing decision', hint: 'See model selection detail', prompt: 'Analyse this request and explain which provider should handle it based on latency, capabilities, cost and failover.' },
]

export function Welcome({ mode, setMode, model, hasModels, onPick, onAttach, onOpenSettings }: {
  mode: Mode; setMode: (m: Mode) => void; model: string; hasModels: boolean
  onPick: (t: string) => void; onAttach: () => void; onOpenSettings: () => void
}) {
  const m = modeOf(mode)
  return (
    <div className="pb-8 pt-2 text-center sm:pt-[4vh] 2xl:pt-[8vh]">
      <div className="mx-auto w-11 h-11 2xl:w-14 2xl:h-14 rounded-xl bg-accent/10 border border-accent/25 text-accent flex items-center justify-center">
        <Icon name={m.icon} size={20} />
      </div>
      <h2 className="mt-4 text-xl sm:text-2xl 2xl:text-3xl font-semibold -tracking-tight">
        {mode === 'images' ? 'Create and inspect images'
          : mode === 'video' ? 'Prototype video generations'
          : mode === 'systemone' ? 'Ask the decision model'
          : 'What would you like to test?'}
      </h2>
      <p className="mt-2 text-sm 2xl:text-base text-muted max-w-md 2xl:max-w-lg mx-auto leading-relaxed">{m.blurb}</p>

      {!hasModels && (
        // text-amber on plain bg-surface clears 4.5:1 in the light theme;
        // the previous amber/10 tint measured 4.11.
        <div className="mt-5 mx-auto max-w-md flex items-start gap-2 text-left text-xs text-amber bg-surface border border-amber/40 rounded-lg px-3 py-2.5" role="status">
          <Icon name="alert" size={13} className="mt-0.5 shrink-0" />
          <span>No models available for this endpoint. Open <b>Models</b> and run Discover, then come back.</span>
        </div>
      )}

      {/* Phones get compact one-line cards: the full two-line grid is taller
          than a small viewport once the composer is on screen. */}
      <div className="mx-auto mt-5 grid max-w-[620px] grid-cols-1 gap-2 text-left sm:mt-7 sm:grid-cols-2 sm:gap-2.5 2xl:mt-9 2xl:max-w-[760px] 2xl:gap-3">
        {TEMPLATES.map(t => (
          <button key={t.title}
            onClick={() => { if (t.mode) setMode(t.mode); onPick(t.prompt) }}
            className="group flex items-center gap-2.5 rounded-xl border border-stone bg-surface p-2.5 text-left transition-colors hover:border-accent/40 hover:bg-raised/50 sm:block sm:p-3.5 2xl:p-4">
            <span className="inline-flex w-6 h-6 shrink-0 items-center justify-center rounded-md border border-stone bg-raised text-muted group-hover:border-accent/30 group-hover:text-accent 2xl:w-7 2xl:h-7">
              <Icon name={t.icon} size={13} />
            </span>
            <b className="min-w-0 truncate text-[13px] font-semibold sm:ml-2 2xl:text-sm">{t.title}</b>
            <span className="mt-1.5 hidden text-xs leading-relaxed text-muted sm:block 2xl:text-[13px]">{t.hint}</span>
          </button>
        ))}
      </div>

      <div className="mt-5 flex flex-wrap items-center justify-center gap-2 text-xs text-muted sm:mt-7">
        <button onClick={onAttach} className="inline-flex items-center gap-1.5 h-8 px-3 rounded-lg border border-stone bg-surface hover:border-accent/40 hover:text-paper transition-colors">
          <Icon name="paperclip" size={13} /> Attach a file
        </button>
        <button onClick={onOpenSettings} className="inline-flex items-center gap-1.5 h-8 px-3 rounded-lg border border-stone bg-surface hover:border-accent/40 hover:text-paper transition-colors">
          <Icon name="sliders" size={13} /> Request parameters
        </button>
        <span className="font-mono text-[10px] text-muted px-1">{model || 'no model selected'}</span>
      </div>
    </div>
  )
}

/**
 * Request-configuration panel. Prop-driven and shared verbatim by the inline
 * column (≥1280px) and the narrow-viewport drawer.
 */
import {
  Button, Field, Icon, Input, SegmentedControl, Select, Textarea,
} from '../../components/ui'
import ModelCombobox from '../../components/ModelCombobox'
import { modeOf } from './constants'
import { MODES } from './constants'
import type { CatalogModel, QType, QuestionDraft } from '../../lib/playground/types'

export type SettingsProps = {
  mode: string
  setMode: (m: any) => void
  isChatMode: boolean; isDecision: boolean; isImageMode: boolean; isVideoMode: boolean
  model: string; catalogInfo: CatalogModel | null
  visibleModels: CatalogModel[]; comboboxOptions: any[]; setModel: (m: string) => void
  system: string; setSystem: (v: string) => void; systemRef: React.RefObject<HTMLTextAreaElement>
  stream: boolean; setStream: (v: boolean) => void
  temperature: string; setTemperature: (v: string) => void
  maxTokens: string; setMaxTokens: (v: string) => void
  reasoningEffort: string; setReasoningEffort: (v: string) => void
  supportsReasoning: boolean; reasoningLevels: string[]
  imageN: number; setImageN: (v: number) => void
  imageSize: string; setImageSize: (v: string) => void
  videoSeconds: string; setVideoSeconds: (v: string) => void
  videoSize: string; setVideoSize: (v: string) => void
  stateInput: string; setStateInput: (v: string) => void
  questions: QuestionDraft[]; setQuestions: (q: QuestionDraft[]) => void
  gatewayKey: string; setGatewayKey: (v: string) => void
  availableKeys: any[]; advanced: boolean; setAdvanced: (v: boolean) => void
  onClose: () => void; onReset: () => void
}

export function SettingsPanel(p: SettingsProps) {
  const caption = 'block text-xs font-medium text-muted mb-1.5'
  const isMedia = p.isImageMode || p.isVideoMode
  return (
    <>
      <div className="shrink-0 h-11 px-4 border-b border-stone flex items-center gap-2">
        <span className="text-[13px] font-semibold -tracking-tight">Request configuration</span>
        <div className="ml-auto flex items-center gap-1">
          <Button size="sm" variant="ghost" className="!px-1.5 !text-[11px]" onClick={p.onReset} title="Reset to defaults">
            <Icon name="refresh" size={12} /> Reset
          </Button>
          <Button size="sm" variant="ghost" className="!px-1.5 xl:!hidden" onClick={p.onClose} aria-label="Close settings">
            <Icon name="x" size={14} />
          </Button>
        </div>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto p-4 space-y-5">
        {/* Endpoint */}
        <section>
          <div className={caption}>API endpoint</div>
          <div className="grid grid-cols-2 gap-1.5">
            {MODES.map(m => (
              <button key={m.id} onClick={() => p.setMode(m.id)}
                aria-pressed={p.mode === m.id}
                className={`flex items-center gap-1.5 rounded-lg border px-2 py-1.5 text-left text-[11px] transition-colors ${
                  p.mode === m.id ? 'border-accent/50 bg-accent/10 text-paper' : 'border-stone bg-app text-muted hover:text-paper'}`}>
                <Icon name={m.icon} size={12} className="shrink-0" />
                <span className="truncate">{m.short}</span>
              </button>
            ))}
          </div>
          <div className="mt-2 font-mono text-[10px] text-muted truncate">{modeOf(p.mode as any).path}</div>
        </section>

        {/* Model */}
        <section>
          <div className={caption}>Model</div>
          <ModelCombobox
            mode="single"
            value={p.model ? [p.model] : []}
            onChange={next => p.setModel(next.length ? next[next.length - 1] : '')}
            options={p.comboboxOptions}
            placeholder={p.visibleModels.length ? 'Select a model' : 'No models discovered'}
            ariaLabel="Model"
          />
          {p.visibleModels.length === 0 && (
            <div className="mt-1.5 flex items-center gap-1.5 text-[11px] text-amber" role="status">
              <Icon name="alert" size={11} />
              {p.isDecision ? 'No decision models found.' : 'No models discovered — run Discover on Models.'}
            </div>
          )}
          {p.catalogInfo && (
            <div className="mt-2 rounded-lg border border-stone bg-app/60 p-2.5 space-y-1.5">
              <div className="font-mono text-[11px] text-muted truncate">{p.catalogInfo.model_id}</div>
              <div className="font-mono text-[11px] text-muted">via <span className="text-accent">{p.catalogInfo.provider_name}</span></div>
              <div className="flex gap-1 flex-wrap pt-0.5">
                {!!p.catalogInfo.context_window && <Chip>ctx {(p.catalogInfo.context_window / 1000).toFixed(0)}k</Chip>}
                {!!p.catalogInfo.max_output && <Chip>out {(p.catalogInfo.max_output / 1000).toFixed(0)}k</Chip>}
                {p.catalogInfo.attachment && <Chip>vision</Chip>}
                {p.catalogInfo.tool_call && <Chip>tools</Chip>}
                {p.catalogInfo.reasoning && <Chip>reasoning</Chip>}
                {!!p.catalogInfo.input_cost && <Chip accent>${p.catalogInfo.input_cost}/{p.catalogInfo.output_cost}/1M</Chip>}
              </div>
            </div>
          )}
        </section>

        {/* Media settings */}
        {p.isImageMode && (
          <section className="space-y-3">
            <div className={caption}>Image generation</div>
            <div className="grid grid-cols-2 gap-2">
              <Field label="Images">
                <Input type="number" min={1} max={4} value={p.imageN} inputMode="numeric" className="font-mono"
                  onChange={e => p.setImageN(Math.max(1, Math.min(4, parseInt(e.target.value, 10) || 1)))} />
              </Field>
              <Field label="Size">
                <Select value={p.imageSize} onChange={e => p.setImageSize(e.target.value)}>
                  {['1024x1024', '1536x1024', '1024x1536', '1792x1024', '1024x1792', 'auto'].map(s => <option key={s} value={s}>{s}</option>)}
                </Select>
              </Field>
            </div>
            <div className="text-[11px] text-muted leading-relaxed">
              Attach a reference image in the composer to condition generation where the model supports it.
            </div>
          </section>
        )}

        {p.isVideoMode && (
          <section className="space-y-3">
            <div className={caption}>Video generation</div>
            <div className="grid grid-cols-2 gap-2">
              <Field label="Seconds">
                <Select value={p.videoSeconds} onChange={e => p.setVideoSeconds(e.target.value)}>
                  {['4', '8', '12'].map(s => <option key={s} value={s}>{s}</option>)}
                </Select>
              </Field>
              <Field label="Size">
                <Select value={p.videoSize} onChange={e => p.setVideoSize(e.target.value)}>
                  {['1280x720', '720x1280', '1024x1792', '1792x1024'].map(s => <option key={s} value={s}>{s}</option>)}
                </Select>
              </Field>
            </div>
            <div className="text-[11px] text-muted leading-relaxed">
              Attach a reference image in the composer to condition the generation where the model supports it.
            </div>
          </section>
        )}

        {/* Decision settings */}
        {p.isDecision && (
          <section className="space-y-3">
            <div className={caption}>State</div>
            <Textarea value={p.stateInput} onChange={e => p.setStateInput(e.target.value)} rows={4}
              placeholder="Plain text, or JSON for structured state." aria-label="Decision state" />
            <QuestionsEditor questions={p.questions} onChange={p.setQuestions} />
          </section>
        )}

        {/* Chat params */}
        {p.isChatMode && (
          <section className="space-y-3">
            <div className={caption}>Model behaviour</div>
            <div className="flex items-center justify-between gap-2">
              <span className="text-xs text-muted">Streaming</span>
              <SegmentedControl<'on' | 'off'>
                options={[{ value: 'on', label: 'On' }, { value: 'off', label: 'Off' }]}
                value={p.stream ? 'on' : 'off'}
                onChange={v => p.setStream(v === 'on')}
              />
            </div>
            <div className="grid grid-cols-2 gap-2">
              <Field label="Temperature">
                <Input value={p.temperature} onChange={e => p.setTemperature(e.target.value)} placeholder="default" inputMode="decimal" className="font-mono" />
              </Field>
              <Field label="Max tokens">
                <Input value={p.maxTokens} onChange={e => p.setMaxTokens(e.target.value)} placeholder="1024" inputMode="numeric" className="font-mono" />
              </Field>
            </div>
            <Field label="Reasoning effort">
              <Select value={p.reasoningEffort} onChange={e => p.setReasoningEffort(e.target.value)}>
                <option value="">off / default</option>
                {(p.supportsReasoning ? p.reasoningLevels : ['low', 'medium', 'high']).map(lv => <option key={lv} value={lv}>{lv}</option>)}
              </Select>
            </Field>
            {p.catalogInfo && !p.catalogInfo.reasoning && p.reasoningEffort && (
              <div className="flex items-center gap-1.5 text-[11px] text-amber">
                <Icon name="alert" size={11} /> This model may reject reasoning_effort.
              </div>
            )}
          </section>
        )}

        {/* Instructions */}
        {!isMedia && !p.isDecision && (
          <section>
            <div className={caption}>System / developer instructions</div>
            <textarea
              ref={p.systemRef}
              value={p.system}
              onChange={e => p.setSystem(e.target.value)}
              rows={4}
              aria-label="System instructions"
              placeholder="You are a helpful assistant…"
              className="w-full bg-app border border-stone rounded-lg px-3 py-2.5 text-xs font-mono leading-relaxed placeholder:text-muted focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/20"
            />
          </section>
        )}

        {/* Advanced */}
        <section>
          <button onClick={() => p.setAdvanced(!p.advanced)} aria-expanded={p.advanced}
            className="inline-flex items-center gap-1.5 text-xs font-medium text-muted hover:text-paper transition-colors">
            <Icon name="chevronDown" size={13} className={`transition-transform duration-150 ${p.advanced ? 'rotate-180' : ''}`} />
            Authentication
          </button>
          {p.advanced && (
            <div className="mt-3 space-y-3 rounded-lg border border-stone bg-app/40 p-3">
              <Field label="Gateway key" hint="Overrides the dashboard session for this request only.">
                <Input value={p.gatewayKey} onChange={e => p.setGatewayKey(e.target.value)}
                  placeholder="sk-gw-… (optional — session cookie is used otherwise)" />
              </Field>
              {!p.gatewayKey.trim() && (
                <div className="-mt-2 flex items-center gap-1.5 text-[11px] text-accent font-mono">
                  <Icon name="lock" size={11} /> using the logged-in session cookie
                </div>
              )}
              {p.availableKeys.length > 0 && (
                <div className="flex flex-wrap gap-1 items-center">
                  {p.availableKeys.slice(0, 4).map((k: any) => (
                    <span key={k.id} className="font-mono text-[10px] text-muted border border-stone px-1.5 py-0.5 rounded-full">..{k.prefix}</span>
                  ))}
                  <span className="text-[10px] text-muted">copy a key from the Keys page to impersonate it</span>
                </div>
              )}
            </div>
          )}
        </section>
      </div>
    </>
  )
}

function Chip({ children, accent }: { children: React.ReactNode; accent?: boolean }) {
  return (
    <span className={`font-mono text-[10px] tabular-nums border rounded-full px-1.5 py-px ${
      accent ? 'border-accent/30 text-accent' : 'border-stone text-muted'}`}>{children}</span>
  )
}

export function QuestionsEditor({ questions, onChange }: { questions: QuestionDraft[]; onChange: (q: QuestionDraft[]) => void }) {
  const update = (i: number, patch: Partial<QuestionDraft>) =>
    onChange(questions.map((q, idx) => (idx === i ? { ...q, ...patch } : q)))
  const remove = (i: number) => onChange(questions.filter((_, idx) => idx !== i))
  const add = () => onChange([...questions, { id: `q${questions.length + 1}`, type: 'noul', instructions: '', criteria: '' }])

  return (
    <div>
      <div className="flex items-center justify-between mb-1.5">
        <span className="text-xs font-medium text-muted">Questions</span>
        <button type="button" onClick={add} className="inline-flex items-center gap-1 text-xs text-accent hover:underline">
          <Icon name="plus" size={12} /> Add
        </button>
      </div>
      <div className="space-y-2">
        {questions.map((q, i) => (
          <div key={i} className="rounded-lg border border-stone bg-app/40 p-2.5 space-y-2">
            <div className="flex items-center gap-2">
              <div className="flex-1 min-w-0">
                <Input value={q.id} onChange={e => update(i, { id: e.target.value })} placeholder="question id" className="font-mono"
                  aria-label={`Question ${i + 1} id`} />
              </div>
              <div className="w-24 shrink-0">
                <Select value={q.type} onChange={e => update(i, { type: e.target.value as QType })}
                  aria-label={`Question ${i + 1} type`}>
                  <option value="noul">noul</option>
                  <option value="choice">choice</option>
                  <option value="score">score</option>
                </Select>
              </div>
              <button type="button" onClick={() => remove(i)} disabled={questions.length <= 1}
                className="text-muted hover:text-danger disabled:opacity-40 transition-colors shrink-0"
                aria-label={`Remove question ${q.id || i + 1}`} title="Remove question">
                <Icon name="trash" size={13} />
              </button>
            </div>
            <Input value={q.instructions} onChange={e => update(i, { instructions: e.target.value })}
              placeholder="instructions — what to decide" aria-label={`Question ${i + 1} instructions`} />
            {q.type === 'choice' && (
              <Textarea value={q.criteria} onChange={e => update(i, { criteria: e.target.value })} rows={3}
                aria-label={`Question ${i + 1} choice options`}
                placeholder={'option: description (one per line)\nbilling: Payments and refunds\ntechnical: Outages and errors'} />
            )}
            {q.type === 'score' && (
              <Input value={q.criteria} onChange={e => update(i, { criteria: e.target.value })}
                aria-label={`Question ${i + 1} score levels`}
                placeholder="ordered levels, lowest first (comma separated)" />
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

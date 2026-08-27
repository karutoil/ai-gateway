import { useEffect, useRef, useState } from 'react'
import { api, buildApiUrl } from '../lib/api'
import {
  Badge, Button, Card, Field, Icon, Input, PageHeader, SegmentedControl, Select, Textarea,
} from '../components/ui'
import ModelCombobox from '../components/ModelCombobox'

export default function Playground(){
  const [mode, setMode] = useState<'openai'|'anthropic'|'responses'>('openai')
  const [gatewayKey, setGatewayKey] = useState('')
  const [availableKeys, setAvailableKeys] = useState<any[]>([])
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('Hello, what is the gateway?')
  const [system, setSystem] = useState('')
  const [stream, setStream] = useState(true)
  const [temperature, setTemperature] = useState('')
  const [maxTokens, setMaxTokens] = useState('1024')
  const [reasoningEffort, setReasoningEffort] = useState('')
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [out, setOut] = useState('')
  const [running, setRunning] = useState(false)
  const [models, setModels] = useState<any[]>([])
  const [catalogModels, setCatalogModels] = useState<string[]>([])
  const [catalogInfo, setCatalogInfo] = useState<any>(null)
  const [rawResponse, setRawResponse] = useState('')
  const [cacheStatus, setCacheStatus] = useState('')
  const [fallbackUsed, setFallbackUsed] = useState('')

  // Presentation-only: anchor the assistant pane to the newest streamed chunk.
  const convoRef = useRef<HTMLDivElement>(null)
  useEffect(()=>{
    const el = convoRef.current
    if(el) el.scrollTop = el.scrollHeight
  },[out])

  // Auth for /v1/* rides on the HttpOnly session cookie when no explicit
  // gateway key is pasted; the JWT itself is never read client-side.
  const authToken = gatewayKey.trim()
  const usingSession = !gatewayKey.trim()

  useEffect(()=>{
    api.providerModels.list().then(d=> {
      const list = d.data||[]
      setModels(list)
      if(list.length>0 && !model){
        setModel(list[0].model_id)
      }
    }).catch(()=>{})
    // Merge catalog ids (and aliases resolve through the gateway anyway) so
    // the picker works even before provider discovery has run.
    api.catalog.list(undefined, undefined, undefined, 200).then((res:any)=>{
      const arr = res?.data ?? res
      if(Array.isArray(arr)){
        const ids = arr.map((m:any)=> m?.model_id || m?.id).filter(Boolean).map(String)
        setCatalogModels(ids)
      }
    }).catch(()=>{})
    api.keys.list().then(keys=>{
      setAvailableKeys(keys||[])
    }).catch(()=>{})
  },[])

  useEffect(()=>{
    const q = model
    if(!q) return
    const found = models.find(m=> m.model_id===q || m.id===q)
    if(found){
      setCatalogInfo(found)
      return
    }
    api.catalog.get(q).then(setCatalogInfo).catch(()=> setCatalogInfo(null))
  },[model, models])

  const reasoningLevels = parseLevels(catalogInfo?.reasoning_levels)
  const supportsReasoning = !!(catalogInfo?.reasoning) && reasoningLevels.length > 0

  // Combobox options: distinct discovered model ids merged with catalog ids
  // (presentation-only mapping).
  const comboboxOptions = Array.from(new Set([
    ...models.map(m=> m.model_id).filter(Boolean),
    ...catalogModels,
  ] as string[])).sort((a,b)=> a.localeCompare(b))

  const run = async ()=>{
    setRunning(true); setOut(''); setRawResponse('')
    try {
      // Only attach a Bearer header for an explicit sk-gw- key; otherwise the
      // session cookie authenticates the gateway hop automatically.
      const headers: Record<string,string> = { 'Content-Type':'application/json' }
      if (authToken) headers['Authorization'] = `Bearer ${authToken}`
      let url = buildApiUrl('/v1/chat/completions')
      const temp = temperature === '' ? undefined : Number(temperature)
      const maxTok = maxTokens === '' ? undefined : parseInt(maxTokens, 10)
      const effort = reasoningEffort || undefined

      let body:any
      if(mode==='anthropic'){
        url = buildApiUrl('/v1/messages')
        body = { model, max_tokens: maxTok || 1024, messages:[{role:'user', content: prompt}] }
        if(stream) body.stream = true
        if(system.trim()) body.system = system.trim()
        if(temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
        if(effort) body.thinking = { type: 'enabled', effort }
      } else if(mode==='responses'){
        url = buildApiUrl('/v1/responses')
        // Honor the Stream toggle — pass the requested mode explicitly.
        body = { model, input: prompt, stream }
        if(system.trim()) body.instructions = system.trim()
        if(temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
        if(maxTok && !Number.isNaN(maxTok)) body.max_output_tokens = maxTok
        if(effort) body.reasoning = { effort }
      } else {
        const messages: any[] = []
        if(system.trim()) messages.push({role:'system', content: system.trim()})
        messages.push({role:'user', content: prompt})
        body = { model, messages, stream }
        if(temp !== undefined && !Number.isNaN(temp)) body.temperature = temp
        if(maxTok && !Number.isNaN(maxTok)) body.max_tokens = maxTok
        if(effort) body.reasoning_effort = effort
      }
      const res = await fetch(url, { method:'POST', headers, body: JSON.stringify(body), credentials:'same-origin'})
      setCacheStatus(res.headers.get('x-cache') || '')
      setFallbackUsed(res.headers.get('x-fallback-used') || '')
      setRawResponse(`Status: ${res.status} ${res.statusText}\nHeaders: ${JSON.stringify(Object.fromEntries(res.headers.entries()), null, 2)}`)
      if(!res.ok){
        const t = await res.text()
        setOut(`Error ${res.status}: ${t}`)
        setRunning(false); return
      }
      const contentType = res.headers.get('content-type') || ''
      const isSSE = contentType.includes('text/event-stream') || stream
      if(isSSE && res.body){
        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let text=''
        let all=''      // every byte received (fallback rendering)
        let pending=''  // incomplete trailing line carried across chunks
        const handleLine = (line: string)=>{
          const trimmed = line.trim()
          if(!trimmed) return
          if(trimmed.startsWith('data: ')){
            const data = trimmed.slice(6).trim()
            if(data==='[DONE]') return
            try{
              const j = JSON.parse(data)
              const delta = j.choices?.[0]?.delta?.content
              if(delta) { text += delta; setOut(text) }
              const anthDelta = j.delta?.text
              if(anthDelta) { text += anthDelta; setOut(text) }
              // Responses API streams text via response.output_text.delta events.
              if(j.type === 'response.output_text.delta' && typeof j.delta === 'string'){
                text += j.delta; setOut(text)
              }
            } catch{}
          } else if(trimmed.startsWith('{')){
            try{
              const j = JSON.parse(trimmed)
              const t = j.delta?.text || (j.type === 'response.output_text.delta' && typeof j.delta === 'string' ? j.delta : '') || j.output_text || j.text || j.content?.[0]?.text
              if(t) { text += t; setOut(text) }
            } catch{}
          }
        }
        while(true){
          const {done,value} = await reader.read()
          if(done) break
          const chunk = decoder.decode(value, {stream:true})
          all += chunk
          // Split the accumulated buffer, not the raw chunk: JSON lines that
          // straddle network reads stay intact because an incomplete trailing
          // line is carried over to the next chunk.
          pending += chunk
          const nl = pending.lastIndexOf('\n')
          if(nl === -1) continue
          const complete = pending.slice(0, nl + 1)
          pending = pending.slice(nl + 1)
          complete.split('\n').forEach(handleLine)
        }
        // Flush any final line that arrived without a trailing newline.
        if(pending.trim()) handleLine(pending)
        if(!text){
          const buffer = all
          try{
            const j = JSON.parse(buffer)
            if(j.output_text){
              setOut(j.output_text)
            } else if(j.content && Array.isArray(j.content)){
              const txt = j.content.map((c:any)=> c.text || '').join('')
              if(txt) setOut(txt)
              else setOut(JSON.stringify(j, null, 2))
            } else if(j.choices){
              setOut(JSON.stringify(j, null, 2))
            } else {
              setOut(buffer.slice(0,2000))
            }
          } catch{
            const texts: string[] = []
            buffer.split('\n').forEach(line=>{
              if(line.includes('"text"')){
                try{
                  const j = JSON.parse(line.slice(line.indexOf('{')))
                  if(j.delta?.text) texts.push(j.delta.text)
                  else if(j.content_block?.text) texts.push(j.content_block.text)
                } catch{}
              }
            })
            if(texts.length) setOut(texts.join(''))
            else setOut(buffer.slice(0,3000))
          }
        }
        if(!text && all.includes('thinking')){
          setOut(all.slice(0,4000))
        }
      } else {
        const j = await res.json()
        setOut(JSON.stringify(j, null, 2))
      }
    } catch(e:any){ setOut(String(e?.message || e))}
    setRunning(false)
  }

  /** Caption row shared by settings controls. */
  const captionCls = 'block text-xs font-medium text-muted mb-1.5 uppercase tracking-wide'

  return (
    <div className="space-y-6">
      <PageHeader
        title="Playground"
        description="Send live requests through the gateway across OpenAI, Anthropic, and Responses APIs."
      />

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 items-start">
        {/* Left pane: request settings */}
        <div className="lg:col-span-1 space-y-4">
          <Card>
            <div>
              <span className={captionCls}>API</span>
              <SegmentedControl<'openai'|'anthropic'|'responses'>
                options={[
                  { value:'openai', label:'openai' },
                  { value:'anthropic', label:'anthropic' },
                  { value:'responses', label:'responses' },
                ]}
                value={mode}
                onChange={setMode}
              />
            </div>

            <div className="mt-4">
              <span className={captionCls}>Model</span>
              <ModelCombobox
                value={model ? [model] : []}
                onChange={(next)=> setModel(next.length ? next[next.length-1] : '')}
                options={comboboxOptions}
                placeholder={models.length ? models[0]?.model_id : 'muse-spark-1.2-contributor'}
              />
              {models.length===0 && (
                <div className="mt-1.5 flex items-center gap-1.5 text-xs text-amber">
                  <Icon name="alert" size={12}/> No models discovered. Go to Models, then Discover.
                </div>
              )}
            </div>

            {catalogInfo && (
              <div className="mt-3 rounded-lg border border-stone bg-app/50 p-3 text-xs space-y-1.5">
                <div className="font-semibold text-sm">{catalogInfo.display_name || catalogInfo.model_id || catalogInfo.name || catalogInfo.id}</div>
                <div className="font-mono text-xs text-muted truncate">{catalogInfo.model_id || catalogInfo.id} · {catalogInfo.provider_name || catalogInfo.provider}</div>
                <div className="flex gap-1.5 flex-wrap pt-0.5">
                  {catalogInfo.context_window && <span className="font-mono tabular-nums border border-stone rounded-full px-2 py-0.5">ctx {(catalogInfo.context_window/1000).toFixed(0)}k</span>}
                  {catalogInfo.max_output && <span className="font-mono tabular-nums border border-stone rounded-full px-2 py-0.5">out {(catalogInfo.max_output/1000).toFixed(0)}k</span>}
                  {catalogInfo.reasoning && <Badge tone="warn">reasoning: {catalogInfo.reasoning_type||'effort'} {reasoningLevels.join('/')}</Badge>}
                  {(catalogInfo.input_cost||catalogInfo.output_cost) && <span className="font-mono tabular-nums border border-teal/30 text-teal rounded-full px-2 py-0.5">${catalogInfo.input_cost}/${catalogInfo.output_cost}/1M</span>}
                </div>
              </div>
            )}

            <div className="mt-4">
              <span className={captionCls}>Stream</span>
              <SegmentedControl<'on'|'off'>
                options={[{ value:'on', label:'On' }, { value:'off', label:'Off' }]}
                value={stream ? 'on' : 'off'}
                onChange={(v)=> setStream(v==='on')}
              />
            </div>

            <div className="mt-4 grid grid-cols-2 gap-2">
              <Field label="Temperature"><Input value={temperature} onChange={e=>setTemperature(e.target.value)} placeholder="default" inputMode="decimal" className="font-mono"/></Field>
              <Field label="Max tokens"><Input value={maxTokens} onChange={e=>setMaxTokens(e.target.value)} placeholder="1024" inputMode="numeric" className="font-mono"/></Field>
            </div>

            <Button variant="primary" onClick={run} disabled={running || !model} className="w-full mt-4">
              <Icon name="play" size={14}/> {running ? 'Streaming' : 'Send via Gateway'}
            </Button>
            {!model && <div className="mt-1.5 text-xs text-amber">Select a model.</div>}

            {/* Telemetry strip — status dot + real response headers only. */}
            <div className="mt-3 rounded-lg border border-stone bg-app/50 px-2.5 py-2 flex items-center gap-2 font-mono text-xs">
              <span className={`w-2 h-2 rounded-full shrink-0 ${running ? 'bg-teal animate-pulse-soft shadow-glow' : 'bg-stone'}`} />
              <span className="text-muted shrink-0">{running ? 'streaming' : 'idle'}</span>
              {cacheStatus && (cacheStatus==='HIT'
                ? <Badge tone="good">cached</Badge>
                : <Badge tone="neutral">live</Badge>)}
              {fallbackUsed && <Badge tone="warn">fallback {fallbackUsed}</Badge>}
            </div>
            {rawResponse && (
              <details className="mt-2 text-xs font-mono text-muted">
                <summary className="cursor-pointer hover:text-paper transition-colors">Debug headers</summary>
                <pre className="whitespace-pre-wrap break-all mt-1">{rawResponse}</pre>
              </details>
            )}
          </Card>

          <Card>
            <Field label="System prompt">
              <Textarea value={system} onChange={e=>setSystem(e.target.value)} rows={4} placeholder="optional system / instructions"/>
            </Field>

            <button
              type="button"
              onClick={()=>setShowAdvanced(v=>!v)}
              aria-expanded={showAdvanced}
              className="mt-3 inline-flex items-center gap-1.5 text-xs font-medium text-muted hover:text-paper transition-colors focus-visible:outline-none"
            >
              <Icon name="chevronDown" size={13} className={`transition-transform duration-150 ${showAdvanced ? 'rotate-180' : ''}`}/>
              Advanced
            </button>

            {showAdvanced && (
              <div className="mt-3 space-y-3 rounded-lg border border-stone bg-app/40 p-3">
                <Field label="Gateway key" hint="Overrides session auth for this request only.">
                  <Input value={gatewayKey} onChange={e=>setGatewayKey(e.target.value)}
                    placeholder={usingSession ? 'optional — using admin session' : 'sk-gw-... (create in API Keys tab)'}/>
                </Field>
                {usingSession && (
                  <div className="-mt-2 text-xs text-teal font-mono">Using logged-in session cookie for /v1/*</div>
                )}
                {availableKeys.length>0 && (
                  <div className="-mt-1 flex flex-wrap gap-1 items-center">
                    {availableKeys.slice(0,3).map((k:any)=> (
                      <span key={k.id} className="text-xs font-mono text-muted border border-stone px-2 py-0.5 rounded-full">..{k.prefix}</span>
                    ))}
                    <span className="text-xs text-muted">copy from Keys tab to impersonate a key</span>
                  </div>
                )}
                <Field label="Reasoning effort">
                  <Select value={reasoningEffort} onChange={e=>setReasoningEffort(e.target.value)}>
                    <option value="">off / default</option>
                    {(supportsReasoning ? reasoningLevels : ['low','medium','high']).map(lv=> (
                      <option key={lv} value={lv}>{lv}</option>
                    ))}
                  </Select>
                </Field>
                {catalogInfo && !catalogInfo.reasoning && reasoningEffort && (
                  <div className="-mt-2 flex items-center gap-1.5 text-[11px] text-amber">
                    <Icon name="alert" size={11}/> This model may reject reasoning_effort.
                  </div>
                )}
              </div>
            )}
          </Card>
        </div>

        {/* Right pane: conversation */}
        <Card pad={false} className="lg:col-span-2 flex flex-col lg:h-[680px] overflow-hidden">
          <div className="px-4 py-3 border-b border-stone flex items-center justify-between gap-2">
            <span className="text-xs font-semibold uppercase tracking-wider text-muted">Conversation</span>
            <div className="flex items-center gap-1.5">
              <Badge tone="neutral">{mode}</Badge>
              <Badge tone={stream ? 'good' : 'neutral'} dot={stream}>{stream ? 'stream on' : 'stream off'}</Badge>
            </div>
          </div>

          <div ref={convoRef} className="flex-1 min-h-[380px] overflow-y-auto p-4 space-y-4">
            {prompt.trim() !== '' && (
              <div className="flex justify-end">
                <div className="ml-auto max-w-[85%] bg-raised rounded-xl px-3 py-2 text-sm whitespace-pre-wrap break-words">{prompt}</div>
              </div>
            )}
            <div className="max-w-full">
              <div className="mr-auto border-l-2 border-teal/70 bg-app/50 rounded-r-xl px-3 py-2 font-mono text-sm whitespace-pre-wrap break-words min-h-[42px]">
                {out || <span className="text-muted">Output will appear here.</span>}
              </div>
            </div>
          </div>

          <div className="border-t border-stone p-3 flex items-center gap-2">
            <Input
              value={prompt}
              onChange={e=>setPrompt(e.target.value)}
              placeholder="Your prompt"
              className="flex-1"
              disabled={running}
            />
            <Button variant="primary" onClick={run} disabled={running || !model} title="Send via Gateway">
              <Icon name="play" size={14}/> Send
            </Button>
          </div>
        </Card>
      </div>
    </div>
  )
}

function parseLevels(s?: string): string[] {
  if(!s) return []
  try { const v=JSON.parse(s); return Array.isArray(v)? v: [] } catch { return s.split(',').map(x=>x.trim()).filter(Boolean) }
}

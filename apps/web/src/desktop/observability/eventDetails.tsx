import type { ApiAgentEvent } from '@/api/client'
import { useT } from '@/lib/i18n'
import { elapsed } from './shared'

function pretty(value: unknown): string {
  try {
    return JSON.stringify(value ?? {}, null, 2)
  } catch {
    return String(value)
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value)
}

function asTraceText(value: unknown): { text: string; length?: number; truncated?: boolean } | null {
  if (typeof value === 'string') return { text: value }
  if (!isRecord(value) || typeof value.text !== 'string') return null
  return {
    text: value.text,
    length: typeof value.length === 'number' ? value.length : undefined,
    truncated: Boolean(value.truncated),
  }
}

function MetaGrid({ items }: { items: Array<{ label: string; value: unknown }> }) {
  return (
    <div className="grid grid-cols-2 gap-2 md:grid-cols-4">
      {items.filter((item) => item.value !== undefined && item.value !== null && item.value !== '').map((item) => (
        <div key={item.label} className="rounded-[9px] border border-ink-100 bg-paper px-3 py-2">
          <div className="text-[9.5px] font-bold uppercase tracking-[0.12em] text-ink-300">{item.label}</div>
          <div className="mt-1 truncate font-mono text-[11.5px] text-ink-800">{String(item.value)}</div>
        </div>
      ))}
    </div>
  )
}

function PayloadBlock({ label, value, empty }: { label: string; value: unknown; empty?: string }) {
  const t = useT()
  const traced = asTraceText(value)
  const text = traced ? traced.text : typeof value === 'undefined' ? '' : pretty(value)
  const emptyText = empty ?? t('obs.payloadEmpty')
  return (
    <div>
      <div className="mb-1 flex items-center justify-between gap-3">
        <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-400">{label}</div>
        {traced?.length !== undefined && (
          <div className="font-mono text-[10px] text-ink-300">
            {t('obs.payloadChars', { n: traced.length })}{traced.truncated ? t('obs.payloadTruncated') : ''}
          </div>
        )}
      </div>
      <pre className="max-h-[360px] overflow-auto rounded-[10px] border border-ink-100 bg-ink-900 p-3 text-[11px] leading-[1.55] text-sky2-50">
        {text || emptyText}
      </pre>
    </div>
  )
}

function TraceInputView({ value }: { value: unknown }) {
  const t = useT()
  if (!Array.isArray(value)) return <PayloadBlock label={t('obs.labelInput')} value={value} />
  return (
    <div className="space-y-2">
      <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-400">{t('obs.labelInputMessages')}</div>
      {value.map((item, idx) => {
        const rec = isRecord(item) ? item : {}
        const content = Array.isArray(rec.content) ? rec.content : []
        return (
          <div key={idx} className="rounded-[10px] border border-ink-100 bg-paper p-3">
            <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-ink-400">
              <span>#{idx + 1}</span>
              {rec.role !== undefined && <span>role={String(rec.role)}</span>}
              {rec.type !== undefined && <span>type={String(rec.type)}</span>}
              {rec.callId !== undefined && <span>call={String(rec.callId)}</span>}
            </div>
            <div className="space-y-2">
              {rec.output !== undefined && <PayloadBlock label={t('obs.labelFunctionOutput')} value={rec.output} />}
              {content.map((part, partIdx) => {
                const p = isRecord(part) ? part : {}
                const type = String(p.type ?? `part ${partIdx + 1}`)
                if (p.text !== undefined) return <PayloadBlock key={partIdx} label={type} value={p.text} />
                if (type === 'input_image') {
                  return (
                    <div key={partIdx} className="rounded-[9px] border border-sky2-100 bg-sky2-50 px-3 py-2 font-mono text-[11px] text-skype-deep">
                      {t('obs.imageAuto', { detail: String(p.detail ?? t('obs.imageAutoDefault')), url: String(p.imageUrl ?? '') })}
                    </div>
                  )
                }
                return <PayloadBlock key={partIdx} label={type} value={p} />
              })}
              {content.length === 0 && rec.content !== undefined && <PayloadBlock label={t('obs.labelContent')} value={rec.content} />}
            </div>
          </div>
        )
      })}
    </div>
  )
}

function ModelRequestDetails({ data }: { data: Record<string, unknown> }) {
  const t = useT()
  const request = isRecord(data.request) ? data.request : {}
  return (
    <div className="mt-3 space-y-3">
      <MetaGrid items={[
        { label: t('obs.metaModel'), value: data.model },
        { label: t('obs.metaHop'), value: data.hop },
        { label: t('obs.metaPrevious'), value: data.previousResponseId },
        { label: t('obs.metaMaxTokens'), value: request.maxOutputTokens },
      ]} />
      <PayloadBlock label={t('obs.labelInstructions')} value={data.instructions} />
      <TraceInputView value={data.input} />
    </div>
  )
}

function ModelResponseDetails({ data }: { data: Record<string, unknown> }) {
  const t = useT()
  const usage = isRecord(data.usage) ? data.usage : {}
  const toolCalls = Array.isArray(data.toolCalls) ? data.toolCalls : []
  return (
    <div className="mt-3 space-y-3">
      <MetaGrid items={[
        { label: t('obs.metaModel'), value: data.model },
        { label: t('obs.metaHop'), value: data.hop },
        { label: t('obs.metaResponse'), value: data.responseId },
        { label: t('obs.metaStatus'), value: data.status },
        { label: t('obs.metaInputTokens'), value: usage.input_tokens },
        { label: t('obs.metaOutputTokens'), value: usage.output_tokens },
      ]} />
      <PayloadBlock label={t('obs.labelOutputText')} value={data.outputText} empty={t('obs.noAssistantText')} />
      {toolCalls.length > 0 && (
        <div className="space-y-2">
          <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-400">{t('obs.labelToolCalls')}</div>
          {toolCalls.map((call, idx) => {
            const rec = isRecord(call) ? call : {}
            return (
              <div key={idx} className="rounded-[10px] border border-ink-100 bg-paper p-3">
                <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10.5px] text-ink-400">
                  <span>{String(rec.name ?? t('obs.toolDefault'))}</span>
                  {rec.callId !== undefined && <span>{String(rec.callId)}</span>}
                </div>
                <PayloadBlock label={t('obs.labelArguments')} value={rec.arguments ?? rec.rawArguments} />
              </div>
            )
          })}
        </div>
      )}
      <PayloadBlock label={t('obs.labelResponseOutputItems')} value={data.output} />
    </div>
  )
}

function ToolDetails({ data }: { data: Record<string, unknown> }) {
  const t = useT()
  return (
    <div className="mt-3 space-y-3">
      <MetaGrid items={[
        { label: t('obs.metaHop'), value: data.hop },
        { label: t('obs.metaCall'), value: data.callId },
        { label: t('obs.metaDuration'), value: typeof data.durationMs === 'number' ? elapsed(data.durationMs) : undefined },
        { label: t('obs.metaOk'), value: data.ok },
      ]} />
      {data.args !== undefined && <PayloadBlock label={t('obs.labelArguments')} value={data.args} />}
      {data.output !== undefined && <PayloadBlock label={t('obs.labelOutput')} value={data.output} />}
      {data.error !== undefined && data.error !== null && <PayloadBlock label={t('obs.labelError')} value={data.error} />}
    </div>
  )
}

export function EventDetails({ event }: { event: ApiAgentEvent }) {
  const t = useT()
  const data = event.data ?? {}
  const specialized = event.kind === 'model.request'
    ? <ModelRequestDetails data={data} />
    : event.kind === 'model.response'
      ? <ModelResponseDetails data={data} />
      : event.kind.startsWith('tool.')
        ? <ToolDetails data={data} />
        : null
  const defaultOpen = event.kind === 'model.request' || event.kind === 'model.response' || event.kind.startsWith('tool.')

  return (
    <div>
      {specialized}
      <details className="mt-3" open={!specialized && defaultOpen}>
        <summary className="cursor-pointer select-none text-[11px] font-semibold text-skype-deep">{t('obs.labelRawData')}</summary>
        <pre className="mt-2 max-h-[260px] overflow-auto rounded-[9px] bg-ink-900 p-3 text-[11px] leading-[1.45] text-sky2-50">
          {pretty(data)}
        </pre>
      </details>
    </div>
  )
}

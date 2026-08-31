/** 观测页(#219 ①)共享词表 —— 目的色板 / 单位与来源筛选 / i18n 助手。
 *  页面壳与 ./observability/ 下各卡片/表格件共同消费;块体自
 *  ObservabilityPage.tsx 原样搬移(#147 ② 的 format 层仍在 @/lib/format)。 */

import {
  fmtTokens,
  fmtUsdCompact as fmtUsd,
} from '@/lib/format'
import { type MessageKey, tLabel, type useT } from '@/lib/i18n'
import type { LlmCallPurpose } from '../api'

// ─── Purpose → label + swatch ────────────────────────────────────────────
//
// One source of truth for every purpose-keyed visual: a human label (short
// enough for chart legends), a one-line explainer (for the rollup table's
// secondary line + tooltip), and a swatch color used by both the chart and
// the table dot. Colors are sampled from the existing palette — sky for the
// big-model real-task tier, coral for cerebellum gates, gold for tools /
// one-shots — so the page reads as a member of the family.

interface PurposeMeta {
  label: string
  blurb: string
  swatch: string
}

const PURPOSE_META: Record<LlmCallPurpose, PurposeMeta> = {
  // Big-model real tasks — sky family (these are the sanctioned spend).
  'agent-turn':           { label: 'Agent turn',         blurb: "Main reasoning hop per turn — the agent's actual reply work.", swatch: '#0078C8' },
  'convene-speech':       { label: 'Convene speech',     blurb: 'One agent speaking inside a live convene session.',           swatch: '#4FC2F4' },
  // Cerebellum gates — coral family (these shield the big brain).
  'inbox-triage':         { label: 'Inbox triage',       blurb: 'Cloud gate that decides whether a human message wakes the brain.', swatch: '#FF7A6B' },
  'synthetic-wake-gate':  { label: 'Synthetic-wake gate',blurb: 'Gate for idle / background-scan / poll-update wakes.',         swatch: '#FFA39A' },
  'agenda':               { label: 'Agenda check',       blurb: "Heartbeat classifier on cards / events / stalls.",            swatch: '#C84E3F' },
  // Mid-turn cerebellum — wisteria family (auxiliary judgment on big-turn work).
  'compaction':           { label: 'Auto-compaction',    blurb: 'Summarizes earlier tool work when history nears the context cap.', swatch: '#9B7BD4' },
  'completion-verify':    { label: 'Completion verify',  blurb: 'Re-checks an agent’s terminal status against side effects.',   swatch: '#7A5BB8' },
  'steer-summary':        { label: 'Steer summary',      blurb: 'Digests mid-turn messages so the agent can react without history blow-up.', swatch: '#B898DD' },
  'convene-decision':     { label: 'Convene decision',   blurb: '"Did the convene reach a decision?" closer.',                 swatch: '#D4B3FF' },
  // One-shot utilities — gold family.
  'palette':              { label: 'Palette tool',       blurb: 'Agent-invoked 5-color palette generator.',                    swatch: '#F4B740' },
  'gender':               { label: 'Gender inference',   blurb: 'Avatar-pipeline gender pick.',                                swatch: '#BA8418' },
  'avatar-image':         { label: 'Avatar image',       blurb: 'Image gen at agent creation / regeneration.',                 swatch: '#D49520' },
  'agent-image':          { label: 'Agent image tool',   blurb: 'Image gen invoked by an agent mid-turn.',                     swatch: '#E8A030' },
}

// i18n: parallel lookup tables for the 12 LlmCallPurpose values. The
// original PURPOSE_META keeps English text as the fallback so existing
// product copy keeps rendering even if a key is missing. These tables
// just give t() a MessageKey to resolve; the underlying data shape
// (label/blurb/swatch) is untouched, so author edits to PURPOSE_META
// stay in sync automatically.
const PURPOSE_LABEL_KEY: Record<LlmCallPurpose, MessageKey> = {
  'agent-turn':           'adminobs.purposeAgentTurn',
  'convene-speech':       'adminobs.purposeConveneSpeech',
  'inbox-triage':         'adminobs.purposeInboxTriage',
  'synthetic-wake-gate':  'adminobs.purposeSyntheticWakeGate',
  'agenda':               'adminobs.purposeAgenda',
  'compaction':           'adminobs.purposeCompaction',
  'completion-verify':    'adminobs.purposeCompletionVerify',
  'steer-summary':        'adminobs.purposeSteerSummary',
  'convene-decision':     'adminobs.purposeConveneDecision',
  'palette':              'adminobs.purposePalette',
  'gender':               'adminobs.purposeGender',
  'avatar-image':         'adminobs.purposeAvatarImage',
  'agent-image':          'adminobs.purposeAgentImage',
}
const PURPOSE_BLURB_KEY: Record<LlmCallPurpose, MessageKey> = {
  'agent-turn':           'adminobs.purposeAgentTurnBlurb',
  'convene-speech':       'adminobs.purposeConveneSpeechBlurb',
  'inbox-triage':         'adminobs.purposeInboxTriageBlurb',
  'synthetic-wake-gate':  'adminobs.purposeSyntheticWakeGateBlurb',
  'agenda':               'adminobs.purposeAgendaBlurb',
  'compaction':           'adminobs.purposeCompactionBlurb',
  'completion-verify':    'adminobs.purposeCompletionVerifyBlurb',
  'steer-summary':        'adminobs.purposeSteerSummaryBlurb',
  'convene-decision':     'adminobs.purposeConveneDecisionBlurb',
  'palette':              'adminobs.purposePaletteBlurb',
  'gender':               'adminobs.purposeGenderBlurb',
  'avatar-image':         'adminobs.purposeAvatarImageBlurb',
  'agent-image':          'adminobs.purposeAgentImageBlurb',
}

const FALLBACK_SWATCH = '#94A8BC'
export function metaFor(p: LlmCallPurpose | string): PurposeMeta {
  const m = (PURPOSE_META as Record<string, PurposeMeta | undefined>)[p]
  return m ?? { label: p, blurb: '', swatch: FALLBACK_SWATCH }
}

// ─── Formatters(#147 ②:与 desktop/ObservabilityView 共享 @/lib/format,
// 本页用紧凑 $ 档位语体;import 重命名维持百处调用点零改动) ─────────

// ─── Unit: $ vs tokens ───────────────────────────────────────────────────
//
// The $ figures are ESTIMATES — cost.ts seeds one assumed per-token rate per
// model (verified:false unless an operator supplies CUMORA_MODEL_PRICES_JSON).
// For BYOA every user is on their own provider/plan, so the $ is meaningless to
// them; token counts are the objective truth. This toggle swaps every primary
// metric between $ and tokens. Default = tokens. Cached input tokens are ALWAYS
// shown distinctly from uncached (they bill ~10× less and behave differently),
// regardless of unit.
export type Unit = 'tokens' | 'usd'
export const UNITS: Array<{ key: Unit; label: string }> = [
  { key: 'tokens', label: 'Tokens' },
  { key: 'usd',    label: 'USD $' },
]
/** Total billable tokens across every bucket of a row, cached input included
 *  (cached tokens are still tokens the model processed). reasoning is optional
 *  — only the rollup / raw-call shapes carry it. */
export const totalTokens = (r: { inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningTokens?: number }): number =>
  r.inputTokens + r.cachedInputTokens + r.outputTokens + (r.reasoningTokens ?? 0)
/** One primary metric formatted per the active unit. */
export const fmtAmount = (unit: Unit, usd: number, tokens: number): string =>
  unit === 'usd' ? fmtUsd(usd, usd < 1 ? 4 : 2) : fmtTokens(tokens)

// ─── Source filter ───────────────────────────────────────────────────────
//
// The ledger now records BOTH cloud sub2api AND BYOA local spend (the daemon
// emits per-hop trajectory via /runtime/llm-calls). Cloud rows are real $;
// BYOA rows are meter-equivalent — same per-token rate, but the operator's
// actual bill is a flat subscription. Default is "All", but the explicit
// filter is here so the operator can isolate either side when investigating
// a spike: "did cloud cost spike, or just BYOA token usage?"

// One BYOA pill covering every local engine. The rollup
// table still shows them as separate rows (different engine/model); this is
// just the filter — the operator rarely wants to isolate a single BYOA engine,
// and "Cloud vs BYOA" is the split that matters.
export type SourceFilter = 'all' | 'cloud' | 'byoa'
export const SOURCE_FILTERS: Array<{ key: SourceFilter; label: string }> = [
  { key: 'all',   label: 'All' },
  { key: 'cloud', label: 'Cloud' },
  { key: 'byoa',  label: 'BYOA' },
]
// i18n lookup by SourceFilter key.
export const SOURCE_LABEL_KEY: Record<SourceFilter, MessageKey> = {
  'all':   'adminobs.sourceAll',
  'cloud': 'adminobs.sourceCloud',
  'byoa':  'adminobs.sourceByoa',
}
export const isByoaSource = (s: string): boolean => s.startsWith('byoa-')

// i18n helpers — shared by the page and its module-scope card/table
// components. `tLabel` prefers the translated key but always falls back to
// the inline English source-of-truth, so upstream copy edits keep rendering
// even before a translation lands.
type TFn = ReturnType<typeof useT>
export const purposeLabel = (t: TFn, p: LlmCallPurpose | string): string =>
  tLabel(t, (PURPOSE_LABEL_KEY as Partial<Record<string, MessageKey>>)[p], metaFor(p).label)
export const purposeBlurb = (t: TFn, p: LlmCallPurpose | string): string =>
  tLabel(t, (PURPOSE_BLURB_KEY as Partial<Record<string, MessageKey>>)[p], metaFor(p).blurb)

/** Color-class for the hit-rate value: coral when bad, gold when mid, sky-
 *  deep when good. Same thresholds the per-purpose bars use, so the hero +
 *  bars stay visually consistent. */
export function cacheToneClass(hitRate: number | null | undefined): string {
  if (hitRate == null) return ''
  if (hitRate >= 0.7) return 'obs-cache-tone-good'
  if (hitRate >= 0.3) return 'obs-cache-tone-mid'
  return 'obs-cache-tone-bad'
}

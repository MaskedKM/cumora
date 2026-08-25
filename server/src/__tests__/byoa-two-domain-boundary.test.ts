/**
 * Content assertion for the BYOA two-domain privacy boundary (#32): for
 * local engines the prompt IS the boundary — no OS sandbox exists — so the
 * persona header's wording is the enforcement surface and gets pinned
 * here. Two domains per Q9 of the workspace concept consensus: the
 * agent's private home + team workspaces it is a member of (full work
 * rights inside, including execution); everything else stays untouchable.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PERSONA_HEADER } from '../agents/computer/engine.js'

const header = PERSONA_HEADER({ id: 'nova', name: 'Nova', role: 'PM', systemPrompt: '' })

test('boundary is two-domain: private home + member team workspaces', () => {
  assert.match(header, /TWO domains/)
  assert.match(header, /Your private home/)
  assert.match(header, /Team workspaces you are a member of/)
})

test('team workspace grants full work rights, incl. execution', () => {
  assert.match(header, /cumora workspace ls/)
  assert.match(header, /full work\s+rights: read and write files, run builds, tests, and git/)
})

test('outside the two domains is still strictly off-limits', () => {
  assert.match(header, /Everything else on the machine [\s\S]*is private and not yours to touch/)
  assert.match(header, /~\/\.ssh/)
  assert.match(header, /Do not read, open, list, or search anything outside those two domains/)
  assert.match(header, /NEVER paste, quote, summarize, or send [\s\S]*outside your two domains/)
})

test('private scratch vs team workspace vocabulary stays unambiguous', () => {
  assert.match(header, /workspace\/` — private scratch .* NOT the team workspace/s)
  assert.match(header, /Your own files stay under `cumora ws`/)
})

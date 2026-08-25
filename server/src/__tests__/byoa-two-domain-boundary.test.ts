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
import { TWO_DOMAIN_PRIVACY_RULE } from '../agents/computer/daemon.js'

const header = PERSONA_HEADER({ id: 'nova', name: 'Nova', role: 'PM', systemPrompt: '' })

test('boundary is two-domain: private home + member team workspaces', () => {
  assert.match(header, /may operate in exactly\s+TWO domains/)
  assert.match(header, /Your private home/)
  assert.match(header, /Team workspaces you are a member of/)
})

test('execution rights inside a workspace come with the ask-for-the-path clause', () => {
  assert.match(header, /full work\s+rights: read and write files, run builds, tests, and git/)
  assert.match(header, /ask the operator for its folder path first/)
})

test('standing prompt (system channel) states the same two-domain rule', () => {
  assert.match(TWO_DOMAIN_PRIVACY_RULE, /operate only inside your private home and the team workspaces you are a member of/)
  assert.match(TWO_DOMAIN_PRIVACY_RULE, /cumora workspace ls/)
  assert.match(TWO_DOMAIN_PRIVACY_RULE, /never read or expose them/)
})

test('the old one-domain wording is gone from both prompt channels', () => {
  assert.doesNotMatch(header, /stay inside your home directory/i)
  assert.doesNotMatch(TWO_DOMAIN_PRIVACY_RULE, /stay inside your home directory/i)
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

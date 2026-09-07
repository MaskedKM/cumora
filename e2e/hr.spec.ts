/**
 * #346 HR Agent 评估全链 e2e:配对 → HR 指派 → 触发评估 → echo "Brain"
 * (消费 wake brief → CLI 拉输入 → run 生命周期 → CLI 交报告)→ HR 页看到
 * done 轮次与报告载荷;顺带断言 device 面三件真事:
 *   ① roster(/computers/me/agents)含 hr-<companyId> 虚拟行(UNION 生效)
 *   ② device 面可为 hr-<companyId> 铸 runtime JWT(MintAgentRuntimeToken 分支)
 *   ③ 评估 run 以 hr-<companyId> 归因落 agent_runs(观测归因键)
 * 全程生产路径;SUT/vite preview 由 tests/integration/run.mjs 的
 * INTEGRATION_E2E 形态自起(smoke.spec 同款契约)。
 */
import { test, expect } from '@playwright/test'
import { createHash } from 'node:crypto'
import { Client } from 'pg'

const API = process.env.CUMORA_E2E_API_BASE ?? ''
const WEB = process.env.CUMORA_E2E_WEB_BASE ?? ''
test.skip(!API || !WEB, 'CUMORA_E2E_API_BASE/WEB_BASE 未注入(须由 INTEGRATION_E2E=1 runner 起跑)')
// 文案断言锚定 en(评审 P2:默认 locale 恰为 en-US 才成立,显式钉死)
test.use({ locale: 'en-US' })

const USER = 'u-e2e-hr'
const COMPANY = 'c-e2e-hr'
const SESSION_TOKEN = `e2e-session-hr-${Date.now()}`
const HR_AGENT_ID = `hr-${COMPANY}`
const SCORE_TEXT = `e2e-评估结论:${Date.now()}`

async function apiCall(path: string, init: RequestInit = {}, extraHeaders: Record<string, string> = {}) {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: {
      'content-type': 'application/json',
      'x-test-user': USER,
      'x-company-id': COMPANY,
      ...extraHeaders,
      ...(init.headers as Record<string, string> | undefined),
    },
  })
  if (!res.ok) throw new Error(`${init.method ?? 'GET'} ${path} → ${res.status}: ${(await res.text()).slice(0, 300)}`)
  return res.json() as Promise<Record<string, unknown>>
}

/** HR echo "Brain":daemon 形最小合成 —— 收到带 backgroundBrief 的 wake 后:
 * 拉输入(hr context)→ 起 run → 交报告(hr report)→ 收 run。走的全是
 * 真实 daemon 的生产面(runtime JWT 鉴权)。 */
function startHrEchoRuntime(bearer: string): { stop: () => void; started: Promise<void> } {
  const ac = new AbortController()
  let startedResolve!: () => void
  const started = new Promise<void>((r) => { startedResolve = r })
  void (async () => {
    try {
      const res = await fetch(`${API}/runtime/wake-stream`, {
        headers: { authorization: `Bearer ${bearer}` },
        signal: ac.signal,
      })
      console.error(`[hr-echo] wake-stream → ${res.status}`)
      if (!res.ok || !res.body) throw new Error(`wake-stream ${res.status}`)
      startedResolve()
      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        let idx: number
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, idx)
          buf = buf.slice(idx + 2)
          const ev = /^event: (.+)$/m.exec(block)?.[1]
          const data = /^data: (.+)$/m.exec(block)?.[1]
          if (ev !== 'wake' || !data) continue
          const frame = JSON.parse(data) as {
            backgroundBrief?: { source: string; ref?: string }
          }
          const brief = frame.backgroundBrief
          if (!brief || brief.source !== 'hr-eval' || !brief.ref) continue
          console.error(`[hr-echo] hr-eval wake ref=${brief.ref}`)
          // ① 拉输入快照(daemon Brain 的第一步)
          const ctx = await fetch(`${API}/runtime/cli`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
            body: JSON.stringify({ argv: ['hr', 'context', brief.ref] }),
          })
          const ctxBody = await ctx.json() as { ok: boolean; text: string }
          console.error(`[hr-echo] context ok=${ctxBody.ok} bytes=${ctxBody.text?.length ?? 0}`)
          // ② 起 run(daemon 形;归因键=JWT sub=hr-<companyId>)
          const run = await fetch(`${API}/runtime/runs`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
            body: JSON.stringify({
              trigger: { source: 'byoa', engine: 'echo', reason: 'hr-eval' },
              inboxCount: 0,
            }),
          })
          const runBody = await run.json() as { runId?: string }
          console.error(`[hr-echo] run → ${run.status}: ${JSON.stringify(runBody).slice(0, 160)}`)
          // ③ 交报告(结构化载荷;输入快照真实可读即引用之)
          const report = {
            summary: SCORE_TEXT,
            rounds: [{ score: 4, findings: ['inputs readable'], inputsBytes: ctxBody.text?.length ?? 0 }],
          }
          const rep = await fetch(`${API}/runtime/cli`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
            body: JSON.stringify({ argv: ['hr', 'report', brief.ref, JSON.stringify(report)] }),
          })
          console.error(`[hr-echo] report → ${rep.status}`)
          // ④ 收 run(daemon 形)
          if (runBody.runId) {
            const fin = await fetch(`${API}/runtime/runs/${runBody.runId}/finish`, {
              method: 'POST',
              headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
              body: JSON.stringify({ status: 'completed', summary: 'hr echo round' }),
            })
            console.error(`[hr-echo] finish → ${fin.status}`)
          }
        }
      }
    } catch (e) {
      console.error(`[hr-echo] exited: ${e instanceof Error ? e.message : String(e)}`)
    }
  })()
  return { stop: () => ac.abort(), started }
}

test('hr: 触发评估 → echo Brain 交报告 → HR 页看到 done 轮次', async ({ page }) => {
  test.setTimeout(180_000)

  // ── 1) 种登录态(owner) ──
  const pg = new Client({ connectionString: process.env.DATABASE_URL })
  await pg.connect()
  const tokenHash = createHash('sha256').update(SESSION_TOKEN).digest('base64url')
  for (const q of [
    `DELETE FROM sessions WHERE user_id = '${USER}'`,
    `DELETE FROM companies WHERE id = '${COMPANY}'`,
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('${COMPANY}', 'E2E HR Co', 'e2e-hr', '${USER}')`,
    `INSERT INTO users (id, email, display_name, tier) VALUES ('${USER}', '${USER}@test.local', 'HR Human', 'free') ON CONFLICT (id) DO NOTHING`,
    `INSERT INTO company_members (company_id, user_id, role) VALUES ('${COMPANY}', '${USER}', 'owner') ON CONFLICT DO NOTHING`,
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status) VALUES ('${USER}', '${COMPANY}', 'human', 'HR Human', 'owner', 'H', '#abcdef', 'avail') ON CONFLICT DO NOTHING`,
    `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ('${tokenHash}', '${USER}', NOW() + interval '1 day')`,
  ]) {
    await pg.query(q)
  }

  // ── 2) 配对(种 starter 团队;#345 钩子同路置备 hr_agents 行)──
  const { code } = await apiCall('/api/computers', { method: 'POST', body: '{}' })
  const pair = await apiCall('/api/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code, engines: ['claude'], hostName: 'e2e-hr-host', version: 'test' }),
  }) as { deviceToken: string; computerId: string }

  // ── 3) 指派(配对收养只覆盖 participants;HR 指派是 owner 的显式动作;
  //    PUT 兜底置备 hr_agents 行)──
  await apiCall('/api/hr', {
    method: 'PUT',
    body: JSON.stringify({ computerId: pair.computerId, engine: 'claude' }),
  })

  // ── 4) roster UNION 断言:device 面的本机清单含 hr-<companyId>(裸数组)
  //    + device 面铸 hr runtime JWT(两个真分支)──
  const roster = await apiCall('/api/computers/me/agents', {}, {
    authorization: `Bearer ${pair.deviceToken}`,
    'x-test-user': '',
  }) as Array<{ id: string }>
  const hrEntry = roster.find((a) => a.id === HR_AGENT_ID)
  expect(hrEntry, 'roster UNION carries the virtual HR entry').toBeTruthy()

  const rt = await apiCall(`/api/agents/${HR_AGENT_ID}/runtime-token`, { method: 'POST', body: '{}' }, {
    authorization: `Bearer ${pair.deviceToken}`,
    'x-test-user': '',
  }) as { token: string }
  expect(rt.token, 'device face mints the HR runtime JWT').toBeTruthy()

  // ── 5) echo Brain 先上线,再触发(订阅先于触发,Redis 一次性投递)──
  const echo = startHrEchoRuntime(rt.token)
  await echo.started
  await new Promise((r) => setTimeout(r, 300))
  const created = await apiCall('/api/hr/evaluations', { method: 'POST', body: '{}' }) as { id: string }
  expect(created.id).toBeTruthy()

  // ── 6) 浏览器:注入登录态 → HR 页 ──
  await page.addInitScript(
    ({ web, token, company }) => {
      localStorage.setItem('cumora.serverUrl', web)
      localStorage.setItem('cumora.auth.token', token)
      localStorage.setItem('cumora.auth.company', company)
    },
    { web: WEB, token: SESSION_TOKEN, company: COMPANY },
  )
  await page.goto(WEB)
  // #368 刀1:rail 退役 —— 登录锚点改 ☰ 菜单钮,HR 面从滑出菜单进入(ADR 0009)。
  await expect(page.getByRole('button', { name: 'Menu' })).toBeVisible({ timeout: 30_000 })
  await page.getByRole('button', { name: 'Menu' }).click()
  await page.getByRole('button', { name: 'HR' }).click({ timeout: 15_000 })

  // ── 7) 评估轮上屏:done 徽章 → 展开首行 → 载荷与输入快照可见 ──
  await expect(page.getByText('done', { exact: true })).toBeVisible({ timeout: 30_000 })
  await page.locator('li button').first().click()
  await expect(page.getByText('Report payload')).toBeVisible()
  await page.getByText('Report payload').click() // 展开 details,载荷才可见
  await expect(page.getByText(SCORE_TEXT)).toBeVisible()
  await expect(page.getByText('Input snapshot')).toBeVisible()

  // ── 8) 归因键落 agent_runs + 花名册零泄漏 ──
  const { rows: runRows } = await pg.query(
    `SELECT id FROM agent_runs WHERE agent_id = $1 AND company_id = $2 LIMIT 1`,
    [HR_AGENT_ID, COMPANY],
  )
  expect(runRows.length, 'evaluation run attributed under hr-<companyId>').toBe(1)
  const { rows: leakRows } = await pg.query(
    `SELECT id FROM participants WHERE company_id = $1 AND (id = $2 OR id LIKE 'hr-%')`,
    [COMPANY, HR_AGENT_ID],
  )
  expect(leakRows.length, 'roster zero-leak holds end to end').toBe(0)

  echo.stop()
  await pg.end()
})

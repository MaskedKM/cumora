/**
 * #147④ 最小 e2e 冒烟:登录 → 给 agent 发消息 → 收到 agent 回复。
 *
 * 链路是"真"的端到端(侦察结论的合成面全是生产路径):
 *   浏览器(登录态)→ POST /conversations/:id/messages → scheduler
 *   → SSE /runtime/wake-stream(本 spec 内联的最小 echo runtime 消费)
 *   → POST /runtime/cli reply → WS message.new → UI 实时上屏。
 *
 * SUT 与 vite preview 由 server/run-integration-tests.mjs 的
 * INTEGRATION_E2E 形态自建;Go 无 CORS,页面侧同源走 preview 代理
 * (localStorage 'cumora.serverUrl' 指向 WEB_BASE,压掉烘焙的 5181)。
 * 登录态不经 OAuth(AuthScreen 无表单):pg 直种 sessions 行 +
 * localStorage 注入(stores/auth 的 key 契约)。
 */
import { test, expect } from '@playwright/test'
import { createHash } from 'node:crypto'
import { Client } from 'pg'

const API = process.env.CUMORA_E2E_API_BASE ?? ''
const WEB = process.env.CUMORA_E2E_WEB_BASE ?? ''
test.skip(!API || !WEB, 'CUMORA_E2E_API_BASE/WEB_BASE 未注入(须由 INTEGRATION_E2E=1 runner 起跑)')

const USER = 'u-e2e-smoke'
const COMPANY = 'c-e2e-smoke'
const SESSION_TOKEN = `e2e-session-${Date.now()}`
const HUMAN_MSG = `冒烟:人来话 ${Date.now()}`
const REPLY_MSG = `冒烟:atlas 回声 ${Date.now()}`

/** Node 侧 harness 调用:fake-auth 头直连 SUT。 */
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

/** 消费 SSE wake-stream 的最小 "echo agent runtime":收到 wake 帧就对
 * 该会话 cli-reply 一句冒烟文本。等价于 mirror-scheduler 触发 + 真实
 * byoa-daemon 回复的最小合成,走的全是生产面(runtime JWT 鉴权)。 */
async function startEchoRuntime(bearer: string): Promise<() => void> {
  const ac = new AbortController()
  const replied = new Set<string>()
  void (async () => {
    try {
      const res = await fetch(`${API}/runtime/wake-stream`, {
        headers: { authorization: `Bearer ${bearer}` },
        signal: ac.signal,
      })
      // eslint-disable-next-line no-console
      console.error(`[echo-rt] wake-stream → ${res.status}`)
      if (!res.ok || !res.body) throw new Error(`wake-stream ${res.status}`)
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
          // eslint-disable-next-line no-console
          console.error(`[echo-rt] frame event=${ev} data=${(data ?? '').slice(0, 160)}`)
          if (ev !== 'wake' || !data) continue
          const frame = JSON.parse(data) as { conversationId?: string }
          if (!frame.conversationId || replied.has(frame.conversationId)) continue
          replied.add(frame.conversationId)
          const r = await fetch(`${API}/runtime/cli`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
            body: JSON.stringify({ argv: ['reply', frame.conversationId, REPLY_MSG] }),
          })
          // eslint-disable-next-line no-console
          console.error(`[echo-rt] reply → ${r.status}: ${(await r.text()).slice(0, 200)}`)
        }
      }
    } catch (e) {
      // eslint-disable-next-line no-console
      console.error(`[echo-rt] exited: ${e instanceof Error ? e.message : String(e)}`)
    }
  })()
  return () => ac.abort()
}

test('smoke: 登录 → 给 atlas 发消息 → 收到回复', async ({ page }) => {
  test.setTimeout(120_000)

  // ── 1) 种登录态:users/companies/membership/participants + sessions ──
  const pg = new Client({ connectionString: process.env.DATABASE_URL })
  await pg.connect()
  const tokenHash = createHash('sha256').update(SESSION_TOKEN).digest('base64url')
  for (const q of [
    `DELETE FROM sessions WHERE user_id = '${USER}'`,
    `DELETE FROM companies WHERE id = '${COMPANY}'`,
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('${COMPANY}', 'E2E Smoke Co', 'e2e-smoke', '${USER}')`,
    `INSERT INTO users (id, email, display_name, tier) VALUES ('${USER}', '${USER}@test.local', 'Smoke Human', 'free') ON CONFLICT (id) DO NOTHING`,
    `INSERT INTO company_members (company_id, user_id, role) VALUES ('${COMPANY}', '${USER}', 'owner') ON CONFLICT DO NOTHING`,
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status) VALUES ('${USER}', '${COMPANY}', 'human', 'Smoke Human', 'owner', 'S', '#abcdef', 'avail') ON CONFLICT DO NOTHING`,
    `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ('${tokenHash}', '${USER}', NOW() + interval '1 day')`,
  ]) {
    await pg.query(q)
  }

  // ── 2) pairing:种 starter 团队(atlas/iris/bram/nova + DMs + all-hands)──
  const { code } = await apiCall('/api/computers', { method: 'POST', body: '{}' })
  const pair = await apiCall('/api/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code, engines: ['claude'], hostName: 'e2e-host', version: 'test' }),
  }) as { deviceToken: string }
  const { rows } = await pg.query(
    `SELECT id FROM participants WHERE company_id = $1 AND kind = 'agent' ORDER BY name LIMIT 1`,
    [COMPANY],
  )
  const atlasId = rows[0]?.id as string
  expect(atlasId, 'starter agent seeded by pairing').toBeTruthy()

  // ── 3) echo runtime 上线(mint runtime JWT + SSE 消费) ──
  const rt = await apiCall(`/api/agents/${atlasId}/runtime-token`, { method: 'POST', body: '{}' }, {
    authorization: `Bearer ${pair.deviceToken}`,
    // runtime-token 走 device 面;fake-auth 头留着无害(优先级更高会注
    // 入 USER —— 显式去掉,让 device token 语义真实生效)。
    'x-test-user': '',
  }) as { token: string }
  const stopRuntime = await startEchoRuntime(rt.token)

  // ── 4) 浏览器:注入登录态,进桌面壳 ──
  page.on('websocket', (ws) => console.error(`[ws] open ${ws.url()}`))
  page.on('pageerror', (e) => console.error(`[page-error] ${e.message}`))
  await page.addInitScript(
    ({ web, token, company }) => {
      localStorage.setItem('cumora.serverUrl', web)
      localStorage.setItem('cumora.auth.token', token)
      localStorage.setItem('cumora.auth.company', company)
    },
    { web: WEB, token: SESSION_TOKEN, company: COMPANY },
  )
  await page.goto(WEB)
  await expect(page.locator('[aria-label="Conversations"]')).toBeVisible({ timeout: 30_000 })

  // ── 5) 打开 Atlas 的 DM,发消息 ──
  // DM 行无 aria-label(侦察假设有误):行内是 img[alt=名] + 文本节点,
  // 用精确文本锚点(快照实测);点击文本冒泡到行 onClick。
  await page.getByText('Atlas', { exact: true }).first().click({ timeout: 30_000 })
  const composer = page.locator('div[role="textbox"]').first()
  await composer.click()
  await composer.fill(HUMAN_MSG)
  await composer.press('Enter')
  await expect(page.getByText(HUMAN_MSG)).toBeVisible({ timeout: 15_000 })
  const { rows: dmRows } = await pg.query(
    `SELECT conversation_id FROM messages WHERE body = $1 LIMIT 1`,
    [HUMAN_MSG],
  )
  const atlasDmId = dmRows[0].conversation_id as string
  expect(atlasDmId).toBeTruthy()

  // ── 6) agent 回复上屏 ──
  // 链路断言到 cli reply 落库为止(全链:msg.new→scheduler→wake SSE→
  // echo runtime→cli reply);呈现面经 REST 重取 —— Go 网关当前是
  // doc-only(gateway.go "非 doc 帧:消息面归 #60,静默忽略"),聊天
  // WS 推送面未迁 Go,浏览器侧靠切换会话触发的 refetch 看到新消息
  // (缺口另开票,补齐后本步可改断言实时上屏)。
  const deadline = Date.now() + 30_000
  for (;;) {
    const { rows: got } = await pg.query(
      `SELECT 1 FROM messages WHERE conversation_id = $1 AND body = $2 LIMIT 1`,
      [atlasDmId, REPLY_MSG],
    )
    if (got.length > 0) break
    if (Date.now() > deadline) throw new Error('reply never landed in pg')
    await page.waitForTimeout(500)
  }
  await page.getByText('Bram', { exact: true }).first().click()
  await page.getByText('Atlas', { exact: true }).first().click()
  await expect(page.getByText(REPLY_MSG)).toBeVisible({ timeout: 15_000 })

  stopRuntime()
  await pg.end()
})

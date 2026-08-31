/**
 * #147④ 最小 e2e 冒烟:登录 → 给 agent 发消息 → 收到 agent 回复。
 *
 * 链路是"真"的端到端(侦察结论的合成面全是生产路径):
 *   浏览器(登录态)→ POST /conversations/:id/messages → scheduler
 *   → SSE /runtime/wake-stream(本 spec 内联的最小 echo runtime 消费)
 *   → POST /runtime/cli reply → WS message.new → UI 实时上屏。
 *
 * SUT 与 vite preview 由 tests/integration/run.mjs 的
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
/** #210:终局前的流式前缀(daemon delta 上报的最小合成)。 */
const DELTA_TEXT = `冒烟:atlas 流式前缀 ${Date.now()}`

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

/** 消费 SSE wake-stream 的最小 "echo agent runtime":收到 wake 帧先走
 * #210 流式增量(POST /runtime/message-delta 上报前缀),等 spec 确认
 * 前缀已上屏(releaseReply)再 cli-reply 终局。等价于 mirror-scheduler
 * 触发 + 真实 byoa-daemon(deltaReporter → cli reply)的最小合成,走
 * 的全是生产面(runtime JWT 鉴权)。 */
async function startEchoRuntime(bearer: string): Promise<{ stop: () => void; releaseReply: () => void }> {
  const ac = new AbortController()
  const replied = new Set<string>()
  let releaseReply!: () => void
  const replyGate = new Promise<void>((r) => { releaseReply = r })
  void (async () => {
    try {
      const res = await fetch(`${API}/runtime/wake-stream`, {
        headers: { authorization: `Bearer ${bearer}` },
        signal: ac.signal,
      })
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
          console.error(`[echo-rt] frame event=${ev} data=${(data ?? '').slice(0, 160)}`)
          if (ev !== 'wake' || !data) continue
          const frame = JSON.parse(data) as { conversationId?: string }
          if (!frame.conversationId || replied.has(frame.conversationId)) continue
          replied.add(frame.conversationId)
          // #210 流式前缀:两帧 delta(daemon 铸流 id,与终局 id 不配对)。
          const streamId = `ds-e2e-${Date.now()}`
          for (const [i, chunk] of [[1, DELTA_TEXT.slice(0, 8)], [2, DELTA_TEXT.slice(8)]] as const) {
            const d = await fetch(`${API}/runtime/message-delta`, {
              method: 'POST',
              headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
              body: JSON.stringify({ conversationId: frame.conversationId, messageId: streamId, delta: chunk, sequence: i, done: false }),
            })
            console.error(`[echo-rt] delta seq=${i} → ${d.status}`)
          }
          await replyGate // 等 spec 断言"前缀先上屏"再发终局(保序确定)
          const r = await fetch(`${API}/runtime/cli`, {
            method: 'POST',
            headers: { 'content-type': 'application/json', authorization: `Bearer ${bearer}` },
            body: JSON.stringify({ argv: ['reply', frame.conversationId, REPLY_MSG] }),
          })
          console.error(`[echo-rt] reply → ${r.status}: ${(await r.text()).slice(0, 200)}`)
        }
      }
    } catch (e) {
      console.error(`[echo-rt] exited: ${e instanceof Error ? e.message : String(e)}`)
    }
  })()
  return { stop: () => ac.abort(), releaseReply }
}

test('smoke: 登录 → 给 atlas 发消息 → 收到回复', async ({ page }) => {
  test.setTimeout(180_000)

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
  const echoRt = await startEchoRuntime(rt.token)

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
  // DM 行无 aria-label(快照实测):行内是 img[alt=名] + 文本节点,用
  // 精确文本锚点。.first() 依赖 DOM 顺序(会话列表在 ChatPane 之前)
  // ——本流程首开无选中会话,列表的 Atlas 是唯一匹配(评审 P3 留档)。
  await page.getByText('Atlas', { exact: true }).first().click({ timeout: 30_000 })
  const composer = page.locator('div[role="textbox"]').first()
  await composer.click()
  await composer.fill(HUMAN_MSG)
  await composer.press('Enter')
  // Scoped to the transcript (role="log"): the sidebar preview legitimately
  // carries the same text now that message.new patches it synchronously
  // (#220) — an unscoped getByText matches both and violates strict mode.
  const transcript = page.getByRole('log')
  await expect(transcript.getByText(HUMAN_MSG)).toBeVisible({ timeout: 15_000 })

  // ── 6) agent 回复实时上屏(#202 补齐后:WS 推送面直连)──
  // 链路:cli reply 落库 → events.MessageNew 发 Redis → 网关桥按租户
  // 转发 → 浏览器 WS message.new → stores/messages applyEvent 上屏。
  // 全程停在 Atlas 会话、无 refetch/切会话 —— 断言的就是实时推送面
  // 本身(REST 重取兜底的旧形态随 #202 关闭)。
  // ── 6b) #210:delta 先到 → 增量渲染(灰显流式气泡),再放行终局 ──
  await expect(transcript.getByText(DELTA_TEXT)).toBeVisible({ timeout: 15_000 })
  echoRt.releaseReply()

  // ── 6c) message.new 收口:真回复上屏,瞬态前缀被幂等替换(不残留)──
  await expect(transcript.getByText(REPLY_MSG)).toBeVisible({ timeout: 30_000 })
  await expect(transcript.getByText(DELTA_TEXT)).toHaveCount(0)

  echoRt.stop()
  await pg.end()
})

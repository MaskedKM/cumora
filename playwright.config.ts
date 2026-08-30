/**
 * #147④ 最小 e2e 冒烟的 Playwright 配置。
 * SUT(Go server + sidecar + mock LLM + 一次性 pg/redis)与 vite preview
 * 由 server/run-integration-tests.mjs 的 INTEGRATION_E2E 形态自建 —— 本
 * 配置只管测试面;端点经环境变量注入:
 *   CUMORA_E2E_WEB_BASE  vite preview 页面源
 *   CUMORA_E2E_API_BASE  Go SUT origin(测试里写进 localStorage
 *                        'cumora.serverUrl',运行时覆盖三层解析的第一层)
 */
import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: 'e2e',
  timeout: 60_000,
  // SUT 是进程级单例(fake-auth 会话 + TRUNCATE 语义),必须串行。
  workers: 1,
  fullyParallel: false,
  reporter: [['list']],
  use: {
    headless: true,
  },
})

/** Harness-minimal env(#70 TS 退役裁剪)。
 *
 * 原 444 行的运行时校验器随 TS server 退役;harness(测试进程的
 * pool/redis/jwt/_helpers/邮件种子)只消费下列字段。测试进程仍经
 * `dotenv/config` 捡起仓库根 .env(runner 也会显式注入测试专属值)。
 */
import 'dotenv/config'

function required(name: string, fallback?: string): string {
  const v = process.env[name] ?? fallback
  if (!v) {
    console.error(`[env] Missing required environment variable: ${name}`)
    process.exit(1)
  }
  return v
}

// Public-source dev default for the runtime-JWT secret —— signAgentToken/
// verifyAgentToken 在测试进程内自洽即可;Go 服对 /runtime 面的令牌
// 校验走它自己的 env(CUMORA_GO_FAKE_AUTH=1 的测试形态)。
const DEV_AGENT_RUNTIME_SECRET = 'dev-agent-runtime-secret-do-not-use-in-prod'

export const env = {
  NODE_ENV: process.env.NODE_ENV ?? 'development',
  DATABASE_URL: required('DATABASE_URL', `postgres://${process.env.USER ?? 'postgres'}@localhost:5432/cumora`),
  REDIS_URL: required('REDIS_URL', 'redis://localhost:6379'),
  AGENT_RUNTIME_SECRET: process.env.AGENT_RUNTIME_SECRET ?? DEV_AGENT_RUNTIME_SECRET,
  EMAIL_DOMAIN: process.env.EMAIL_DOMAIN ?? '',
  EMAIL_INBOUND_HMAC_SECRET: process.env.EMAIL_INBOUND_HMAC_SECRET ?? '',
  CUMORA_RUNTIME_CLIENT: process.env.CUMORA_RUNTIME_CLIENT ?? '',
  // ── sidecar 配套(runner 起 sidecar 子进程时注入;#142 后 sidecar 用
  //    自持的 src/infra/env.ts,不再反向引用本模块)──
  INSTANCE_ID: process.env.INSTANCE_ID ?? '',
  YJS_SIDECAR_TOKEN: process.env.YJS_SIDECAR_TOKEN ?? '',
  YJS_SIDECAR_PORT: Number(process.env.YJS_SIDECAR_PORT ?? 5183),
  // ── storage.ts 消费(sidecar 文档快照/附件 URL;R2 全空=本地 FS 模式)──
  R2_ACCESS_KEY_ID: process.env.R2_ACCESS_KEY_ID ?? '',
  R2_SECRET_ACCESS_KEY: process.env.R2_SECRET_ACCESS_KEY ?? '',
  R2_BUCKET: process.env.R2_BUCKET ?? '',
  R2_ENDPOINT: process.env.R2_ENDPOINT ?? '',
  R2_PUBLIC_BASE: (process.env.R2_PUBLIC_BASE ?? '').replace(/\/+$/, ''),
}

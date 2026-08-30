/** yjs-sidecar 自持 env(#142:内联自 server/src/env.ts 并裁剪)。
 *
 * 退役清尾:sidecar 不再穿透引用 server/src。此处只保留 sidecar 及其
 * infra(pool/redis/storage)消费的字段;缺关键连接串即退,语义与原
 * 件一致。进程经 main.ts 的 `dotenv/config` 捡起仓库根 .env。
 */
function required(name: string, fallback?: string): string {
  const v = process.env[name] ?? fallback
  if (!v) {
    console.error(`[env] Missing required environment variable: ${name}`)
    process.exit(1)
  }
  return v
}

export const env = {
  DATABASE_URL: required('DATABASE_URL', `postgres://${process.env.USER ?? 'postgres'}@localhost:5432/cumora`),
  REDIS_URL: required('REDIS_URL', 'redis://localhost:6379'),
  INSTANCE_ID: process.env.INSTANCE_ID ?? '',
  YJS_SIDECAR_TOKEN: process.env.YJS_SIDECAR_TOKEN ?? '',
  YJS_SIDECAR_PORT: Number(process.env.YJS_SIDECAR_PORT ?? 5183),
  // #145 合帧窗口:同房间 update 攒批后合并落库+跨实例 publish。
  // 窗口毫秒数与触发上限(原始 update 计数),测试经 env 缩短。
  YJS_FLUSH_WINDOW_MS: Number(process.env.YJS_FLUSH_WINDOW_MS ?? 150),
  YJS_FLUSH_MAX_PENDING: Number(process.env.YJS_FLUSH_MAX_PENDING ?? 32),
  // redis.ts 消费(pod 形态懒连接)
  CUMORA_RUNTIME_CLIENT: process.env.CUMORA_RUNTIME_CLIENT ?? '',
  // storage.ts 消费(文档快照/内联图片附件;R2 全空=本地 FS 模式)
  R2_ACCESS_KEY_ID: process.env.R2_ACCESS_KEY_ID ?? '',
  R2_SECRET_ACCESS_KEY: process.env.R2_SECRET_ACCESS_KEY ?? '',
  R2_BUCKET: process.env.R2_BUCKET ?? '',
  R2_ENDPOINT: process.env.R2_ENDPOINT ?? '',
  R2_PUBLIC_BASE: (process.env.R2_PUBLIC_BASE ?? '').replace(/\/+$/, ''),
}

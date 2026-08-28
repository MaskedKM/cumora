# Release Manual

How to cut a new desktop build of Cumora (self-hosted fork, ADR 0003).

## TL;DR

桌面包 = 本地按需重打,没有发布流水线,没有自动更新 feed(#128 起
`package.json` 无 `publish` 配置,打出的包不会向任何上游检查更新):

```bash
# 1. Bump the version in package.json(可选,版本号只影响 --version 与关于页)
npm version patch --no-git-tag-version

# 2. 本地构建(electron:build 会先跑 npm run build 重建 apps/web/dist,
#    再交给 electron-builder —— 永远不会吃到陈旧的 dist)
npm run electron:build:linux   # 或 :mac / :win

# 3. 产物在 release/ 目录,本地安装即可
```

The desktop app connects to the **self-hosted Go server** baked at build
time (#127): `.env.production` 的 `VITE_CUMORA_API_BASE`(默认
`http://127.0.0.1:5181`,即本机 systemd 单元的监听地址)。打包后仍可
经 localStorage `cumora.serverUrl` 运行时改指(Settings/AuthScreen 已
暴露),三层解析见 `apps/web/src/api/client.ts`。

It does **not** deploy the API server. Backend deploys are an explicit,
separately approved action (`godocker build` + `systemctl restart`,见
docs/SWITCHOVER.md);a desktop build must never silently mutate the
backend.

## What a local build does

1. `npm run build` — 重建 `apps/web/dist`(vite production build,吃
   `.env.production` 的自托管指向)。
2. `electron-builder` — 把 `apps/web/dist/**` + `electron/**` 打成平台
   包(AppImage/DEB、DMG、NSIS),输出到 `release/`。
3. 无签名、无公证、无上传 —— 单人自托管不需要;macOS 上如要分发给别人
   再自行配置 Developer ID(上游文档的历史流程已随 release.yml 退役)。

## Common issues

- **桌面包连不上服务器。** 先确认 `VITE_CUMORA_API_BASE` 指向的地址从
  运行桌面的机器可达;服务器不在本机时,用设置面的 serverUrl 覆盖或
  改 `.env.production` 后重打。
- **dist 是旧的。** 直接跑 `electron-builder` 而不走
  `npm run electron:build:*` 会吃到陈旧 `apps/web/dist` —— 永远走
  npm script。
- **想恢复自动更新。** 给 `package.json` 的 `build.publish` 配一个指向
  自己静态源的 `generic` provider(latest-linux.yml 等),`electron/
  autoUpdater.cjs` 的全套机制原样可用。

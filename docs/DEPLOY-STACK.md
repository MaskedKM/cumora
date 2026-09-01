# 单制品部署 runbook:AppImage + 受管栈(ADR 0005)

适用形态:`Cumora-<ver>.AppImage`(桌面客户端 + 整个 Stack:server /
daemon / sidecar / stack / stackd + pg16+pgvector + redis)。数据与配置
全部在制品外:`~/.local/share/cumora/`(数据)与 `~/.config/cumora/`
(配置);AppImage 是纯发行载体,装后可删。

## 净机首启(向导)

桌面 App 启动时探 `127.0.0.1:5181/api/livez`,不通 → 首启向导:

1. 选择凭据源:导入既有 `~/Code/cumora/.env` + `~/.cumora/daemon.env`,
   或新录 GitHub OAuth 两键(`GITHUB_CLIENT_ID/SECRET`,登录链硬依赖)。
2. 向导依次执行(编排在制品内 `cumora-stack` 二进制,GUI 只是壳):
   - `cumora-stack import-env` —— 机器事实转 `stack.toml`,凭据原样搬
     `~/.config/cumora/{stack.env,daemon.env}`(0600);
   - `cumora-stack absorb <resources/bin>` —— 载荷铺进
     `~/.local/share/cumora/releases/<ver>/` + 原子切 `current`;
   - `cumora-stack install` —— 装 `cumora.service`(systemd user unit),
     stackd 拉起 pg→redis→sidecar→server→daemon(首启含 initdb + 迁移)。
3. 完成屏显示 `doctor` 摘要与 OAuth provider 配置态;进入登录。

## 存量部署迁入(migrate-pg,#285)

已有系统 pg(Ubuntu apt 版)上的活数据一次性迁入内置 pg(同 16 major,
dump/restore 无版本坎):

```bash
~/.local/share/cumora/current/cumora-stack migrate-pg
```

窗口语义:自动停链 → 备份源库(`<data>/backups/cumora-premigrate-*.dump`,
`-Fc` 格式可独立 `pg_restore`)→ 恢复进内置库 → 六核心表行数比对
(messages / convene_transcript / board_cards / document_snapshots /
conversations / participants,不一致**阻断切链**)→ `stack.toml`
`pg.mode` 切 `internal` → 起链。

- **幂等**:重跑 no-op(`migrate-pg.state.json` 标记);`--force` 重做。
- **重跑矩阵**:`pg.mode=internal` 后**恒拒**(迁移后的新写入在内置库,
  重做 = 静默销毁,绝不);标记损坏按"状态未知"拒;`--force` 只在
  external 形态下有意义(源库仍是权威,重做安全)。
- **源库全程只读**;`--dry-run` 只探测与出计划。
- 失败语义:任一步失败 → 不切链、旧链路数据零动、自动起链恢复服务;
  修因后 `--force` 重做。

## 旧库退役路径(系统 pg)

迁移完成且新栈稳定运行一段时间后:

1. 观察期建议 ≥ 1 周:doctor 全绿、无回退需求。
2. 备份已经独立存在(`<backups>/cumora-premigrate-*.dump`),随时可
   `pg_restore` 回系统 pg 或任何 16+ 实例。
3. 退役 = 常规系统操作,**cumora 不代劳**:

   ```bash
   sudo systemctl disable --now postgresql      # 停系统服务
   sudo apt remove 'postgresql-16*'             # 真删(可选)
   ```

4. 回退(如需):`stack.toml` 改回 `pg.mode = "external"`(备份目录里的
   `stack.toml.premigrate` 是 external 期的**一次性**留底,internal 后的
   修改不在内——以当前 toml 手工改回为准)+ `stack.env` 里的
   `DATABASE_URL` 指回系统库 → 重启栈。数据以备份或系统库为准。
5. 已切 internal 后确需重迁(高级,自担风险):`systemctl --user stop
   cumora` → `pg_ctl`(制品内)起内置 pg → `dropdb/createdb cumora` →
   `pg_restore` 备份或系统库新 dump → `pg_ctl stop` → 起链。
   `migrate-pg` 本体在此形态下恒拒,是有意防线。

## 升级与回滚(#286)

- 升级:桌面设置页手动确认(下载新 AppImage 不触碰运行中栈);确认后
  停链 → absorb 新制品 → 原子切 `current` → 滚动重启。
- 回滚:`releases/` 保留最近 3 份(`stack.toml` 的 `stack.keep_releases`),
  一键切回;pg schema 迁移不可逆时回滚按钮禁用并说明。
- 完全卸载:`cumora-stack uninstall`(恢复旧三 unit 形态)或
  `--purge` 连数据删。

## 排障入口

- `cumora-stack doctor` —— 为什么坏(任何 fail → 退出码 1);
- `cumora-stack status` —— 现在跑得怎样(含 manifest/stackd 子进程面);
- `cumora-stack logs -f [--svc NAME]` —— journal 单流按 svc 过滤;
- `stack.toml` 坏值/未知键 = doctor 红项 + stackd 拒启(journal 有原因)。

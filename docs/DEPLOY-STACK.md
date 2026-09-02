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

已有外部 pg 上的活数据一次性迁入内置 pg(同 16 major,dump/restore
无版本坎;本部署的实际源 = docker 容器 `cumora-pg`,见下节):

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

## 旧依赖退役路径(docker 容器,2026-09-02 实态)

迁移完成且新栈稳定运行后退役旧依赖。**本部署实态(#316 甄别):存量
pg 与外部 redis 从来不是 systemd 系统服务,而是两个 docker 容器**
(host 网络,`restart=unless-stopped`,数据在具名卷):

| 容器 | 镜像 | 宿主端口 | 角色 |
|---|---|---|---|
| `cumora-pg` | postgres:16-alpine | 5432 | 迁移前存量库(migrate-pg 源,已迁完) |
| `cumora-redis` | redis:7-alpine | 6379 | 切 internal 前的外部 redis 总线 |

1. 前置:redis 先切受管形态 —— `stack.toml` `[redis] mode = "internal"`
   → `cumora-stack restart` → 验证五子进程齐(含受管 redis socket)、
   doctor 无 fail、livez 200。pg 侧 migrate-pg 后已是 internal。
2. 备份已经独立存在(`<backups>/cumora-premigrate-*.dump`),随时可
   `pg_restore` 回任何 16+ 实例;旧库数据卷退役不删,是第二重回退材料。
3. 退役 = 常规 docker 操作,**cumora 不代劳**(数据卷保留):

   ```bash
   docker update --restart=no cumora-pg cumora-redis  # 防御性自文档:手工 stop 后
                                                      # unless-stopped 本就不自启,
                                                      # 但显式 no 免歧义
   docker stop cumora-pg cumora-redis
   ```

4. 真删旧数据(可选,**确认不再走 docker 回退后**):卷被容器引用时
   `volume rm` 会拒,须先删容器 —— 而 `docker rm` 会摧毁第 5 步的
   `docker start` 回退路径(届时只剩 `pg_restore` 备份一条路)。只有
   `cumora-pg` 的卷是回退材料;redis 卷只是易失总线态,随手可删:

   ```bash
   docker rm cumora-pg cumora-redis
   docker volume rm <pg卷> <redis卷>   # docker volume ls 查名
   ```

5. 回退(如需):`docker start cumora-pg cumora-redis` + `stack.toml`
   改回对应 `mode = "external"`(redis 直接改回 —— external 形态解析
   链吃 stack.env 的 `REDIS_URL` 或缺省 `localhost:6379`,本部署两者
   仍指旧容器,无需动作;pg 的 external 形态需 `stack.env` 的
   `DATABASE_URL` 指回 5432)→ 重启栈。数据以旧库卷或备份为准
   (internal 期新写入不在其中)。**长期回 external** 另补
   `docker update --restart=unless-stopped cumora-pg cumora-redis`,
   否则宿主重启后旧依赖不再自启、external 形态静默断链。
6. 已切 internal 后确需重迁(高级,自担风险):`systemctl --user stop
   cumora` → `pg_ctl`(制品内)起内置 pg → `dropdb/createdb cumora` →
   `pg_restore` 备份或旧库新 dump → `pg_ctl stop` → 起链。
   `migrate-pg` 本体在此形态下恒拒,是有意防线。
7. 注意:宿主 6379/5432 若再出现监听,先 `ss -ltnp` 与 `docker ps` 交叉
   甄别归属(同机其他项目可能跑自己的 redis/pg 容器;**未发布端口**的
   bridge 容器在自身 netns 内绑 6379 与宿主面无关,`-p` 发布的才会占
   宿主)—— 只退役属于 cumora 的件。

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

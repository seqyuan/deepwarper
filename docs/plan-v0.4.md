# DeepWarper v0.4 方案：登录即实例化、插件版本化与多租户边界

> 状态：草案，供 review  
> 基线：`v0.3.0`（commit `1287dc3`，2026-10-30）  
> 目标读者：平台/运维/后端/安全  
> 关联代码：`cmd/dw`、`internal/auth`、`internal/gateway`、`internal/instance`、`internal/mount`、`internal/plugin`

---

## 0. 摘要

v0.3.0 已经把 DeepWarper 从“SGE 控制面”收敛为“本机一用户一 dsh 的登录门户”，并完成了大量安全与生命周期加固：

- local 账户 + 登录页 + 子域名路由；
- bubblewrap 目录视图 + rclone 云挂载；
- Store revision/CAS、per-record 锁、端口按 incarnation 归属；
- Host allowlist、Origin 校验、ticket janitor、dsh cookie 刷新；
- dsh/rclone/systemd/bwrap 真实 E2E。

但 v0.3.0 还存在 5 个产品/治理缺口，本方案把它们作为 v0.4 的全部范围：

| # | 缺口 | 影响 |
|---|---|---|
| 1 | **登录后不会自动实例化，也没有 `dw start`** | 用户按 README 登录后看到 503“请管理员启动”，但管理员没有启动命令 |
| 2 | 没有 TOTP/2FA | 与 goprox 风格不一致；弱口令风险 |
| 3 | 插件只有 `{name, path}`，无版本/校验和/回滚 | catalog 更新不生效；无法审计用户装了什么 |
| 4 | 云端挂载没有 prefix 白名单；配额未执行；没有 audit | 共享 bucket 可能越权；资源不可控；事故不可追溯 |
| 5 | bwrap 只隔离文件系统/PID，**共享网络命名空间** | 同 UID 下其他实例的 `127.0.0.1` 端口可被探测 |

**本方案不重写 v0.3.0 的网关、bwrap、rclone、CAS 和 ticket 机制**，只在其上做增量。预计 2–3 周。

---

## 1. 现状与问题证据

### 1.1 登录不会启动实例

`internal/gateway/login.go` 的 `handleUserHome` 只做查询：

```go
inst, err := g.mgr.ByUser(user)
if inst == nil || inst.State != model.StateRunning {
    writeNotRunning(w) // 503 "该用户的实例尚未运行，请由管理员启动"
    return
}
```

`cmd/dw/main.go` 的命令列表里没有 `start`，管理员只能通过：

```bash
curl -X POST /api/platform/launch -H 'Authorization: Bearer ...' \
     -d '{"user_id":"alice"}'
```

间接触发 `Ensure`。这是当前最明显的“文档/实现/产品流程”不一致。

### 1.2 插件没有版本

`internal/plugin/plugin.go`：

```go
type Entry struct {
    Name string `yaml:"name"`
    Path string `yaml:"path"`
}
```

`Enable` 把 catalog 目录复制到 `home/plugins/<name>`，之后**不会覆盖用户副本**（测试 `TestEnableKeepsUserCopyWhenCatalogMoves` 固化了该行为）。因此：

- 没有版本号，无法回答“alice 装的是 office 1.1 还是 1.2”；
- 没有 sha256，catalog 被替换无法发现；
- 没有原子切换/回滚；
- catalog 升级对已启用用户不生效。

### 1.3 云端挂载没有 prefix 白名单

`internal/mount/mount.go` 的 `Remote` 只有：

```go
type Remote struct {
    Name, Provider, Endpoint, Region, Bucket, CredentialFile string
}
```

`checkPrefix` 只校验语法（不能绝对路径、不能 `..`），不校验管理员允许的业务 prefix。只要 remote 存在，用户就能挂该 bucket 下任意前缀。

### 1.4 配额未执行

`internal/config/config.go` 只解析并给 `Quotas` 默认值；`internal/instance/manager.go` 没有读取 `MaxCPUPerUser` / `MaxMemoryPerUser` / `MaxWalltime` / `MaxInstancesPerUser`。一个用户仍可对不同 distribution 各建一个实例。

### 1.5 网络没有隔离

`internal/mount/mount.go` 的 bwrap 参数包含：

```text
--unshare-user --unshare-pid --proc /proc --dev /dev ...
```

没有 `--unshare-net`。沙箱内的 dsh 可以访问宿主 `127.0.0.1` 上的其他 dsh 端口和控制面端口。虽然 dsh token 不暴露，但这仍是同 UID 下的横向探测面。

### 1.6 其他 v0.4 顺手项

- `Manager.Ensure` 在 readiness 等待期间持有 guard（代码中有 `TODO(launch-cancellation)`），并发登录会串行等待到 readyTimeout。
- `ByHost` / `ByUser` 每次请求 `store.List()`，O(n) 解析 YAML。
- `cmd/dw-local` 仍是历史 POC，和主实现重复。
- `docs/local-mode.md` 仍是 v0.1 的平台 ticket 内容；`docs/plan.md` 只有几行。

---

## 2. 目标与非目标

### 2.1 目标

1. 新建账户登录后，自动创建并启动该用户的 dsh；页面先显示进度，不阻塞 HTTP 请求。
2. 管理员可以显式 `dw start/stop/status`，并可用 `auth.autoStart: false` 保留“管理员启动”模式。
3. `dw user` 支持 TOTP；`dw 2fa-reset` 与 goprox 对齐。
4. 插件支持版本、sha256、原子切换、回滚和 list；catalog 升级可控。
5. 云端挂载支持 `allowPrefixes`；配额在启动/加挂载/装插件前执行；关键操作写 audit JSONL。
6. 明确本地多租户信任边界：默认 `shared` 网络，给出 `isolated` 方案和失败关闭策略。
7. 保持 v0.3.0 的现有安全机制：Host allowlist、Origin、ticket janitor、dsh cookie 刷新、CAS、bwrap、rclone 清理。

### 2.2 非目标

- 不恢复 SGE / k8s / Pod。
- 不做多控制面 HA。
- 不做通用 npm 在线安装；插件仍由管理员 catalog 治理。
- 不在本期承诺“任意不可信用户强隔离”；网络隔离先做方案验证和可选模式。
- 不做 Web 管理后台的完整 UI；保持 server-rendered 最小页面。

---

## 3. 总体设计

```text
浏览器
  │  GET /login
  ▼
Portal (gateway/login.go)
  │  密码 / TOTP
  ▼
auth (usersDir)
  │  成功
  ▼
Instance Launcher (new: instance/launch.go)
  │  StartAsync(user)
  ├─ 校验身份/配额/workspace/mount
  ├─ 写 Pending 记录（同步、快速）
  └─ 后台 launchPending()
        ├─ materialize profile / prepareHome
        ├─ attachSandbox (bwrap + rclone + mounts)
        ├─ backend.Start
        └─ await ready -> Running
  │
  ▼
浏览器 302 / (或 apex -> 子域名 ticket)
  │
  ▼
Gateway 反代 127.0.0.1:<port>
  │  dsh cookie broker
  ▼
dsh inside bwrap
```

新增模块：

```text
internal/instance/launch.go   两阶段启动：prepare -> pending -> launch
internal/instance/quota.go    配额检查
internal/auth/totp.go         TOTP
internal/plugin/catalog.go    catalog v2 (version/sha256/requires)
internal/plugin/install.go    安装/升级/回滚
internal/mount/prefix.go      allowPrefixes 匹配
internal/audit/audit.go       JSONL 审计
cmd/dw/start.go               start/stop/status
```

---

## 4. P0：登录即实例化 + `dw start`

### 4.1 为什么不能直接在 login handler 里调用 `Ensure`

`Ensure` 会一直阻塞到 dsh `ready.json`（最长 `readyTimeout`，默认 90s），并且当前在等待期间持有 per-user guard。直接在 HTTP handler 里调用会导致：

- 登录请求挂 90s；
- 并发登录排队；
- 浏览器超时后重试，可能触发第二次启动。

因此拆成两阶段。

### 4.2 两阶段启动

```text
StartAsync(req)
  1. 校验 user/dist/workspace/mount/quota
  2. 加 guard
  3. 已有 Running/Starting/Pending -> 直接返回
  4. 解析 home/workspace/control/log/port/profile
  5. 写 Pending 记录（含 LaunchRequestedAt、incarnation）
  6. 释放 guard
  7. 投递到 per-user singleflight worker
  8. 立即返回 Pending

launchPending(inst)
  1. materialize profile
  2. prepareHome（conda + plugin reconcile）
  3. ensureDefaultWorkspaceLink
  4. attachSandbox（bwrap/rclone/mounts）
  5. backend.Start
  6. 更新 Starting
  7. h.Endpoint(ctx, readyTimeout)
  8. 更新 Running / abortLocked(Failed)
```

关键点：

- `StartAsync` 只做“可快速失败”的校验和 Pending 落盘，不碰 dsh。
- `launchPending` 用 `context.Background()` 或 server context，不随登录请求取消。
- 用 `singleflight` 或 `Manager` 内 `launching map[incarnation]bool` 去重；同一 incarnation 只跑一次。
- `Ensure` 保留为同步入口（平台 API、`dw start --wait`），内部复用 `prepare + launchPending`。
- 控制面在 Pending 阶段重启：`Reconcile` 现有 `reconcilePending` 会在 `pendingLimit` 后 abort；如果需要自动恢复，可在 v0.4 增加 `auth.autoResumePending`，但默认不自动重试，避免重启风暴。

### 4.3 状态机

```text
(no record)
  │ autoStart / dw start
  ▼
pending ──launch──▶ starting ──ready──▶ running
  │                    │                 │
  │ fail/timeout       │ fail            │ stop/idle
  ▼                    ▼                 ▼
failed               failed           stopping ──▶ stopped
```

语义：

- `pending`：已接受启动请求，尚未拉起 dsh（新增于异步路径，v0.3 已有该状态）。
- `starting`：backend 已启动，等待 ready。
- `running`：可反代。
- `stopping`：停止中；launch 请求返回 409/202，不新建实例。

### 4.4 登录流程

```text
POST /login
  ├─ 密码/TOTP 失败 -> 401/429
  ├─ ByUser(user)
  │    ├─ Running -> 发 dw_session；apex 登录则 ticket 到子域名
  │    ├─ Pending/Starting -> 发 dw_session；302 /；页面 202 进度
  │    ├─ Stopping -> 409 + Retry-After
  │    └─ nil
  │         ├─ auth.autoStart=true  -> StartAsync；302 /（202 进度）
  │         └─ auth.autoStart=false -> 503 + "请运行 dw start <user>"
  └─ 任何启动错误 -> 4xx/5xx，不吞掉
```

`/_dw/starting` 或现有 `writeRetryPage`：

```html
<meta http-equiv="refresh" content="3">
<h2>实例启动中</h2>
<p>状态：pending/starting</p>
```

ready 后刷新 `/` 即可；子域名模式在 apex 登录时仍走 ticket 跳转。

### 4.5 CLI

```bash
dw start [-d configDir] [-c config.yaml] <user> [--wait]
dw stop  [-d configDir] [-c config.yaml] <user>
dw status [user] [--json]
dw user list
```

- `dw start --wait`：调用 `StartAsync` 后轮询 `ByUser`，直到 Running/Failed 或超时；默认不等待。
- `dw status`：输出 user/state/dist/host/port/mounts/plugins/error。
- `auth.autoStart` 默认 `false`，保持 v0.3 行为；推荐部署在示例里设为 `true`。
- `dw start` 只允许管理员 CLI 调用；Web 端没有启动按钮（避免普通用户绕过配额）。

### 4.6 配置

```yaml
auth:
  usersDir: /etc/deepwarper/users
  # true: 登录时自动 StartAsync；false: 保持 v0.3 的“管理员启动”
  autoStart: true
  loginMaxAttempts: 10
  loginWindow: 5m
  loginLockout: 5m
```

### 4.7 验收

- 新建用户 → 登录 → 5s 内看到进度页 → 自动进入 dsh；URL 无 dsh token。
- 10 个并发登录同一用户：只产生一个实例、一个 dsh 进程。
- `autoStart=false`：登录返回 503 且页面给出 `dw start <user>`。
- `dw start --wait` 能返回 Running；`dw stop` 后状态为 stopped。
- 控制面在 pending/starting 阶段被 `kill -9`：重启后不会留下 double start；有明确 failed/stopped 结论。

---

## 5. P1：TOTP / 2FA

### 5.1 数据模型

```go
type Auth struct {
    PasswordHash   string `yaml:"password_hash"`
    SessionVersion int    `yaml:"session_version"`
    Deleted        bool   `yaml:"deleted,omitempty"`
    TOTPSecret     string `yaml:"totp_secret,omitempty"`
    TOTPEnabled    bool   `yaml:"totp_enabled,omitempty"`
}
```

- secret 存用户 YAML，0600；不写日志。
- `dw 2fa-enable <name>`：生成 base32 secret，打印 `otpauth://` URL（可选二维码），要求输入一次正确 OTP 后置 `TOTPEnabled=true`。
- `dw 2fa-reset <name>`：清空 secret、`TOTPEnabled=false`、`SessionVersion++`（旧会话失效）。
- `dw passwd` 同样递增 `SessionVersion`。

### 5.2 登录流程

```text
POST /login (username, password)
  ├─ 密码错误 -> 401（限速）
  ├─ TOTP 未启用 -> 直接发 dw_session
  └─ TOTP 启用 -> 不发 session；设置短期 dw_login cookie（5m）
                  返回 OTP 表单
POST /login/2fa (otp)
  ├─ 校验 ±1 时间步、6 位、30s
  ├─ 成功 -> 发 dw_session；清除 dw_login
  └─ 失败 -> 401/429
```

- `dw_login` 用现有 HMAC key 签，payload `{user, exp, nonce}`，HttpOnly。
- 限速对 IP、IP|user、IP|user|otp 三层；OTP 失败也计数。
- 用户不存在与密码错误保持相同耗时（已有 dummy bcrypt）。

### 5.3 依赖

优先用成熟库：

- `github.com/pquerna/otp/totp`（TOTP 生成/校验）
- QR 可选 `github.com/skip2/go-qrcode`

如果不想加依赖，可用 stdlib `crypto/hmac` + `crypto/sha1` 实现 RFC 6238；但二维码/otpauth URL 需要自己拼。方案评审时定。

### 5.4 验收

- 开启 2FA 后，只有密码不能登录；密码 + OTP 成功。
- `dw 2fa-reset` 后旧 `dw_session` 和 `dw_login` 都失效。
- OTP 重放/过期/错误时间窗被拒绝。
- 登录限速覆盖两步。

---

## 6. P1：插件 v2（版本、校验和、回滚）

### 6.1 Catalog v2

```yaml
plugins:
  - name: office
    version: 1.2.0
    # bundle 优先：预构建 tar.zst；path 用于本地开发目录
    bundle: /opt/deepwarper/catalog/office-1.2.0.tar.zst
    sha256: 9f2c...
    path: /opt/deepwarper/catalog/office-1.2.0
    patch: /opt/deepwarper/catalog/office-1.2.0.patch.yml
    requiresDsh: ">=0.2.1-rc.2"
    description: Office 文档插件
```

规则：

- `bundle` 与 `path` 二选一；`bundle` 安装前校验 sha256。
- `patch` 可选；安装时追加到 `home/user-cordis.patch.yml`，带 marker。
- `requiresDsh` 用 `runtime.dshVersion` 比较（简单 semver 子集；解析失败则告警不阻断）。
- Catalog 由管理员维护，普通用户只能 enable/disable 已发布版本。

### 6.2 安装布局

```text
<home>/plugins/<name>/<version>/
    package.json
    node_modules/
    ...
<home>/plugins/<name>/current -> <version>     # 可选指针
<home>/user-cordis.patch.yml                   # id: dw-plugin-<name>, plugin: .../<version>
```

- 安装到版本目录，不覆盖旧版本。
- patch 行指向精确版本目录；切换版本 = 原子替换该行。
- 旧版本保留，`dw plugin gc` 清理未被引用的版本。
- 如果插件包需要 dsh 的 `package.json` 依赖，catalog bundle 必须已包含 `node_modules`；不接受运行时 `npm install`。

### 6.3 安装/升级/回滚

```bash
dw catalog add office --bundle ... --sha256 ...
dw plugin install <user> office@1.2.0
dw plugin update  <user> office          # 取 catalog 最新版
dw plugin rollback <user> office         # 切回上一个 patch 行版本
dw plugin list <user>
dw plugin disable <user> office
```

流程：

```text
1. 校验 catalog/version/sha256/requiresDsh
2. 停实例（若运行）
3. 解包到 <home>/plugins/<name>/<version>（staging + rename）
4. 校验插件目录结构（package.json、无绝对 symlink、无 ..）
5. 备份当前 user-cordis.patch.yml
6. 原子写 patch 行（temp + rename）
7. 可选：dsh --profile ... --dump-config 冒烟
8. 启动实例；失败则恢复 patch 行并重启旧版本
9. audit 记录 name/version/sha256/actor
```

### 6.4 兼容 v0.3 的 flat 布局

- 已存在的 `home/plugins/<name>`（无版本目录）视为 `legacy`；`dw plugin list` 显示 `legacy`。
- 首次 `install/update` 时迁移到 `<name>/<version>`，保留旧目录一周期。
- `Reconcile` 只处理 `dw-plugin-*` 行，不动用户其他 patch 行。

### 6.5 验收

- 安装 1.1 → 升级 1.2 → rollback 1.1，dsh 加载结果随之变化。
- sha256 不匹配拒绝安装；`requiresDsh` 不满足拒绝。
- 两个用户装同名插件互不影响；禁用后 patch 行移除、进程重启后插件不加载。
- 安装失败时旧版本仍可启动。

---

## 7. P1：挂载白名单、配额、审计

### 7.1 Remote allowPrefixes

```go
type Remote struct {
    Name           string   `yaml:"name"`
    Provider       string   `yaml:"provider"` // tos | s3
    Endpoint       string   `yaml:"endpoint"`
    Region         string   `yaml:"region"`
    Bucket         string   `yaml:"bucket"`
    CredentialFile string   `yaml:"credentialFile"`
    AllowPrefixes  []string `yaml:"allowPrefixes"`
    AllowRW        bool     `yaml:"allowRw"`
}
```

CLI：

```bash
dw remote add tos1 --provider tos --endpoint ... --region ... --bucket data \
  --credential-file /etc/deepwarper/tos.cred \
  --allow-prefix P123/ --allow-prefix public/ [--allow-rw]
```

匹配规则：

- prefix 统一去掉前导 `/`，保留结尾 `/` 语义；
- 请求 prefix 必须等于 allow 项，或以 `allow/` 开头；
- `*` 只有显式配置才允许整个 bucket；
- 空 `allowPrefixes` 默认拒绝所有 bucket mount（fail closed）；
- 老配置没有该字段时，启动日志警告并拒绝新的 bucket mount，给出迁移提示。

`mount add` 与 `mount.Build` 双重校验；`mount ls/rm` 补齐。

### 7.2 配额

```yaml
quotas:
  maxInstancesPerUser: 1
  maxCPUPerUser: 16
  maxMemoryPerUser: 64Gi
  maxWalltime: 24h
  maxMountsPerUser: 8
  maxPluginsPerUser: 8
```

执行点：

- `StartAsync` / `Ensure`：检查 active 实例数、CPU/内存总和、walltime 上限。
- `auth.mode` 下强制“一个用户一个活跃实例”，无论 distribution；多 distribution 允许存在 stopped 记录，但不能同时 Running。
- `mount add` / `plugin install`：检查每用户数量上限。
- 错误类型 `ErrQuota`；HTTP 409（或 403）+ 明确文本；CLI 非零退出。
- 控制面重启后 `Reconcile` 重新统计，拒绝超额的新启动。

### 7.3 审计

新增 `internal/audit`：

```json
{"ts":"2026-10-30T12:00:00Z","event":"login","actor":"alice","ip":"10.0.0.1","result":"ok"}
{"ts":"...","event":"start","actor":"alice","user":"alice","instance":"...","result":"ok"}
{"ts":"...","event":"plugin.install","actor":"admin","user":"alice","detail":{"name":"office","version":"1.2.0","sha256":"..."},"result":"ok"}
```

- 文件：`<dataDir>/audit/audit-YYYY-MM-DD.jsonl`，0600，只追加。
- 事件：user.create/passwd/delete/2fa-reset、login success/fail、start/stop/fail、mount.add/rm、plugin.install/update/rollback/disable、quota.reject。
- 不含密码、token、AK/SK、完整挂载凭据。
- `dw audit tail [-n N]` 可选。
- 按天轮转；保留天数由 `audit.retainDays` 配置，默认 90。

### 7.4 验收

- 越界 prefix 被 `dw mount add` 和启动双重拒绝。
- 超过 maxMounts/maxPlugins/CPU 内存被拒绝。
- 同一用户两个 distribution 不能同时 Running。
- 审计文件包含关键事件且无 secret。

---

## 8. P2：网络隔离 / 多租户边界

v0.3 的 bwrap 只隔离文件系统/PID，`--share-net` 效果等同于共享宿主网络。需要明确：

### 8.1 默认策略

- `runtime.local.network: shared`（默认）
- `auth.trustedUsers: true` 显式声明“这些用户互信”
- 启动/`dw doctor` 打印：

```text
warning: bwrap shares the host network; users can reach 127.0.0.1 ports of other instances
```

如果 `auth.trustedUsers=false` 且 `network=shared`，启动直接失败（fail loud），避免误部署。

### 8.2 isolated 方案候选

| 方案 | 优点 | 缺点 |
|---|---|---|
| per-user Linux UID + iptables `-m owner` | 不改 dsh；隔离真实 UID | 需要 root/sudo 建用户；控制面要能跨 UID 代理 |
| bwrap `--unshare-net` + pasta/slirp4netns | 无 root；网络命名空间隔离 | 需要 per-instance 网络配置/端口转发；复杂度高 |
| systemd `PrivateNetwork` + HTTP proxy | 复用 systemd | 需要代理模型 API；WebSocket/本地端口复杂 |

建议先做 **per-user UID + iptables** 的 spike（如果站点允许 root），否则接受 `shared` + `trustedUsers`。

### 8.3 验收

- isolated 模式下，实例 A 无法连接实例 B 的 `127.0.0.1:<port>`。
- 实例仍能访问模型 API / TOS endpoint。
- 控制面能代理到实例端口。
- `dw doctor` 明确报告当前网络模式。

> 本节不阻塞 v0.4 前四项；如果站点只服务可信团队，可以只做 `trustedUsers` 校验和告警。

---

## 9. 配置变更汇总

```yaml
auth:
  usersDir: /etc/deepwarper/users
  autoStart: true
  trustedUsers: true
  loginMaxAttempts: 10
  loginWindow: 5m
  loginLockout: 5m

runtime:
  dshVersion: 0.2.1-rc.2       # 插件 requiresDsh 使用
  local:
    network: shared            # shared | isolated

quotas:
  maxInstancesPerUser: 1
  maxCPUPerUser: 16
  maxMemoryPerUser: 64Gi
  maxWalltime: 24h
  maxMountsPerUser: 8
  maxPluginsPerUser: 8

audit:
  retainDays: 90

plugin:
  catalog: /etc/deepwarper/catalog.yaml
  allowNpm: false              # 保留字段，默认 false
```

挂载 roots/remotes 继续使用 `<configDir>/roots.yaml`、`<configDir>/remotes.yaml`；catalog 继续使用 `<configDir>/catalog.yaml`。

---

## 10. 文件级实现清单

| 文件 | 变更 |
|---|---|
| `cmd/dw/main.go` | 注册 `start/stop/status/user list/2fa-*` |
| `cmd/dw/start.go`（新） | start/stop/status 命令，`--wait` |
| `cmd/dw/admin.go` | `2fa-enable/reset`、`user list`、`plugin install/update/rollback/list`、`mount ls/rm` |
| `internal/auth/auth.go` | `TOTPSecret/TOTPEnabled`、`User.List`、session version 复用 |
| `internal/auth/totp.go`（新） | secret、otpauth URL、verify |
| `internal/auth/totp_test.go`（新） | 时间窗/重放/错误码 |
| `internal/gateway/login.go` | TOTP 二步、autoStart、StartAsync、503/202 |
| `internal/gateway/gateway.go` | 复用 writeRetryPage；审计 hook |
| `internal/gateway/security_test.go` | autoStart/2FA/并发登录 |
| `internal/instance/manager.go` | 拆出 `prepare`/`launchPending`；配额；审计 |
| `internal/instance/launch.go`（新） | `StartAsync`、singleflight、后台启动 |
| `internal/instance/quota.go`（新） | 配额计算/错误类型 |
| `internal/plugin/plugin.go` | legacy 兼容、版本目录、原子 patch |
| `internal/plugin/catalog.go`（新） | catalog v2 解析/校验 |
| `internal/plugin/install.go`（新） | install/update/rollback/gc |
| `internal/mount/mount.go` | `AllowPrefixes`、`AllowRW`、`mount ls/rm` |
| `internal/mount/prefix.go`（新） | prefix 匹配 |
| `internal/audit/audit.go`（新） | JSONL writer/rotation |
| `internal/config/config.go` | auth.autoStart/trustedUsers、quotas、audit、dshVersion、network |
| `docs/local-mode.md` | 重写为 v0.3/v0.4 的登录/bwrap/mount/plugin 流程 |
| `cmd/dw-local` | 删除或移到 `examples/legacy` |

---

## 11. 测试计划

### 11.1 Unit

- `StartAsync`：Pending 立即返回；并发 10 次只启动 1 次；失败落 Failed；guard 在 readiness 等待期间释放。
- 登录：autoStart true/false；Pending/Starting/Running/Stopping 分支；429/409/503。
- TOTP：RFC 向量、±1 步、重放、secret reset、session version。
- Plugin：sha256、版本目录、原子 patch、rollback、legacy 迁移、catalog 更新。
- Mount：allowPrefixes 边界（`P123/` vs `P1234/`）、`*`、空列表 fail closed、rw 限制。
- Quota：CPU/内存/实例数/mount/plugin 上限。
- Audit：字段、0600、无 secret、轮转。

### 11.2 Integration / E2E

- 新建用户 → 登录 → 自动启动 → dsh 页面/API/WS；URL 无 token。
- 两个用户同时登录：独立 home/workspace/mount；互访 401/404。
- 插件安装 → 重启 → 插件生效；rollback 后恢复。
- TOS ro mount → dsh 读取；卸载清理。
- `kill -9 dw serve` 在 pending/starting/running 三个阶段：无重复实例、无端口泄漏、能 adopt。
- `auth.autoStart=false` 的管理员流程。

### 11.3 回归

- `go test ./...`、`go test -race ./...`、`go vet ./...`。
- 现有 28 个 gateway 安全测试 + mount/plugin/instance 测试全部保持通过。
- `DW_TEST_SYSTEMD` / 真实 bwrap/rclone E2E 仍通过。

---

## 12. 里程碑

| 阶段 | 内容 | 预计 |
|---|---|---|
| M0 | 更新基线文档；删除 `cmd/dw-local`；补 `docs/local-mode.md` | 0.5d |
| M1 | `StartAsync` + `dw start/stop/status` + login autoStart | 2–3d |
| M2 | TOTP + `dw 2fa-*` + 登录二步 | 2d |
| M3 | Plugin v2（catalog/install/rollback） | 3–4d |
| M4 | allowPrefixes + quotas + audit | 2–3d |
| M5 | 网络隔离 spike / `trustedUsers` 失败关闭 | 2–4d |
| M6 | E2E/chaos/docs/release | 2d |

总计约 2–3 周。M1 是最高优先级，先解决“登录后无法进入”的产品断点。

---

## 13. 风险与待确认

| 风险 | 应对 |
|---|---|
| 两阶段启动引入 Pending 悬挂 | `pendingLimit` + Reconcile 清理；chaos 测试；控制面重启后不自动重试 |
| 并发登录重复启动 | per-user singleflight + Store CAS + 现有 guard |
| `Ensure` 在 readiness 期间持 guard | M1 一并释放；`StartAsync` 不等待 |
| TOTP 依赖选择 | 先用 `pquerna/otp`；不想加依赖则 stdlib RFC 6238 |
| 插件 catalog 被篡改 | sha256 + 管理员权限 + audit；bundle 优先 |
| legacy flat plugin 迁移 | 兼容目录 + 一周期保留 + `dw plugin list` 标记 |
| allowPrefixes 破坏旧配置 | 启动告警 + fail closed；提供迁移脚本提示 |
| 网络隔离复杂度 | 默认 `shared` + `trustedUsers`；`isolated` 先 spike，不阻塞 v0.4 |
| 配额统计并发 | 在 guard 内统计 active 记录；Store CAS |
| 审计泄露 | 字段白名单；不记录 token/password/AK/SK |

### 需要确认

1. `auth.autoStart` 默认值：`true`（登录即用）还是 `false`（保留 v0.3 管理员启动）？
2. 用户之间是否互信？决定 `trustedUsers` 与网络隔离优先级。
3. 插件来源是否只允许管理员预构建 bundle？是否允许私有 npm registry？
4. TOTP 是否强制，还是用户可选？
5. 配额默认值：CPU/内存/mount/plugin 数量。
6. 审计保留天数与是否外发到平台审计。
7. 是否保留 `cmd/dw-local` 作为示例，还是直接删除？

---

## 14. 验收清单（Definition of Done）

- [ ] 新用户登录后自动进入 dsh；无 503 断点；10 并发只启动一次。
- [ ] `dw start/stop/status` 可用；`autoStart=false` 时页面给出明确命令。
- [ ] TOTP 可开启/重置；旧会话失效；限速覆盖两步。
- [ ] 插件可安装/升级/回滚；sha256 校验；catalog 更新可控；用户隔离。
- [ ] bucket mount 受 allowPrefixes 限制；越界 fail loud。
- [ ] 配额在启动/挂载/插件前执行；超额有明确错误。
- [ ] audit JSONL 覆盖关键操作且无 secret。
- [ ] 网络模式在 `dw doctor` 可见；`trustedUsers=false + shared` 启动失败。
- [ ] `go test -race ./...`、`go vet ./...` 通过；现有 v0.3 安全测试无回归。
- [ ] `docs/local-mode.md` 与 README 不再描述过时的平台 ticket 流程。

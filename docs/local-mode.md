# 单机生产模式（local）验证记录

> 代码：`cmd/dw` + `internal/`；配置：[`examples/config.local.yaml`](../examples/config.local.yaml)  
> 测试 dsh：`@deepseek-ai/dsh 0.2.1-rc.2`（npm 安装）  
> 日期：2026-10-27

## 1. 启动

```bash
go build -o dw ./cmd/dw
./dw doctor -c examples/config.local.yaml
./dw serve -c examples/config.local.yaml
```

`dw serve` 常驻；SIGTERM 只关闭网关，不停止已运行的 dsh（控制面重启后 reconcile 采纳）。

## 2. 平台入口

```bash
curl -sS -X POST http://127.0.0.1:7658/api/platform/launch \
  -H 'Authorization: Bearer poc-token' \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice","display_name":"Alice"}'
```

返回一次性 ticket URL。浏览器打开后：

- 303 设置 `dw_session`；
- dsh 的 `?token=` 只由网关交换，浏览器 URL 中没有 token；
- `dsh-auth-*` 不下发浏览器；
- 页面、静态资源、`/plugins`、`/api`、WebSocket 均正常。

已用 Playwright（headless Chromium）验证：

```text
URL   http://alice.localhost:7658/
TITLE DeepSeek Harness
BODY  新会话 / 工作区 / 默认工作区 / ...
```

## 3. Workspace 写边界（P0 关键发现）

dsh 的写边界是 **session cwd**，不是 `sandbox-policy.workspaceRoot`。  
本次落地用：

```yaml
# control/<id>/runtime.patch.yml
- id: workspace-controller
  config:
    documentsDirectory: <workspace>
- id: working-directory
  config:
    defaultDirectory: <workspace>
```

并在 `<workspace>/deepseek-harness/default-workspace` 建符号链接指向 `<workspace>`。  
dsh 的 Workspace registry 会 canonicalize（`realpath`），实测 session header：

```json
{"type":"session","cwd":"/tmp/dw-prod/workspaces/alice"}
{"type":"sandbox/mode","mode":"workspace-write"}
```

因此 agent 的写权限落在 workspace 根，而不是嵌套的 `deepseek-harness/default-workspace`。

## 4. 多用户隔离

- `alice` / `bob` 各自一个 dsh 进程，独立 `DSH_HOME` / workspace；
- `alice.localhost` / `bob.localhost` 子域名；
- 交叉使用 `dw_session` → 401；
- 两个实例端口由端口池分配。

## 5. 持久化

停止 alice 后：

- `homes/alice/profiles/default` 保留；
- `homes/alice/sessions` 保留；
- `workspaces/alice` 保留；
- 重新 launch 后页面 200，旧 `dw_session` 仍然有效（HMAC key 持久化；网关重新交换 dsh cookie）。

控制面 `kill` 后重启：

- 已运行的 dsh 不被杀；
- reconcile 重新 reserve 端口；
- 旧 `dw_session` 仍可访问。

## 6. 已修复的坑

1. `dsh --patch` 是 launcher 参数，必须放在 app 参数（`--no-open` 等）之前；否则报 `unknown option '--patch'`。
2. `--public-url` 会让 dsh 打印公网 URL，不能从该行解析监听端口；端口由 `dw` 端口池分配后显式传入。
3. `dsh` 的 profile 名不能是 `web`（shipped）；本次用 `default`，启动命令 `dsh --profile default`。
4. `dsh-base` 必须在 bundles 第一层，否则没有 session/tools/webserver。

## 7. systemd cgroup 模式（本轮新增）

`runtime.local.limiter` 决定启动器：

| 值 | 行为 |
| --- | --- |
| `auto`（默认） | 能用 `systemd-run --user` 就用 systemd user unit，否则退到 `prlimit`，再否则直接子进程 |
| `systemd` | 必须用 unit；systemd 不可用时启动直接失败，不静默降级 |
| `prlimit` | 保持旧的 `prlimit --as/--cpu --` 路径（`prlimit` 不存在则直接子进程） |
| `none` | 直接子进程 |

`runtime.local.systemdUser` 为三态 `auto|always|never`（也接受 `true/false`）。
`never` 完全不探测/调用 systemd；`always` 跳过探测直接建 unit。
矛盾组合在 `config.Validate()` 就报错：
`limiter: systemd` + `systemdUser: never`、`limiter: prlimit|none` + `systemdUser: always`。

unit 模式的关键属性：

```
systemd-run --user --unit=dw-<slug>-<inc8>.service --quiet \
  -p CPUQuota=<cpu*100>% \
  -p MemoryMax=<bytes> -p MemorySwapMax=0 \
  -p TasksMax=<tasksMax> \
  -p KillMode=control-group \
  -p TimeoutStopSec=<stopGrace> \
  -p WorkingDirectory=<workspace> \
  -p StandardOutput=append:<log> -p StandardError=append:<log> \
  -p ExecStopPost=-/bin/sh -c "echo $EXIT_STATUS $SERVICE_RESULT > '<log>.verdict'" \
  --setenv=PATH=... --setenv=HOME=... --setenv=DSH_HOME=... \
  -- <dwBin> instance --spec <controlDir>/spec.json
```

- **不用 `--scope`**：scope 会阻塞客户端直到进程退出，且没有可按名字停止的 unit。`--unit` 让控制面重启后仍能用持久化的 `backendRef` 调 `systemctl --user stop`。
- **unit 的主进程始终是 `dw instance`，不是 dsh**：dsh 的 `?token=` 只能经过 agent 的 redactor 写日志。
- **不用 `RLIMIT_NPROC`**：它按真实 UID 全机计数，在登录节点上会把 dsh 的 `clone()` 全部打成 `EAGAIN`。fork 炸弹防护由 `TasksMax=` 承担。
- **`MemorySwapMax=0`**：否则 `MemoryMax` 是软限制，dsh 会 swap 而不是被 OOM kill。
- **`StandardOutput=append:`**：unit 不继承我们的 fd，日志必须由 systemd 打开；启动前 backend 先把日志以 `0600` 创建好，systemd 的 `append:` 会沿用该权限。
- **verdict 文件**：`<log>.verdict` 记录 `<EXIT_STATUS> <SERVICE_RESULT>`。dsh 被 OOM kill 时 agent 和 dsh 一起死，只有该文件能解释死因；`Endpoint()` 会把它带进错误信息，避免白等 `readyTimeout`。
- **stop 不预探测**：`systemctl --user stop` 是第一条命令；“unit 不存在”按成功处理，只有 DBus 等真实错误才返回。停止后 `reset-failed` 释放 unit 名。
- **`activating|deactivating|reloading` 不算死**；unit 模式连续 3 次负读数才认定 gone，避免一次 `systemctl` 抖动误删活实例。

### 7.1 运维前置：linger

unit 是 user manager 的 transient unit，注销后没有 linger 会一起消失：

```bash
loginctl enable-linger "$USER"
./dw doctor -c examples/config.local.yaml   # linger: yes
```

### 7.2 查看与排障

```bash
systemctl --user status 'dw-alice-*'
journalctl --user -u dw-alice-3f2a1b9c.service
systemctl --user show dw-alice-3f2a1b9c.service \
  -p LoadState -p ActiveState -p MainPID -p Result -p ExecMainStatus
cat /tmp/dw-prod/logs/alice-default.log.verdict
```

`dw instance` 自己的 fd 1/2（启动早期错误、panic）由 systemd 写进同一个日志；dsh 的 stdout/stderr 仍由 agent 写。若要把 unit 自身输出交给 journald，设 `unitStdio: journal`。

### 7.3 回退路径的可见差异

实例记录的 `limiter` / `backendRef` / `command` 字段会写明实际使用的模式：

- `limiter: systemd` + 非空 `backendRef`：cgroup 限制生效；
- `limiter: prlimit`：只有 `RLIMIT_AS`（虚拟地址空间，不是 RSS）和可选的累计 `RLIMIT_CPU`；
- `limiter: none`：无资源限制。

`prlimitCPUSeconds` 默认 `0`（不设 `RLIMIT_CPU`）。旧实现固定 `CPU*3600`，对长驻服务意味着 `cpu: 2` 的实例在约 2 CPU 小时后被 SIGXCPU 杀掉；如需安全上限请显式配置。

### 7.4 本轮本机验证

```text
$ ./dw doctor -c examples/config.local.yaml
config: ok (examples/config.local.yaml)
dsh: ok (/opt/dw/bin/dsh)
dist default: ok (.../examples/dist/default/1.0.0/profile, profile=default)
dw bin: ok (.../dw)
systemd --user: ok (systemd-run --user --scope ok)
systemd: systemd 255 (255.4-1ubuntu8.18)  cgroup: v2
linger: yes
limiter: auto -> systemd (systemdUser=auto)
tasksMax: 512
readyTimeout: 90s (unit start 10s, stopGrace 20s, unitStdio=log)

$ DW_TEST_SYSTEMD=1 go test ./internal/runtime/local -run TestRealSystemdUnitLifecycle -v
--- PASS: TestRealSystemdUnitLifecycle (0.50s)

$ DW_TEST_SYSTEMD=1 DW_TEST_SYSTEMD_DW=$PWD/dw go test ./internal/runtime/local -run TestRealSystemd -v
--- PASS: TestRealSystemdDwInstance (2.38s)
--- PASS: TestRealSystemdUnitLifecycle (0.50s)
```

集成测试覆盖：真实 unit 启动、控制面“重启”后用 `backendRef` 采纳、按 unit 名停止、同一 unit 名二次启动；`TestRealSystemdDwInstance` 还用真实的 `dw instance` 作为 unit 主进程，验证 dsh 的 `?token=` 仍被 agent redactor 挡住。

## 8. 当前边界

- systemd 模式需要可用的 user manager（`systemd-run --user`）和 cgroup v2；`MemorySwapMax` 在纯 cgroup v1 上会失败。
- 回退路径没有 RSS 级内存上限，也没有 fork 炸弹防护（`RLIMIT_NPROC` 不可用），记录里的 `limiter` 字段会如实标注。
- 运行时只有本机进程：每个用户一个 dsh，由 systemd user unit 或普通子进程拉起。
- 平台鉴权为 Bearer token；HMAC/ticket 持久化接口已留。

## 9. 安全与运维约束（本轮加固）

- `dw serve` 对 dataDir 持有单实例锁（`locks/serve.lock`）：第二个控制面进程直接退出，避免两份进程互相覆盖实例记录与端口。
- 实例进入 `stopping` 后不会立即释放端口/句柄；停止失败按 `scanInterval`（≥10s）重试，最多 5 次后记录 `orphan suspected`。记录为 `stopping` 时再次 launch 返回 409 + `Retry-After`。
- 端口按 incarnation 归属：控制面重启会为所有活跃记录重新 reserve，旧 incarnation 不能释放新进程的端口。
- `dw del <user>` 写 tombstone（`deleted: true`，清空口令/挂载/插件并推进 `session_version`）；同名 `dw user` 重建继承更高代次，删除前的 `dw_session` 永久失效。
- workspace 只能落在 `<dataDir>/workspaces/<slug>` 或 `server.workspaceRoots` 内；挂载 host 路径做 realpath 并校验根目录，路径挂载目标不能是 `/workspace`。
- 网关对 `/login`、`/_dw/launch`、`/_dw/plugins`、`/_dw/mounts` 和 `/` 校验 Host（仅 apex 或一级子域），其它 Host 返回 421；`/_dw/launch` 只对有效 ticket 写 `.used`，过期 ticket 由 janitor 清理。
- 登录失败按 IP 与 IP|用户名 限速（默认 10 次 / 5 分钟、锁定 5 分钟，返回 429 + `Retry-After`），成功登录清零。
- 反向代理场景设置 `routing.publicScheme: https` + `routing.publicPort: 443`（需要时加 `trustProxyHeaders: true`）；否则实例 URL 会带 `:7658`，Origin 校验也会按 http 端口比较。

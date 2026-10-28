# DeepWarper 方案（v0.3 规划）

> **一句话**：把“定制好的 DeepSeek Harness Web”做成一个可分发的运行时，一次构建、按需启动；每个用户/项目拿到一个带自己工作区、数据挂载、bucket 挂载和资源上限的 dsh 实例，通过平台统一入口访问。
>
> 项目代号：`deepwarper`，二进制：`dw`。
> 实现状态（2026-10-27）：**local（含 systemd cgroup）与 SGE 后端已落地**；SGE 为 fake Runner/Tunnel 验证（`docs/sge-mode.md`），真实集群待站点配置；FUSE/k8s 仍按本文规划推进。

---

## 0. 结论（TL;DR）

1. **一个 Go 单二进制 `dw`**：控制面 + 网关 + 生命周期管理；不引入数据库、CRD、Operator。
2. **一个运行时镜像**：Node 22 + 固定版本 dsh + 预装好的插件 profile；运行时不跑 `pnpm`、不联网装插件。
3. **一份 `distribution.yaml`**：声明“这组插件 + 这些补丁 + 默认资源/挂载”，构建成不可变发行版。
4. **首发生产后端是 SGE**（已确认控制面在登录节点、登录节点到计算节点 SSH 可用）；`local` 用于开发/单机；`k8s` 是适配器，等集群能力确认后再启用。
5. **挂载模型**：管理员声明 storage roots，用户只能在这些根下选子路径；`/workspace` 是唯一可写工作区，参考数据/bucket 默认只读。
6. **bucket 支持 TOS / S3，允许 FUSE**：SGE 首发用 FUSE（rclone/tosfs）或预挂载；k8s 后续用 CSI；`dw` 本身不把密钥写进实例记录。
7. **访问入口默认子域名**：`https://<user>.dsh.example.com/`；同时也保留路径前缀模式作为开发/兜底。网关做 dsh 的 **launch-token broker**、Host/Origin 处理、WebSocket 透传、尾斜杠重定向。
8. **一个用户只能有一个活跃实例**：`DSH_HOME` 按用户持久化，停止/重启后 dsh 会话、模型配置、API key 都还在。

这套方案的部署复杂度目标是：

```text
构建：CI 里跑一次 dw dist build
部署：一个二进制 + 一个 YAML + 一个运行时镜像
运行：平台点“打开”，dw 起一个实例，网关转发
```

### 0.1 本轮已确认的事实与影响（2026-10-25）

| 事项 | 确认结果 | 对方案的影响 |
|---|---|---|
| 平台入口 | **子域名**（每个用户一个） | 默认 `routing.mode: host`；保留 `path` 作为开发/兜底 |
| 入口能力 | 平台反代按子域名转发；WebSocket 需确认 | `dw` 自己处理尾斜杠 308 和 WebSocket 升级；提供平台反代最小契约和 `dw doctor ingress` |
| k8s 权限 | **暂不确定**能否建 Pod、是否有 RWX PVC / bucket CSI | k8s 不作为 MVP 阻塞项；先做 `dw doctor k8s` 能力探测，SGE 先上线 |
| SGE | 控制面在登录节点；登录节点到计算节点 SSH 可用；站点参数通过配置文件提供 | **SGE 作为首发生产后端**；queue/PE/apptainer/SSH/共享路径全部来自 `config.yaml`，不硬编码 |
| bucket | **TOS / S3**；**允许 FUSE** | 优先 `tos://` / `s3://`；SGE 首发用 FUSE（rclone/tosfs），premount 兜底；k8s CSI 后续 |
| 平台鉴权 | 平台控制登录；按按钮时把用户信息交给 deepwarper | 不要求平台理解“身份头”；提供 server-to-server launch API + 一次性 ticket |
| 首批 distribution | 暂不限定 | distribution 设计保持通用，先做一个最小 `default` 验证闭环 |
| 模型 | DeepSeek 公网 API；**用户可以自己配置** | 不内置共享 key；用户 key 存 `$DSH_HOME/.credentials.yaml`；distribution 只给默认 provider/model |
| 实例数 | **一个用户只能有一个实例** | 状态表以 user 唯一；`flock` 锁 home；重复创建返回现有实例 |
| 用户数据/配置 | 必须保存并可复启用 | `DSH_HOME` 按用户持久化；profile 按 distribution 分目录；停止不删 home；distribution 升级显式备份 |

---

## 1. 目标与非目标

### 1.1 目标

- 科技服务公司的云工具平台上，用户点一下就能进入自己的 dsh Web。
- 插件集可按客户/业务线定制，且**可版本化、可复现、可审计**。
- 支持宿主路径挂载、共享参考数据挂载、bucket 路径挂载。
- 支持 k8s 与 SGE 两种资源后端，资源上限由后端执行。
- 每个用户/项目的数据、会话、工作区互相隔离。
- 管理员不需要写 k8s YAML / qsub 脚本就能管理实例。
- 部署简单：单二进制、文件系统即数据库、无外部依赖。

### 1.2 非目标（MVP 明确不做）

- ❌ 不做通用 dsh 插件市场 / 在线安装插件。
- ❌ 不做多个用户共享同一个 dsh 进程。
- ❌ 不做通用 IDE、桌面应用、Jupyter/RStudio 托管。
- ❌ 不做 bucket 文件浏览器 / 通用网盘；bucket 只以目录形式挂进实例。
- ❌ 不替换平台已有的 SSO、计费、审计系统；只对接。
- ❌ 不做多控制面 HA（先单副本；文件锁 + 启动 reconcile 保证可恢复）。
- ❌ 不做条件分支/流程编排；dsh 自己就是 agent 运行时。

---

## 2. 使用场景

### 2.1 场景 A：生信项目助手（主场景）

用户 `alice` 在云工具平台打开“DeepSeek 生信助手”：

- 工作区：`/data/projects/P123/alice` → 容器内 `/workspace`，读写。
- 参考数据：`/data/ref/hg38` → 容器内 `/data/ref`，只读。
- bucket：`s3://bio-raw/P123/fastq` → 容器内 `/data/raw`，只读。
- 插件集：`bio-v1`（生信 skills、公司知识库 MCP、Office/报告插件、默认 DeepSeek provider）。
- 模型：默认 DeepSeek 公网 API；用户在 dsh 设置里填自己的 API key，key 存在自己的 `DSH_HOME`，不共享。
- 资源：4 核 / 16 GiB / 最长 8 小时；空闲 30 分钟回收。
- 访问：`https://alice.dsh.example.com/`（平台按钮代入用户信息后跳转）。
- 关闭页面 30 分钟后实例停止；下次再打开，dsh 会话、模型设置、`DSH_HOME` 都还在。

### 2.2 场景 B：多项目 / 多客户

- 每个客户一个 distribution（插件集、persona、可用 MCP、默认模型不同）。
- 每个项目一组 storage root 和配额。
- 用户只能在授权 root 下选子路径；只读数据无法被 agent 改写。
- 管理员能看到“谁、何时、哪个 distribution digest、挂了哪些路径、用了多少资源”。

### 2.3 场景 C：管理员

- 上传/构建新的 distribution 版本，设置默认资源模板和挂载模板。
- 查看实例列表、状态、日志、资源使用，强制停止。
- 不需要登录 k8s / 提交节点。
- 升级 dsh 或插件时，新实例用新版本，旧实例保持 pinned。

### 2.4 场景 D：开发/调试

- 开发者本机 `dw serve --runtime local`，挂本地目录，改 patch 后重启实例。
- 不依赖 k8s/SGE，能把网关、profile、挂载、token broker 全链路跑通。

### 2.5 场景 E：自动化（后续）

- dsh 的 `headless` / `sdk` profile 也可用同一套 distribution 和运行时启动。
- MVP 只做 `web`，但 backend 接口不写死 web。

---

## 3. 总体架构

```text
                         ┌──────────────────────────────┐
                         │ 云工具平台 / 门户 / SSO       │
                         └──────────────┬───────────────┘
                                        │ 按钮 → /api/platform/launch
                                        │ 平台 token/HMAC + user info
                                        ▼
┌───────────────────────────────────────────────────────────────────┐
│ dw serve（单二进制）                                               │
│                                                                   │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐  │
│  │ Web/API     │  │ Gateway      │  │ Instance Manager         │  │
│  │ 实例列表/创建│  │ <user>.dsh…  │  │ create/stop/reap/reconcile│  │
│  └─────────────┘  │ token broker │  └────────────┬─────────────┘  │
│                   │ Host/Origin  │               │                │
│                   │ WebSocket    │               │                │
│                   └──────┬───────┘               │                │
└──────────────────────────┼───────────────────────┼────────────────┘
                           │                       │
             ┌─────────────▼─────────┐   ┌─────────▼──────────────┐
             │ 运行时后端             │   │ 状态/审计（文件系统）   │
             │ local / k8s / sge     │   │ data/instances/*.yaml  │
             └──────┬───────┬────────┘   │ data/audit/*.jsonl     │
                    │       │            └────────────────────────┘
          ┌─────────▼─┐   ┌─▼──────────┐
          │ k8s Pod   │   │ SGE Job    │
          │ 条件启用  │   │ 首发生产   │
          │ dsh web   │   │ + apptainer│
          │ + dw agent│   │ + ssh -L   │
          └─────┬─────┘   └─────┬──────┘
                │               │
                ▼               ▼
        ┌────────────────────────────────┐
        │ DSH_HOME（持久化）              │
        │ workspace / data / bucket 挂载  │
        └────────────────────────────────┘

构建面（CI，不在运行时）：
distribution.yaml + 插件包 + patches
        │
        ▼
dw dist build ──► 运行时镜像（dsh + profile + node_modules）
                └─► manifest.json（版本、digest、dsh 版本）
```

关键设计取舍：

- **控制面不碰容器**，只通过 `Backend` 接口启动/停止；代理层不碰容器，只通过实例记录里的 endpoint 转发。
- **文件系统即数据库**：实例、distribution、审计都是 YAML/JSONL；启动时 reconcile。
- **一次构建，运行时不装插件**：避免运行时依赖公网 npm、避免版本漂移。

---

## 4. 分发：一次构建，处处启动

### 4.1 `distribution.yaml`

一个 distribution 就是“一组插件 + 补丁 + 默认策略”的不可变快照。

```yaml
apiVersion: deepwarper/v1
kind: Distribution
metadata:
  name: bio-v1
  version: 1.0.0
  description: 生信分析助手（scRNA / 报告 / 内部知识库）

spec:
  dsh:
    version: 0.2.1-rc.2          # 必须精确锁定
    # profile 名 = distribution name（bio-v1）；构建时创建 profiles/bio-v1

  # dsh profile 的 bundle 顺序
  bundles:
    - "@deepseek-ai/dsh-base"     # 必须第一层：session/tools/sandbox/webserver
    - "@deepseek-ai/dsh-web-app"
    - "company-dsh-bio"           # 公司自定义 bundle（本地包或私有 npm）

  # 额外插件包；构建时 pnpm add，运行时不安装
  packages:
    - "file:./plugins/company-dsh-bio"
    - "@deepseek-ai/dsh-skill-office@0.2.1-rc.2"

  # 按顺序叠加的 Cordis patch
  patches:
    - patches/00-company-base.patch.yml
    - patches/10-bio-tools.patch.yml
    - patches/20-internal-mcp.patch.yml

  # 技能目录，挂到 $DSH_HOME/skills 或 customSkillDirs
  skills:
    - skills/scRNA
    - skills/report

  # 种子 home（AGENTS.md、默认设置等）
  homeSeed: home/

  defaults:
    resources:
      cpu: 2
      memory: 4Gi
      walltime: 8h
    idleTTL: 30m
    mounts:
      - name: workspace
        target: /workspace
        mode: rw
        source: user
      - name: ref
        target: /data/ref
        mode: ro
        source: storage:ref
    env:
      - name: DEEPSEEK_BASE_URL
        value: "https://llm-gateway.internal/v1"
```

### 4.2 构建产物

`dw dist build` 在 CI 里执行（需要 Node 22 + pnpm）：

```text
/opt/dw/dist/bio-v1/1.0.0/
  manifest.json              # dsh 版本、插件 lock hash、构建时间、git commit
  profile/
    package.json             # dsh.profile.bundles + dependencies
    pnpm-lock.yaml
    cordis.patch.yml
    node_modules/            # pnpm install --prod 的结果
  patches/
  skills/
  home-seed/
```

构建步骤：

1. 生成 profile 的 `package.json`（bundle 顺序 + 依赖）。
2. `pnpm install --frozen-lockfile`。
3. 拷贝 patches、skills、home-seed。
4. 运行 `dsh --profile <distribution> --dump-config` 校验组合。
5. 做一次 headless smoke boot（不调模型），确认必需插件都能加载。
6. 写 `manifest.json`，计算 profile 内容 digest。

> 运行时镜像里只带 `node`、dsh 构建产物、`/opt/dw/dist` 和 `dw` 二进制，**不带 pnpm**。

### 4.3 每个用户的 `DSH_HOME`

**一个用户一个 home，一个 distribution 一个 profile 目录**。这样用户换 distribution 时，模型 key、会话、技能和个人偏好仍在同一个 home 里；同时一个用户只能有一个活跃实例，避免 dsh 的 SQLite/settings 并发写。

```text
/data/dw/homes/<user>/
  profiles/
    <distribution>/                 # 例如 bio-v1；用 `dsh --profile bio-v1` 启动
      package.json                  # 从 dist 复制（小文件）
      pnpm-lock.yaml                # 从 dist 复制
      cordis.patch.yml              # dist 默认值 + 用户设置（可写、用户可改）
      node_modules -> /opt/dw/dist/<dist>/<version>/profile/node_modules
  cordis.patch.yml                  # home 级 patch（可选，跨 profile 生效）
  .credentials.yaml                 # 用户自己的 DeepSeek API key / dsh cookie secret
  sessions/                         # dsh 会话（跨 distribution 保留）
  skills/                           # 用户自己的技能（跨 distribution 保留）
  AGENTS.md                         # 用户/公司指令
  .dw.lock                          # flock：同一 home 只允许一个 dsh 进程
  .dw-state.json                    # 当前活跃 distribution、pinned 版本
```

为什么要复制小文件、只 symlink `node_modules`：

- `node_modules` 很大，按 distribution 版本共享，节省磁盘。
- `package.json` / `cordis.patch.yml` 是用户设置会写入的文件，复制后每个用户可改。
- 每个 profile 记录自己 pinned 的 distribution 版本；升级不自动影响旧用户。
- `.credentials.yaml` 是 home 级的，所以用户换 distribution 后 API key 还在；但不同 profile 的模型/工具设置可以不同。
- 一个 home 一个 `flock`，保证“一个用户一个实例”不是靠 UI 约定，而是靠文件锁。

> 注：dsh 的 profile 名不必是 `web`。只要 profile 的 `dsh.profile.bundles` 里有 `@deepseek-ai/dsh-base` 和 `@deepseek-ai/dsh-web-app`，就可以用 `dsh --profile <distribution> --host ... --port ...` 启动 Web。**已在 0.2.1-rc.2 上验证自定义 profile 名启动。**

### 4.4 分层顺序

dsh 本身的层顺序（从低到高）：

```text
shipped bundles
  → distribution profile/cordis.patch.yml
  → $DSH_HOME/cordis.patch.yml
  → 实例级 runtime.patch.yml（dw 生成，最后应用）
```

profile 名使用 distribution 名，启动命令为：

```bash
# host 模式（默认）
dsh --profile <distribution> --host 127.0.0.1 --port <port> --no-open \
  --public-url https://<user>.dsh.example.com/ \
  --trusted-host <user>.dsh.example.com \
  --patch /data/instances/<id>/runtime.patch.yml

# path 模式（兜底）：不需要 --trusted-host，网关会改写 Host/Origin
dsh --profile <distribution> --host 127.0.0.1 --port <port> --no-open \
  --public-url https://tools.example.com/dsh/i/<id>/ \
  --patch /data/instances/<id>/runtime.patch.yml
```

实例级 patch 只放“这次运行才知道”的值：

```yaml
# data/instances/<id>/runtime.patch.yml
- id: workspace-controller
  config:
    documentsDirectory: /workspace

- id: working-directory
  config:
    defaultDirectory: /workspace
```

注意：**不要 patch `sandbox-policy` 来当写边界**。dsh 的写边界是 session cwd；`workspaceRoot` 只是 agentless 的 fallback。`dw` 让 dsh 进程 `cwd=/workspace`，并通过 `workspace-controller.documentsDirectory` + 默认工作区符号链接把 session cwd 落到 `/workspace`。

`publicUrl` / `openBrowser` / `printUrl` 由 `dw instance` 启动 dsh 时用 CLI 参数传，不写进 patch，避免覆盖 distribution 里的默认配置。

### 4.5 定制插件集的两种方式

**方式 1：只选内置能力**（不改代码）

在 `patches/` 里写 Cordis patch：

- 默认模型 / provider
- `mcp-client` 接内部知识库
- `skill-filesystem` 加公司技能目录
- persona、工具开关、sandbox 默认模式
- web search / office / 其他内置插件

**方式 2：公司自定义 bundle**（推荐有代码时）

写一个 npm 包：

```json
{
  "name": "company-dsh-bio",
  "version": "1.0.0",
  "type": "module",
  "dsh": { "bundle": { "patch": "./cordis.patch.yml" } }
}
```

`cordis.patch.yml` 里 `insert` 公司自己的插件行。distribution 构建时把它作为 profile 依赖安装。

> 原则：**插件集是构建产物，不是运行时状态**。这样才能做到可复现、可审计、可回滚。

---

## 5. 实例与挂载

### 5.1 实例请求

```yaml
apiVersion: deepwarper/v1
kind: Instance
metadata:
  user: alice
  distribution: bio-v1

spec:
  runtime: sge                  # 首发生产；k8s 待能力确认
  workspace:
    source: path:/data/projects/P123/alice
    target: /workspace
    mode: rw
  mounts:
    - source: path:/data/ref/hg38
      target: /data/ref
      mode: ro
    - source: bucket:bio-raw/P123/fastq
      target: /data/raw
      mode: ro
  resources:
    cpu: 4
    memory: 16Gi
    walltime: 8h
    gpu: 0
  idleTTL: 30m
  env:
    PROJECT_ID: P123
```

`dw` 会做三件事：

1. 把 distribution defaults 与请求合并（请求只能收窄，不能突破上限）。
2. 校验每个 source 是否在管理员声明的 storage root 内。
3. 生成后端 spec：pod/job + mounts + env + runtime patch。

### 5.2 Storage roots（管理员声明）

```yaml
# storages.yaml
roots:
  - id: projects
    type: path
    host: /data/projects
    allowRw: true
    description: 项目工作区

  - id: ref
    type: path
    host: /data/ref
    allowRw: false
    description: 共享参考数据

  - id: bio-raw
    type: bucket
    provider: fuse              # fuse（首发）| premount | csi
    uri: tos://bio-raw          # 也支持 s3://...
    endpoint: tos-s3-cn-beijing.volces.com
    region: cn-beijing
    allowPrefixes:
      - "P123/"
      - "public/"
    allowRw: false
    fuse:
      readOnly: true
      vfsCacheMode: off
    premount:
      host: /mnt/bio-raw        # premount 模式：节点上已挂好的路径
    k8s:
      pvc: bio-raw-csi          # CSI 模式使用
```

### 5.3 挂载规则

| 类型 | 目标路径 | 模式 | 说明 |
|---|---|---|---|
| 主工作区 | `/workspace` | rw | 唯一可写根；dsh 的 session cwd 指向它 |
| 参考数据 | `/data/<name>` | ro | 可读不可写；dsh 的 fs 工具写不进去，挂载本身也是 ro |
| bucket | `/data/<name>` 或 `/bucket/<name>` | ro | 默认只读 |
| 额外的 rw 挂载 | `/workspace/_ext/<name>` | rw | 必须嵌套在 workspace 下，否则 dsh 的 workspace-write 不授权写 |

> dsh 的写入边界是 **session cwd**（不是 `sandbox-policy.workspaceRoot`）；读不受限。因此只读数据放在 `/data` 没问题，但**任何需要 agent 写入的目录都必须位于 `/workspace` 下**。`dw` 用 `workspace-controller.documentsDirectory` + 默认工作区符号链接把 session cwd 落到 `/workspace`。

如果没有指定 workspace，`dw` 默认创建并挂载 `/data/dw/workspaces/<user>` 到 `/workspace`；这个目录同样是持久化的，不会随实例停止而删除。

### 5.4 路径安全

所有用户可控路径必须过同一套校验：

- source 必须是已声明 root + 相对子路径；
- 拒绝绝对路径、`..`、空段、控制字符；
- `filepath.Clean` 后再 `EvalSymlinks`，确认仍在 root 内；
- 只 bind **精确路径**，不 bind 父目录（避免把同组其他项目暴露给实例）；
- `ro` root 不允许请求 `rw`；
- bucket 的 key/prefix 必须在 `allowPrefixes` 内；
- 目标路径必须在允许的目标清单里（`/workspace`、`/data/<declared-name>`）。

### 5.5 bucket 挂载：TOS / S3（允许 FUSE）

`dw` 只接受 bucket 地址 + 子路径，不直接调用 S3/TOS SDK，也不把 AccessKey 写进实例记录：

```text
tos://bio-raw/P123/fastq     →  /data/raw
s3://bio-ref/hg38            →  /data/ref
```

具体怎么变成实例里的目录，由 `provider` 决定：

| provider | 适用 | 说明 |
|---|---|---|
| `fuse`（首发可用） | SGE / k8s | Job 宿主侧用 rclone/tosfs mount，再 bind 进容器；需要 `/dev/fuse`；用户已确认允许 |
| `premount` | SGE / k8s | 管理员预先挂到共享路径或节点；`dw` 只 bind |
| `csi` | k8s | CSI PVC；等集群能力确认 |

**SGE 首发策略：FUSE 优先，premount 作为兜底。**

1. `dw` 在 job 脚本里、启动 apptainer 之前，用 `rclone mount` 把 `tos://` / `s3://` 挂到 job 的临时目录；
2. 再用 `apptainer --bind /tmp/dw-mnt/<bucket>:/data/raw:ro` 把它交给 dsh；
3. dsh 退出时 `trap` 卸载，避免计算节点残留 mount；
4. 如果 `/dev/fuse` 不可用或 mount 失败，**job 直接失败并写清楚原因**，不静默跳过 bucket。

> 也可以把 rclone 放进容器内 mount，但 apptainer 需要能访问 `/dev/fuse`；MVP 建议在 job 宿主侧 mount，再 bind 进容器，少一层不确定性。

**TOS / S3 的配置：**

- `s3://`：endpoint、region、access key/secret（或实例角色/临时凭证）。
- `tos://`：endpoint/region、AccessKey/Secret；可使用 TOS 的 S3 兼容端点，或站点提供的 `tosfs`/rclone 后端。具体二进制和参数由 `config.yaml` 指定，不写死在代码里。
- 凭据放在计算节点可读的 0600 文件（或环境变量），**不进入实例记录、日志、浏览器**。
- 每个用户/实例用自己的 rclone config，只包含该实例需要的 remote/prefix；不要共享全量 bucket 凭据。
- rclone config 留在宿主侧，只把挂载点 bind 进容器；dsh 看不到 AccessKey。
- 一个实例只 mount 自己请求的 bucket prefix；不要 mount 整个 bucket 根。

**挂载规则：**

- 默认只读；读写 bucket 必须挂到 `/workspace` 下，并提示一致性和性能风险；
- bucket 名 + prefix 必须在管理员声明的 `allowPrefixes` 内；
- FUSE 只建议在受信任的计算节点/专用节点开启；不要把任意 bucket 暴露给任意用户；
- mount 失败、权限不足、prefix 越界都要 fail loud。

### 5.6 OS 用户与权限（重要）

MVP 不创建 per-user Linux 账号：所有 dsh 实例都以 `dw serve` 的 OS 用户（或 SGE 提交用户）运行。这和 srcos 的模型一致，隔离靠**容器/挂载命名空间 + 每实例只 bind 自己的路径**，不靠 Unix UID。

因此部署前要确认：

- `/data/projects/...` 对运行用户/组可读；需要写的 workspace 对运行用户可写。
- 共享目录用 setgid + group ACL（或平台已有的 ACL 方案），不要依赖“每个人自己的 UID”。
- `DSH_HOME` 只 bind 当前用户目录；不要把 `/data/dw/homes` 父目录 bind 进实例。
- 如果客户强制要求 OS UID 隔离：k8s 用 `runAsUser`/`fsGroup`；SGE 需要 per-user OS 账号 + `sudo -u` 或队列映射，这会显著增加复杂度，**不在 MVP**。
- 同一 OS 用户意味着控制面能读所有 home；控制面必须做好 API 授权，审计不可少。

---

## 6. 运行时后端

### 6.1 统一接口

```go
type Backend interface {
    Name() string
    Start(ctx context.Context, spec StartSpec) (Handle, error)
    Stop(ctx context.Context, h Handle) error
    Reconcile(ctx context.Context) ([]Observation, error)
    Logs(ctx context.Context, h Handle, follow bool) (io.ReadCloser, error)
}

type Handle interface {
    Ref() string                     // pod name / SGE job id
    Endpoint() (host string, port int, ok bool)
    Alive() bool
}

type StartSpec struct {
    InstanceID   string
    Image        string
    Argv         []string
    Env          []string
    WorkDir      string
    Mounts       []Mount
    Resources    Resources
    ControlDir   string             // 共享的 spec/ready 目录
    LogPath      string
}
```

控制面只依赖这个接口；dsh 的细节（token、profile、patch）在 `dw agent`/`dw instance` 里，不在 backend 里。

### 6.2 `local`（开发 / 单机）

- 用 `systemd-run --user` 起瞬时 unit，拿 cgroup 的 CPU/内存限制；没有 user systemd 时降级为普通进程 + `prlimit`。
- dsh 监听 `127.0.0.1:<port>`，端口从端口池分配。
- 写 `ready.json` 到 `data/control/<id>/`。
- 挂载用符号链接/直接路径（开发环境不强行做 mount namespace）。
- 适合开发调试和“单机小规模”部署；不作为多租户生产隔离方案。

#### 单机模式合理吗？

**合理，但要把定位说清楚：单机模式是“一台机器上的多用户多实例”，不是“一个 dsh 进程服务多个用户”。**

- 每个用户仍然是**独立的 dsh 进程**、独立的 `DSH_HOME`、独立的 workspace；不会共享同一个 dsh 会话。
- 资源限制：Linux 用 `systemd-run --user`（CPUQuota/MemoryMax）或 `prlimit` 降级；macOS 开发机没有 cgroup，只能做进程级管理。
- 隔离边界：MVP 所有实例还是同一个 OS 用户，靠“独立 home/workspace + 只 bind 自己的路径 + dsh 自己的 sandbox”隔离；**这不是强多租户安全边界**。如果用户之间不可信，单机模式应升级为容器/per-user UID，或者直接用 SGE/k8s。
- 单机模式仍然复用同一套子域名路由、平台 ticket、token broker、持久化和一用户一实例规则；只是 backend 从 SGE/k8s 换成 local。
- 适合：本机验证、开发调试、小团队内部使用、单台 GPU/大内存工作站。
- 不适合：对外多租户、需要强隔离/强配额的场景。

**本机已验证（POC）**：

- 同一台机器上 `alice` / `bob` 各自一个 dsh 进程，独立 `DSH_HOME`/workspace。
- `alice.localhost` / `bob.localhost` 子域名路由，平台 ticket 进入。
- dsh launch token 只由网关持有；浏览器只拿到 `dw_session`。
- 静态资源、API、WebSocket 升级都通过网关。
- 停止/重启后 `profiles/web` 和 sessions 文件仍在；交叉使用对方 cookie 返回 401。
- 代码在 `cmd/dw-local/`，验证记录见 `docs/poc-local.md`。

### 6.3 `k8s`（条件启用，待集群能力确认）

> 当前还不知道控制面能不能建 Pod、有没有 RWX PVC / bucket CSI。因此 **k8s 不是 MVP 的阻塞项**：先实现接口和 `dw doctor k8s`，等集群权限确认后再启用。如果只能建 Deployment、不能建 Pod，则 k8s 后端直接报“不满足能力”，不要退回共享进程方案（那会破坏每实例资源隔离）。

**能力探测（`dw doctor k8s`）**

- 能否 create/delete Pod？
- 能否 get/list Pod、get logs？
- 是否有 RWX PVC 可挂多个节点？
- bucket 是否有 CSI？
- NetworkPolicy 是否可用？
- 节点是否允许非 root、只读 rootfs、drop capabilities？

**控制面**

- 以 Deployment 跑在集群内，ServiceAccount 只需要：
  - `pods` create/get/list/watch/delete/log
  - `pods/log` get
  - 可选 `events` list
- 不需要 CRD、Operator、Aggregator。

**实例**

一个实例 = 一个 Pod（`restartPolicy: Never`），不是 Deployment/StatefulSet：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: dw-<instance-id>
  labels:
    app: dw-instance
    dw/instance: <instance-id>
    dw/user: <user>
    dw/dist: <distribution>
spec:
  restartPolicy: Never
  serviceAccountName: dw-instance
  terminationGracePeriodSeconds: 10
  containers:
    - name: dsh
      image: registry.example.com/dw-runtime:bio-v1-1.0.0
      command: ["/usr/local/bin/dw", "instance", "--spec", "/dw/control/<id>/spec.json"]
      ports:
        - containerPort: 3080
      env:
        - name: POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: DSH_HOME
          value: /home/dsh
      resources:
        requests: { cpu: "4", memory: "16Gi", ephemeral-storage: "10Gi" }
        limits:   { cpu: "4", memory: "16Gi", ephemeral-storage: "10Gi" }
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: ["ALL"] }
        seccompProfile: { type: RuntimeDefault }
      volumeMounts:
        - { name: control, mountPath: /dw/control }
        - { name: home, mountPath: /home/dsh, subPath: homes/<user> }
        - { name: projects, mountPath: /workspace, subPath: <project-subpath> }
        - { name: ref, mountPath: /data/ref, readOnly: true }
        - { name: tmp, mountPath: /tmp }
  volumes:
    - name: control
      persistentVolumeClaim: { claimName: dw-control }
    - name: home
      persistentVolumeClaim: { claimName: dw-homes }
    - name: projects
      persistentVolumeClaim: { claimName: projects }
    - name: ref
      persistentVolumeClaim: { claimName: ref }
    - name: tmp
      emptyDir: {}
```

要点：

- dsh 必须绑 Pod IP：`--host $POD_IP --port 3080`。dsh 不接受 `0.0.0.0`。
- 控制面通过 Pod IP 访问；用 NetworkPolicy 只允许控制面命名空间访问 3080。
- `ready.json` 写到共享 control PVC，避免解析日志。
- `activeDeadlineSeconds` 可作为 walltime 的兜底（可选）。
- 每个实例独立 Pod，资源限制天然生效；命名空间 ResourceQuota 做外层兜底。
- 不需要为每个实例建 Service/Ingress；控制面直接代理 Pod IP，减少对象数量。

**存储**

- `dw-homes`：RWX PVC，按 `subPath` 挂每个用户 home。
- 项目/参考数据：平台已有 PVC 或 CSI PVC。
- bucket：CSI PVC 或预挂载 hostPath（仅受信任节点）。

### 6.4 `sge`（首发生产后端）

**控制面**

- 跑在登录节点，能以当前 OS 用户执行 `qsub/qstat/qdel/qalter`。
- 登录节点到计算节点 SSH 可用，直接用 `ssh -L` 建立数据通道。
- 与计算节点共享文件系统：submit dir、rendezvous dir、home、workspace、dist、SIF。
- **所有站点参数都来自配置文件**：队列、PE、`h_vmem` 默认值、apptainer 路径、SSH 用户/key、共享目录、bucket 端点/region。不在代码里硬编码。

**实例**

一个实例 = 一个 SGE job：

```bash
#!/bin/bash
#$ -S /bin/bash
#$ -N dw-<id>
#$ -pe smp 4
#$ -l h_vmem=16G
#$ -l h_rt=08:00:00
#$ -q batch.q
#$ -o /data/dw/logs/<id>.log
#$ -e /data/dw/logs/<id>.log

exec apptainer exec --containall --cleanenv \
  --home /home/dsh \
  --pwd /workspace \
  --bind /data/dw/dist:/opt/dw/dist:ro \
  --bind /data/dw/homes/<user>:/home/dsh:rw \
  --bind /data/projects/P123/alice:/workspace:rw \
  --bind /data/ref:/data/ref:ro \
  /data/dw/images/dw-runtime.sif \
  /usr/local/bin/dw instance --spec /data/dw/control/<id>/spec.json
```

如果 bucket 用 FUSE，job 脚本在 `apptainer` 之前 mount、退出时 unmount：

```bash
MOUNT=/tmp/dw-mnt/<id>/raw
mkdir -p "$MOUNT"
# rclone 参数/凭据来自 config.yaml + 0600 配置文件
trap 'fusermount -u "$MOUNT" 2>/dev/null || true' EXIT
rclone mount "tos:bio-raw/P123/fastq" "$MOUNT" --read-only --vfs-cache-mode off &
# 等 mount ready；失败则 exit 1
# ...
apptainer exec ... --bind "$MOUNT:/data/raw:ro" ...
```

流程：

1. 控制面写 `spec.json` 到共享目录，`qsub` 提交。
2. `dw instance` 在计算节点起 dsh，监听 `127.0.0.1:<随机端口>`。
3. 计算节点写 `ready.json`（endpoint + launch token）到 rendezvous 目录。
4. 控制面轮询 `ready.json`，用 `ssh -L 127.0.0.1:<local-port>:127.0.0.1:<remote-port> <compute-node>` 建隧道。
5. 网关代理到本地隧道端口。
6. 停止：`qdel` + 关闭隧道；控制面重启后从 rendezvous 重新 adopt。

SGE 的注意事项（srcos 已验证过的坑）：

- 计算节点通常不能反连登录节点，共享文件系统是控制通道，`ssh -L` 是数据通道。
- `h_rt` 是生命周期上限；长会话需要 `qalter` 续期，或把 walltime 设长并在到期前提醒。
- 容器运行时在计算节点，不要在登录节点 probe。
- `h_vmem` 必须给足，队列默认值可能把 Node/dsh 压死。
- idle 回收默认关闭或设很长；SGE 上杀掉重排队对用户不友好。
- susepend（`s` 状态）不等于失败，不要误判重提。
- home 是整个 `<user>` 目录，不是 `<user>/<dist>`；dsh 用 `--profile <distribution>` 选具体 profile。
- 一个用户一个实例：同一 home 上的 `flock` 必须被 `dw instance` 持有；`qsub` 前控制面也要做一次状态检查，避免双提交。
- bucket 预挂载路径要在计算节点可见；FUSE 优先在 job 宿主侧 mount 后 bind 进容器（容器内 mount 需要 `/dev/fuse`，需实测）。

### 6.5 后端对比

| | local | k8s | sge |
|---|---|---|---|
| 控制面位置 | 同机 | 集群内 | 登录节点 |
| 实例形态 | systemd unit / 进程 | Pod | SGE job + apptainer |
| 资源限制 | cgroup / prlimit | requests/limits + quota | `-pe` / `-l h_vmem` / `-l h_rt` |
| 挂载 | 宿主路径/软链 | PVC + subPath + CSI | apptainer `--bind` |
| 访问 | 127.0.0.1 | Pod IP | `ssh -L` 隧道 |
| 复杂度 | 低 | 中 | 中高 |
| 当前定位 | 开发/单机 | 待能力确认后启用 | **首发生产** |

---

## 7. 访问入口（网关）

### 7.1 路由模式（默认子域名）

入口确认是**子域名**，所以默认 host 模式；路径前缀保留为开发/兜底：

```yaml
routing:
  mode: host                         # host | path
  hostTemplate: "{user}.dsh.example.com"
  publicBaseURL: "https://{user}.dsh.example.com"
  # path 模式才用：
  publicBasePath: /dsh
  listenBasePath: /dsh
  hostSuffix: dsh.example.com
```

| 模式 | URL | 平台需要什么 | dsh 侧 |
|---|---|---|---|
| `host`（默认） | `https://alice.dsh.example.com/` | 泛域名 DNS + 泛域名 TLS，把 `*.dsh.example.com` 路由到 `dw` | 网关按 Host 找用户/实例，保留 Host，dsh 用 `--trusted-host alice.dsh.example.com` |
| `path`（兜底） | `https://tools.example.com/dsh/i/<id>/` | 把 `/dsh/*` 路由到 `dw`，保留路径和 WebSocket | 网关剥掉前缀，Host/Origin 改写到上游 |

host 模式的约定：

- **一个用户一个子域名**：`alice.dsh.example.com`；实例 ID 只用于内部状态。
- 用户 ID 先做 slug（小写、字母数字/连字符）；冲突或非法字符加短 hash，映射存在用户记录里。
- 子域名就是根路径，不需要 `/i/<id>/` 前缀；dsh 的 `<base href="./">` 和 document-relative 路由天然工作。
- 平台只需要：泛解析 `*.dsh.example.com` → `dw`，保留 `Host` 和 WebSocket，转发 `Upgrade`/`Connection`。
- 尾斜杠、cookie、Host/Origin 都由 `dw` 自己处理；`dw doctor ingress` 检查。

**平台反代最小契约：**

1. `*.dsh.example.com` → `dw`（或 `/dsh/*` → `dw`，如果走 path 兜底）。
2. 保留 `Host`；host 模式不需要路径改写。
3. 支持 WebSocket upgrade，不压缩/不缓冲 101 响应。
4. 不重写 HTML/JS 内容；dsh 自己会注入 `<base href="./">`。
5. 允许 `Set-Cookie` / `Cookie` 通过（`dw` 自己管理 session；dsh cookie 不落到浏览器）。
6. TLS 在平台或 `dw` 终止；生产必须 HTTPS。

如果平台没有泛域名 DNS/TLS，再退回 path 模式。

### 7.1.1 平台按钮 → 实例化（用户鉴权由平台负责）

不需要平台理解“身份头”。推荐用**平台后端 server-to-server 调用 + 一次性 ticket**：

```text
[平台按钮]
   │
   ▼
平台后端  POST https://dw.internal/api/platform/launch
          Authorization: Bearer <platform-token>   （或 HMAC 签名）
          {
            "user_id": "u12345",
            "display_name": "张三",
            "tenant": "customer-a",
            "groups": ["bio", "project-P123"],
            "distribution": "bio-v1",          // 可选，默认平台配置
            "workspace": "path:/data/projects/P123/u12345",
            "mounts": [...],                     // 可选
            "resources": {...}                   // 可选
          }
   │
   ▼
dw：校验平台 token/HMAC → 找到/创建 user home → 创建或复用实例
   │
   ▼
返回 { "url": "https://u12345.dsh.example.com/_dw/launch?ticket=..." }
   │
   ▼
平台 302 浏览器到该 URL
   │
   ▼
dw：校验一次性 ticket → 设置 dw 自己的 session cookie → 302 到 /
   │
   ▼
dw 用 dsh token broker 代理到实例
```

关键点：

- **平台始终控制登录**；`dw` 不存用户密码，不做登录页。
- `user_id` 只来自平台 server-to-server 调用（或签名 assertion），**不接受浏览器传入的 user_id**。
- ticket 是一次性、短 TTL（60s）、绑定 user + instance + nonce；只存哈希，可重放检测。
- 启动是幂等的：实例已在运行 → 直接返回 URL；正在启动 → 返回进度页；已停止 → 启动后返回。
- 如果平台后端不方便调用 API，也可以由平台生成签名 URL：`https://<user>.dsh.example.com/_dw/launch?assertion=<HMAC/JWT>`；本质上仍是平台签发身份。
- 平台按钮可以带 `distribution`/`workspace`/`mounts`/`resources`；不传则用 distribution 默认值。

### 7.2 为什么路径前缀也可行（备用模式）

dsh 的 Web 前端有两个关键事实（已在源码确认）：

1. `frontend-static` 给 `index.html` 注入 `<base href="./">`；构建产物里的资源是相对路径（`./assets/...`）。
2. 客户端路由和 WebSocket 是 **document-relative**：`new URL(path.slice(1), document.baseURI)`。

因此浏览器打开 `/dsh/i/<id>/` 时，`document.baseURI` 就是该前缀，前端请求会落到 `/dsh/i/<id>/plugins/...`、`/dsh/i/<id>/api/...`。网关只要剥掉自己的前缀，把剩下的 `/`、`/api/...`、`/plugins/...` 转给 dsh 即可。

> 这是路径兜底模式的依据；host 模式不需要前缀，但仍需在 MVP 里用真实浏览器做一次端到端验证（见 §14）。

### 7.3 dsh launch token broker（必须做）

dsh 每次启动会打印一个带 `?token=...` 的 URL；这个 token 是**进程级凭据**，不能交给浏览器，也不能进日志。网关必须做代理式交换：

```text
1. dw instance 启动 dsh，解析/读取 ready.json 里的 token
2. 控制面发起 GET /?token=<token>
   - host 模式：Host = <user>.dsh.example.com
   - path 模式：Host = <upstream authority>
3. dsh 返回 303 + Set-Cookie（dsh-auth-*）
4. 控制面把 cookie 存在实例记录/内存（0600），后续所有上游请求注入这个 Cookie
5. 浏览器只拿到 dw 自己的 session cookie；dsh cookie 永不下发
6. 上游返回 401 时，控制面用 token 重新交换一次
```

这样：

- 浏览器 URL 里永远没有 dsh token；
- 不同用户/实例的 dsh cookie 不会互相覆盖；
- dsh 进程重启、token 变化时，控制面自动重新交换；
- 网关自己的鉴权与 dsh 的 `dsh-auth-*` 解耦。

### 7.4 Host / Origin / Cookie

**host 模式（默认）：**

- 保留浏览器发来的 `Host: alice.dsh.example.com` 和 `Origin: https://alice.dsh.example.com`。
- 启动 dsh 时传 `--trusted-host alice.dsh.example.com`，让 Host/Origin 栅栏通过。
- dsh 的 cookie audience 是 `alice.dsh.example.com`；一个用户一个子域名，不会和其他用户串。
- 网关仍然做 token broker：dsh 的 `Set-Cookie` 不下发浏览器，上游请求由网关注入 dsh cookie。

**path 模式（兜底）：**

- 网关把上游 `Host` 改成 `<pod-ip>:3080` 或 `127.0.0.1:<port>`，并把同源 `Origin` 改写成上游 authority。
- dsh cookie audience 是上游 authority，按实例隔离。
- 不需要 `--trusted-host`。

两种模式都：

- 上游返回的 `Set-Cookie` 一律不转发给浏览器（网关自己管理）；浏览器发来的 `dsh-auth-*` 也剥掉；
- 浏览器只持有 `dw` 自己的 session cookie（HttpOnly、Secure、SameSite=Lax）；
- 非 owner 访问返回 404，避免枚举。

### 7.5 WebSocket / SSE

- 透传 `Upgrade` / `Connection`，101 响应不做 body 包装。
- 不要对升级响应做带宽限速/压缩。
- 前端事件流走 `/api/remote.mux`（document-relative），网关按普通 WebSocket 处理。

### 7.6 平台鉴权接入

平台控制登录，`dw` 只接受平台签发的身份，不自己做登录页。支持两种接入方式：

**方式 A（推荐）：server-to-server launch API**

- 平台后端调用 `POST /api/platform/launch`。
- 认证：`Authorization: Bearer <platform-token>`，或 `X-DW-Signature: HMAC(secret, timestamp + nonce + body)`。
- 平台传 `user_id` / `display_name` / `tenant` / `groups`；`dw` 校验签名、时间窗、nonce，防重放。
- `dw` 返回一次性 ticket URL；平台 302 浏览器过去。
- 这是最干净的“平台控制鉴权、dw 只负责实例化”。

**方式 B（备选）：签名 assertion URL**

- 平台生成 `https://<user>.dsh.example.com/_dw/launch?assertion=<HMAC/JWT>`。
- `dw` 用共享密钥/公钥校验 assertion，再发自己的 session cookie。
- 适合平台后端不方便调用 API、但能生成签名 URL 的情况。
- 最小 assertion 可以是：`base64url(payload) + "." + HMAC-SHA256(secret, base64url(payload))`，payload 含 `user_id`、`exp`、`nonce`、可选 `distribution`。

**可选：可信反代身份头**

- 如果平台已经有反代注入用户信息，也支持 `auth.mode: header`：读 `X-Auth-User` + HMAC 签名。
- 但这不是前提；平台不需要为了接入去理解 header。

安全要求：

- 平台 token/HMAC secret 只放配置文件/Secret，支持轮换。
- 一次性 ticket 只存哈希，TTL 60s，单次使用。
- 浏览器永远不能自报 `user_id`；身份只来自平台签名。
- `dw` 自己的 session cookie：`HttpOnly; Secure; SameSite=Lax; Path=/`，host-only，TTL 可配（默认 12h）。
- dsh 的 cookie 不下发浏览器；只由网关在上游注入。
- 审计记录 `platform_launch`、user、distribution、mounts、资源。

---

## 8. 生命周期与状态

```text
pending → starting → running → idle → stopping → stopped
                    ↘ failed
```

- **创建**：校验 distribution、挂载、配额；写实例记录；调 backend。
- **就绪**：读 `ready.json`；token broker；健康检查；写 `running`。
- **活跃**：网关每次请求更新 `last_active_at`。
- **空闲回收**：每 30s 扫描；超过 `idleTTL` 停止。
- **walltime**：k8s 用 `activeDeadlineSeconds`；SGE 用 `h_rt` + 续期；local 用 timer。
- **停止**：幂等；先撤路由，再停后端，再更新记录。
- **控制面重启**：reconcile 所有记录：
  - 记录说 running、后端还在 → adopt（重建路由，token broker 重新交换）；
  - 记录说 running、后端已不在 → 结算为 stopped/failed；
  - 记录说 stopping、后端还在 → 继续停。
- **一个用户一个活跃实例**：`maxInstancesPerUser=1`；重复创建返回现有实例；`force=true` 才重启。
- **文件锁**：`dw instance` 启动前对 `$DSH_HOME/.dw.lock` 加 `flock`；控制面在 `qsub`/创建 Pod 前也先占位，避免双提交。
- **审计**：create/stop/reap/failed/adopt，记录 distribution digest、挂载、资源、操作者。

### 8.1 用户数据与配置持久化（重点）

用户数据分两类：

1. **`DSH_HOME`**：dsh 自己的会话、配置、API key、技能、个人指令。
2. **外部挂载**：项目工作区、参考数据、bucket。`dw` 不复制、不删除这些数据。

项目目录/默认 workspace 不在 `DSH_HOME` 里；实例停止/删除都不会删；下次启动按同一 source 挂回。

`DSH_HOME` 里每个文件的去留：

| 路径 | 内容 | 停止/重启 | 换 distribution | distribution 升级 |
|---|---|---|---|---|
| `.credentials.yaml` | 用户自己的 DeepSeek API key、dsh cookie secret | 保留 | 保留 | **绝不覆盖** |
| `sessions/` | dsh 会话日志 | 保留 | 保留 | 保留 |
| `skills/` | 用户自己的技能 | 保留 | 保留 | 保留 |
| `AGENTS.md` | 用户指令 | 保留 | 保留 | 仅当不存在时 seed，不覆盖 |
| `cordis.patch.yml`（home 级） | 跨 profile 的偏好 | 保留 | 保留 | 保留 |
| `profiles/<dist>/cordis.patch.yml` | 该 distribution 的模型/工具设置 | 保留 | 各 profile 独立 | **显式升级时备份；MVP 不自动合并** |
| `profiles/<dist>/package.json` / lock | distribution 元数据 | 保留 | 各 profile 独立 | 升级时从新 dist 复制 |
| `profiles/<dist>/node_modules` | 插件依赖 | 保留 | 各 profile 独立 | symlink 指向新版本 |
| `.dw-state.json` | 当前活跃 distribution / pinned 版本 | 控制面可重建 | 更新 | 更新 |
| `.dw.lock` | 单实例文件锁 | 进程退出释放 | 不变 | 不变 |

平台 `user_id` 是持久化主键：`users/<user>.yaml` 记录 `user_id → home/subdomain slug`。用户改显示名不会换 home，也不会丢会话；subdomain 变化需要显式迁移。

**停止 → 再启动的流程：**

1. 控制面读取实例记录，确认 owner 和 home。
2. 对 `.dw.lock` 加锁；已有锁则返回“已有实例在运行”。
3. 确认 `profiles/<dist>` 存在，`node_modules` symlink 指向记录的版本；缺了就从 dist 重新 materialize（不覆盖用户 patch）。
4. 启动 `dsh --profile <dist>`；token broker；健康检查。
5. 用户看到之前的 sessions；模型 key 仍在 `.credentials.yaml`，不需要重填。

**换 distribution：**

- 同一个 `<user>` home，换到另一个 `profiles/<dist>`；sessions、API key、用户技能保留。
- 必须先停掉当前实例，再启动新实例（一个用户一个实例）。
- 不同 distribution 的 profile patch 各自独立，互不覆盖。

**distribution 升级：**

- 默认**不自动升级**已有用户：每个 profile 记录 pinned 版本，旧实例继续用旧版本。
- 管理员可以显式升级某个用户/全部用户；流程：停实例 → 备份 `profiles/<dist>` 到 `backups/<timestamp>/` → 从新 dist 创建 profile → 保留 `.credentials.yaml` / `sessions/` / `skills/` / home patch。
- **MVP 不自动合并** `profiles/<dist>/cordis.patch.yml`：如果用户在里面改过模型/工具设置，升级会回到新 dist 的默认值；API key 和会话不丢，但自定义 base URL/模型选择可能需要重设。后续可加“按 row id 迁移”的工具，但不在第一版承诺。
- 升级前管理员必须能看到 diff/备份；升级失败可回滚到备份目录。

**备份/恢复：**

- `dw export <user>`：打包 `DSH_HOME`（排除 `node_modules` symlink，记录其 target）和实例元数据；外部工作区不打包。
- `dw import <user>`：恢复 home，重新 materialize profile/symlink。
- 生产上 `DSH_HOME` 放共享存储/PVC；备份策略由平台统一做。

**模型 key 的特殊说明：**

- 默认不注入公司共享 key；用户第一次打开 dsh 时在 Settings → Models 填自己的 DeepSeek API key。
- key 存在 `$DSH_HOME/.credentials.yaml`，权限 0600；`dw` 不读明文、不写日志、不放进实例记录。
- 如果平台提供共享 key，使用 Secret 注入环境变量或 credentials 文件，并明确“用户可覆盖”。
- 生产必须 HTTPS；纯 HTTP 会把 dsh session cookie 和用户输入的 key 暴露在链路上。

---

## 9. API / CLI / UI

### 9.1 REST

```text
# 平台入口（server-to-server，平台后端调用）
POST   /api/platform/launch           # {user_id, display_name, tenant, groups, distribution?, workspace?, mounts?, resources?}
                                      # 返回 {url: "https://<user>.dsh.example.com/_dw/launch?ticket=..."}
GET    /_dw/launch?ticket=...         # 浏览器一次性 ticket → 设置 dw session cookie → 302 /

# 用户/管理面
GET    /api/me
GET    /api/distributions
GET    /api/distributions/:name
GET    /api/storages                  # 只返回当前用户可用的 root
POST   /api/instances                 # 管理员/CLI 用；平台入口走 /api/platform/launch
                                      # 已有实例：默认返回 409（或返回现有实例）；force=true 先停再起
GET    /api/instances                 # 自己的
GET    /api/instances/:id
DELETE /api/instances/:id             # 停止
GET    /api/instances/:id/logs?follow=1

# admin
GET    /api/admin/instances
POST   /api/admin/instances/:id/stop
GET    /api/admin/distributions
POST   /api/admin/distributions/reload
GET    /api/admin/audit
```

### 9.2 CLI

```bash
dw serve -c /etc/deepwarper/config.yaml
dw dist build -f distribution.yaml -o /opt/dw/dist/bio-v1/1.0.0
dw dist verify /opt/dw/dist/bio-v1/1.0.0

dw instance create --dist bio-v1 --workspace path:/data/projects/P123/alice \
  --mount path:/data/ref/hg38:/data/ref:ro \
  --mount bucket:bio-raw/P123/fastq:/data/raw:ro \
  --cpu 4 --memory 16Gi --walltime 8h --idle-ttl 30m
dw ls
dw open <user>        # 打印/打开平台 URL
dw logs <id> -f
dw stop <id>
dw doctor             # 检查 ingress / platform / sge / k8s / 存储 / 镜像 / 权限
```

### 9.3 Web UI

保持简单，服务端渲染即可（不引入 React）：

- **实例页**：列表（状态、distribution、资源、最后活跃、打开/停止）。
- **新建页**：选 distribution、工作区、参考数据、bucket、资源模板。
- **管理页**：distribution 版本、默认资源、实例总览、审计。
- **日志页**：实例启动/运行日志尾部。

---

## 10. 配置与数据布局

```text
/etc/deepwarper/
  config.yaml
  storages.yaml
  distributions.yaml
  secrets/
    platform_token
    platform_hmac
    rclone.conf

/var/lib/deepwarper/
  users/<user>.yaml             # 平台 user_id、subdomain slug、tenant/groups
  instances/<id>.yaml
  instances/<id>/spec.json
  instances/<id>/runtime.patch.yml
  control/<id>/ready.json        # 后端共享目录，0600
  tickets/<hash>.yaml            # 一次性 launch ticket（短 TTL，仅存哈希）
  homes/<user>/                  # DSH_HOME（生产在共享存储/PVC）；内含 profiles/<dist>
  audit/audit-YYYY-MM-DD.jsonl
  logs/<id>.log
```

`config.yaml` 示例：

```yaml
server:
  listen: 0.0.0.0:7658
  dataDir: /var/lib/deepwarper

routing:
  mode: host                     # host（默认）| path（兜底）
  hostTemplate: "{user}.dsh.example.com"
  publicBaseURL: "https://{user}.dsh.example.com"
  # path 模式才用：
  publicBasePath: /dsh
  listenBasePath: /dsh
  hostSuffix: dsh.example.com

platform:
  # 平台后端 server-to-server 调 /api/platform/launch
  tokenFile: /etc/deepwarper/secrets/platform_token
  hmacSecretFile: /etc/deepwarper/secrets/platform_hmac
  ticketTTL: 60s

auth:
  mode: platform                 # platform | header | none
  # header 模式才用；平台不需要为了接入准备它
  userHeader: X-Auth-User

model:
  defaultProvider: deepseek
  defaultModel: deepseek-flash
  allowUserOverride: true
  # 仅用于提示/网络策略；用户仍可在 dsh 里填自己的 key
  allowedHosts:
    - api.deepseek.com

runtime:
  default: sge                   # 首发生产；local 用于开发
  local:
    dshBin: /opt/dw/bin/dsh
  k8s:                            # 能力确认后再启用
    namespace: dsh
    image: registry.example.com/dw-runtime:latest
    serviceAccount: dw-instance
    controlPVC: dw-control
    homePVC: dw-homes
    volumes:
      projects: { pvc: projects }
      ref:      { pvc: ref }
  sge:
    qsub: qsub
    qstat: qstat
    qdel: qdel
    qalter: qalter
    queue: batch.q
    pe: smp
    submitDir: /data/dw/sge/submit
    rendezvousDir: /data/dw/sge/rendezvous
    ssh: ssh
    sshArgs: ["-i", "/etc/deepwarper/secrets/sge_id_ed25519", "-o", "BatchMode=yes"]
    tunnel: true
    apptainerImage: /data/dw/images/dw-runtime.sif
    renewBefore: 20m
    renewFor: 2h
    bucket:
      defaultProvider: fuse       # fuse | premount | csi
      fuse:
        rcloneBin: rclone
        configFile: /etc/deepwarper/secrets/rclone.conf   # 0600
        requireDevFuse: true
        defaultReadOnly: true
      premount:
        root: /mnt                # /mnt/bio-raw -> s3://bio-raw
      backends:
        s3:
          endpoint: ""            # 例：https://s3.example.com
          region: ""
        tos:
          endpoint: ""            # 例：tos-s3-cn-beijing.volces.com
          region: ""

quotas:
  maxInstancesPerUser: 1
  maxCPUPerUser: 16
  maxMemoryPerUser: 64Gi
  maxWalltime: 24h

distributions:
  bio-v1:
    path: /opt/dw/dist/bio-v1/1.0.0
    defaults:
      resources: { cpu: 2, memory: 4Gi, walltime: 8h }
      idleTTL: 30m
```

### 10.1 站点配置清单（全部走配置文件）

| 配置项 | 例子 | 说明 |
|---|---|---|
| `sge.qsub/qstat/qdel/qalter` | `qsub` | 二进制路径或 PATH 名字 |
| `sge.queue` / `sge.pe` | `batch.q` / `smp` | 队列和并行环境 |
| `sge.submitDir` / `rendezvousDir` | `/data/dw/sge/...` | 登录节点与计算节点共享 |
| `sge.ssh` / SSH key | `ssh` + `~/.ssh/id_ed25519` | 登录节点 → 计算节点 |
| `sge.apptainerImage` | `/data/dw/images/dw-runtime.sif` | 计算节点可读 |
| `sge.renewBefore` / `renewFor` | `20m` / `2h` | `h_rt` 续期 |
| `bucket.fuse.rcloneBin` | `rclone` | 计算节点可用 |
| `bucket.fuse.configFile` | `/etc/deepwarper/secrets/rclone.conf` | 0600；TOS/S3 凭据 |
| `bucket.backends.tos.endpoint/region` | `tos-s3-cn-beijing.volces.com` / `cn-beijing` | TOS 接入点 |
| `bucket.backends.s3.endpoint/region` | `https://s3.example.com` / `us-east-1` | S3 接入点 |
| `routing.hostTemplate` | `{user}.dsh.example.com` | 子域名模板 |
| `platform.tokenFile` / `hmacSecretFile` | `/etc/deepwarper/secrets/...` | 平台签名 |
| `quotas.*` | `maxCPUPerUser` 等 | 资源上限 |
| `distributions.<name>.path` | `/opt/dw/dist/...` | distribution 版本 |

> 换站点 = 换 `config.yaml`，不改代码。所有路径、队列、endpoint、凭据位置都由配置提供。

---

## 11. 安全与配额

### 11.1 隔离

- 每个实例独立 Pod / SGE job / 进程；不共享 dsh 进程。
- MVP 所有实例以同一个 OS 用户运行；隔离靠容器/挂载命名空间，不靠 Unix UID（见 §5.6）。
- k8s：非 root、只读 rootfs、drop capabilities、RuntimeDefault seccomp、禁止 privilege escalation。
- SGE：apptainer `--containall --cleanenv`，只 bind 必要路径。
- 挂载：只 bind 精确路径，不 bind 父目录；默认 ro。
- 用户只能访问自己的实例；管理员按角色。

### 11.2 凭据

- dsh launch token：只存在控制面/ready 文件（0600），不进浏览器、不进日志。
- 平台 token / HMAC secret：只放配置文件/Secret（0600），支持轮换；一次性 launch ticket 只存哈希。
- 模型 API key：**用户自己的 DeepSeek key**，存在 `$DSH_HOME/.credentials.yaml`（0600）；不默认注入公司共享 key。
- 如果公司提供共享 key：用 Secret 注入环境变量或 credentials 文件，并允许用户在 Settings 覆盖；共享 key 不写进 distribution 镜像。
- bucket 凭据（TOS/S3）：不放在实例记录/日志里；用 0600 rclone config、节点实例角色或 CSI Secret。
- API token：只存哈希；platform HMAC 文件 0600。

### 11.3 网络

- 实例需要访问 `api.deepseek.com`（或用户自己配置的 DeepSeek 兼容 base URL）。
- 如果 bucket 走 FUSE，实例/计算节点还需要访问对应 TOS/S3 endpoint。
- 默认只放行 DeepSeek + bucket endpoint；如果允许用户改 base URL，需要明确这会把 key 发到用户指定的地址，生产应限制或审计。
- k8s NetworkPolicy：实例只允许必要的出站（模型 API、内部 MCP、DNS）；默认拒绝其他。
- 控制面到实例：只允许 3080（k8s）或隧道端口（SGE）。
- SGE：计算节点出站按集群策略；`ssh -L` 只从控制面到计算节点。
- 禁止把 dsh 端口直接暴露到公网/集群外。

### 11.4 配额

- 每用户最大实例数、CPU、内存、walltime。
- 每 distribution/客户可设上限。
- k8s 外层用 ResourceQuota/LimitRange 兜底。
- SGE 由队列/`h_vmem`/`h_rt` 兜底。
- 创建前 admission；运行中周期性对账。

### 11.5 审计

- 记录：谁、何时、哪个 distribution（含 digest）、哪些挂载、多少资源、创建/停止/回收/失败；平台入口另记 `platform_launch`、ticket 使用、user_id。
- 不记录 token、API key、bucket secret。
- JSONL 按天轮转，只追加；可选外发到平台审计。

### 11.6 数据合规

- dsh 会把对话和必要的文件内容发给 **DeepSeek 公网 API**。生产必须明确告知用户，并确认：
  - 哪些目录不允许 agent 读取（默认只挂必要数据）；
  - 是否关闭 dsh 的产品遥测（distribution patch 里关闭）；
  - 用户自己的 API key 由用户负责保管；平台不集中存储。
- 如果后续要接公司内部模型网关，只需改 distribution 的 provider/base URL，不影响运行时架构。
- 这些是部署策略，不是代码能自动保证的；在 distribution 和 storage root 层收紧。

---

## 12. 部署步骤

### 12.1 构建运行时镜像

多阶段 Dockerfile：

```dockerfile
# build: Node 22 + pnpm，构建 dsh 和 dist
FROM node:22-bookworm AS build
# ... pnpm install / pnpm build / dw dist build ...
# 产物：/out/dsh, /out/dist/bio-v1/1.0.0

# runtime: 只带 Node、dsh、dist、dw
FROM node:22-bookworm-slim
COPY --from=build /out/dsh /opt/dw/bin/dsh
COPY --from=build /out/dist /opt/dw/dist
COPY dw /usr/local/bin/dw
ENV DSH_HOME=/home/dsh
USER 1000
ENTRYPOINT ["/usr/local/bin/dw"]
```

SGE 场景再把镜像转成 `.sif`：

```bash
apptainer build /data/dw/images/dw-runtime.sif docker-daemon://registry.example.com/dw-runtime:bio-v1-1.0.0
```

### 12.2 k8s 部署（条件启用）

> 当前不把 k8s 作为首发路径；等 `dw doctor k8s` 确认能建 Pod、有 RWX PVC 后再启用。

```bash
kubectl apply -k deploy/k8s
```

包含：

- `Namespace`
- `ServiceAccount` + `Role`/`RoleBinding`（pods、pods/log）
- `Deployment`（`dw serve`，1 副本）
- `PVC`：`dw-control`、`dw-homes`（RWX）
- `ConfigMap`：config/storages/distributions
- `Secret`：platform token/HMAC、rclone config
- `Service` + `Ingress`（`*.dsh.example.com` → `dw`；path 兜底时用 `/dsh`）
- `NetworkPolicy`

如果平台已有网关/泛域名入口，`Ingress` 可以省略，直接把 `*.dsh.example.com` 或 `/dsh` 路由到 `dw` Service。

### 12.3 SGE 部署（首发生产）

1. 把 `dw` 二进制放到登录节点（`/usr/local/bin/dw`）。
2. 准备共享目录：`/data/dw/{dist,homes,control,sge/submit,sge/rendezvous,logs,images}`。
3. 准备 `dw-runtime.sif` 和 `config.yaml`（queue/PE/apptainer/SSH/bucket 全部写配置）。
4. 配置 `qsub/qstat/qdel/qalter`、队列、PE、SSH key（登录节点 → 计算节点）。
5. 配置 bucket：TOS/S3 endpoint + rclone/tosfs 的 0600 凭据；确认计算节点 `/dev/fuse` 可用（如果走 FUSE）。
6. `systemd` 起 `dw serve`（或 `nohup`）。
7. `dw doctor` 验证：qsub 可用、共享目录可写、SSH 可达、SIF 可读、dsh 能起、FUSE mount 能成功、bucket prefix 可读。

### 12.4 local 部署

```bash
make build
./dw dist build -f examples/distribution.yaml
./dw serve -c examples/config.local.yaml
# 浏览器打开 http://127.0.0.1:7658
```

---

## 13. 里程碑

### P0：五个技术验证（1–2 周）

1. **网关 + token broker（local，host 子域名）**：`/etc/hosts` 指向 `alice.dsh.local`，真实浏览器打开，dsh UI 正常加载、刷新、WebSocket 事件流正常；URL 里没有 dsh token。
2. **平台按钮 → 一次性 ticket**：模拟平台后端调 `/api/platform/launch`，浏览器带 ticket 打开 dsh；同一用户重复点击复用实例。
3. **distribution profile**：构建一个带自定义 patch + 本地 bundle 的 profile，`--dump-config` 通过，能启动。
4. **SGE 单实例**：qsub 起 apptainer + dsh，写 `ready.json`，登录节点 `ssh -L` 到计算节点，网关打开 dsh UI。
5. **TOS/S3 FUSE**：在计算节点用 rclone/tosfs 挂一个只读 prefix，bind 进 apptainer，dsh 能读；确认 `/dev/fuse` 和凭据方案。

P0 不通过就不进入 MVP。

### P1：MVP（2–3 周，SGE + local）

- local + SGE 后端
- platform launch API + ticket + subdomain 路由
- distribution build / verify
- 实例 create/list/stop/logs
- `/workspace` + ro 数据挂载
- 路径校验、配额、idle reap、启动 reconcile
- 一用户一实例 + `flock` + home 持久化
- 停止/重启后 sessions/API key 恢复
- 服务端渲染的最小 UI + CLI
- 审计 JSONL
- 验收：场景 A 端到端跑通

### P2：k8s 适配器（待能力确认，2 周）

- `dw doctor k8s` 能力探测
- Pod + Pod IP 代理 + ready.json
- RWX PVC / CSI / NetworkPolicy
- 只有在“能建 Pod”时才启用；否则明确报错
- 验收：同一 distribution 在 k8s 上跑通场景 A

### P3：bucket（1–2 周）

- SGE FUSE provider（rclone / tosfs）
- TOS / S3 URI + prefix 校验
- 预挂载 provider 作为兜底
- k8s CSI provider（如果可用）
- 验收：ro bucket 可读、rw 输出写到 workspace、mount 失败 fail loud

### P4：加固（持续）

- 多租户 namespace/配额
- 指标/日志/告警
- distribution 升级/回滚
- headless/SDK profile
- 性能与启动时间优化

---

## 14. 风险与待验证

| 风险 | 影响 | 应对 |
|---|---|---|
| 平台子域名/泛解析 | 没有泛域名 DNS/TLS 或 WebSocket | P0 用 `/etc/hosts` 先验证；给平台反代最小契约；path 模式兜底 |
| 平台 launch API | 平台后端不方便 server-to-server 调用 | 提供签名 assertion URL 备选；`dw doctor platform` |
| k8s 权限未知 | 不能建 Pod / 没有 RWX / 没有 CSI | 不作为 MVP 阻塞；`dw doctor k8s`；SGE 先上线 |
| dsh 仍是 pre-stable | 升级破坏 profile/插件 | 锁定版本；每次升级跑 dist verify + 浏览器 e2e |
| 子域名 Host/Origin | dsh 栅栏拒绝 | host 模式保留 Host + `--trusted-host`；P0 真浏览器验证 |
| launch token / cookie 语义变化 | 网关登录失效 | 把 token broker 隔离成小模块，升级时只改它 |
| 一次性 ticket 重放 | 越权进入实例 | TTL 60s、单次使用、只存哈希、绑定 user+instance+nonce |
| `DSH_HOME` 并发写 | SQLite/设置损坏 | 一用户一实例 + `flock`；禁止同 home 多进程 |
| profile 升级 | 用户 patch 丢失 | 默认不自动升级；显式备份；MVP 不自动合并 profile patch |
| 用户自己的 API key | key 泄露/滥用 | `$DSH_HOME/.credentials.yaml` 0600；HTTPS；不记录；只放行 DeepSeek 域名 |
| SGE 长会话 `h_rt` | 会话被调度器杀掉 | 续期 + 到期提醒；home 持久化，可重启 |
| SGE 控制面重启 | 隧道丢失 | rendezvous 重新 adopt + 重建隧道 |
| TOS/S3 FUSE | `/dev/fuse` 不可用、性能、凭据 | job 宿主侧 mount 优先；失败 fail loud；rclone/tosfs 参数配置化 |
| NFS 权限/UID | 挂载写失败 | 统一 UID/GID 或 root_squash 策略；`dw doctor` 检查 |
| 公网模型数据合规 | 数据外发 | 明确告知；storage root 最小化；关闭遥测；后续可换内部网关 |
| 启动慢 | 用户体验 | 镜像预热；SIF 缓存；ready 前显示进度页 |
| 控制面单点 | 可用性 | 单副本 + 快速重启 + reconcile；后续再考虑 leader election |

**P0 必须实测的清单**：

- [ ] `https://alice.dsh.local/`（host 模式）页面资源、`/api`、`/plugins` 全部正确。
- [ ] WebSocket `remote.mux` 连接成功，消息流不断。
- [ ] 平台后端调 `/api/platform/launch` → ticket → 浏览器进入 dsh；URL 里没有 dsh token。
- [ ] 同一用户重复点击复用实例；不同用户 subdomain/cookie 不串。
- [ ] 刷新、前进/后退、直接打开子路径都正常。
- [ ] dsh token 不出现在浏览器 URL/控制台/日志。
- [ ] SGE：qsub → `ready.json` → `ssh -L` → dsh UI 全链路。
- [ ] TOS/S3 FUSE mount → dsh 能读 ro 数据；mount 失败时实例明确失败。
- [ ] 停止/重启后同一 `DSH_HOME` 的 sessions/API key 都在。
- [ ] ro 挂载在 dsh bash 和 fs 工具里都写不进去。
- [ ] `/workspace` 写入持久化，实例删除后仍在。
- [ ] SGE 资源限制生效（h_vmem/h_rt）。
- [ ] 控制面重启后实例 adopt，隧道/路由恢复。

---

## 15. 与 srcos 的借鉴边界

| 借鉴 | 不借鉴 |
|---|---|
| 单二进制、零依赖、YAML + 目录扫描、无数据库 | srcos 的 Tool/Job/Flow/Grant/MCP 全套控制面 |
| `Backend`/`Handle` 运行原语、启动/停止/reconcile/reap | 工具注册、参数表单、流程画布 |
| SGE 的 rendezvous + `ssh -L` + `qalter` 续期 | SRCOS 的 OS 用户/虚拟用户模型 |
| 端口池、动态路由表 | 多用户共享一个网关的完整权限模型（按需简化） |
| 代理层 Host/Origin 改写、WebSocket 透传、cookie 隔离 | 把 dsh 变成 srcos 的硬依赖 |
| `Jail`/挂载最小粒度、精确 bind、不 bind 父目录 | srcos 的 bwrap/apptainer 工具沙箱细节（dsh 自带沙箱） |
| 审计 JSONL、生命周期状态机 | 复杂的流程/配额/申请审批 |

一句话：**借 srcos 的“运行原语 + 网关 + SGE 隧道 + 安全挂载”模式，不借它的工具平台业务层。**

---

## 16. 需要确认的问题

### 16.1 本轮已确认

- 控制面跑在 SGE 登录节点；登录节点到计算节点 SSH 可用；站点参数全部走配置文件。
- 入口是**子域名**（按“每个用户一个子域名”设计；如果是单个子域名 + 路径，path 兜底已保留）。
- 平台控制用户登录/鉴权；deepwarper 只是一个按钮入口，接收平台代入的用户信息后实例化 dsh。
- bucket 是 **TOS / S3**，允许 FUSE。
- 模型走 DeepSeek 公网 API，用户可以自己配置。
- 一个用户只能有一个实例。
- 用户数据/配置必须持久化并可复启用。
- 首批 distribution 暂不限定。

### 16.2 仍需确认（按优先级）

1. **子域名细节**：泛域名模板（`{user}.dsh.example.com`？）、DNS/TLS 由谁提供、反代是否支持 WebSocket、是否保留 Host。
2. **平台接入方式**：平台后端能否 server-to-server 调 `/api/platform/launch`（共享 token/HMAC）？还是只能生成签名 URL？按钮能传哪些用户信息（user_id/tenant/groups）？
3. **SGE 配置值**：共享文件系统路径、apptainer 版本、队列/PE、`h_vmem` 默认值、SSH 用户/key 位置。这些填进 `config.yaml` 即可。
4. **TOS/S3 细节**：endpoint/region、用 TOS 原生还是 S3 兼容、rclone 还是 tosfs、凭据由谁管理、读写需求、计算节点是否有 `/dev/fuse`。
5. **k8s**（如果后续要做）：能否创建/删除 Pod？是否有 RWX PVC？bucket 是否有 CSI？
6. **模型 key**：只允许用户自己的 key，还是平台也提供共享 key？是否允许用户改 base URL（会改变数据去向）？
7. **配额**：每用户/每客户的 CPU、内存、并发、walltime 默认值。
8. **升级策略**：distribution 升级是否允许自动？用户 profile patch 是否需要迁移？备份保留多久？
9. **首批 distribution**：等业务确定后再定；先做一个最小 `default` 跑通闭环。

---

## 附：最小可运行闭环（建议的第一版 demo）

```text
1. 构建 dist：web + 一个自定义 patch（persona + 内部 MCP + skill 目录）
2. local 起 dw serve（host 模式，/etc/hosts 指向 alice.dsh.local）
3. 模拟平台后端调 /api/platform/launch，拿到一次性 ticket
4. 浏览器带 ticket 打开 https://alice.dsh.local/
5. 在 dsh 里让 agent 读 ref、写 workspace、跑一个 bash
6. 停止实例，再启动，确认会话、API key、workspace 都还在
7. 把同一套 dist 换成 SGE backend，重复 3–6
8. 最后接 TOS/S3 FUSE（或预挂载），确认 ro bucket 可读
```

这个闭环跑通后，再扩 k8s 和 bucket 的更多 provider。

# 本机 POC 验证记录

> 目标：在本机验证子域名路由、平台 ticket、dsh launch-token broker、单机多用户实例与持久化。

## 1. 环境

```text
OS:      Linux x86_64
Go:      1.24.11
Node:    v22.22.0
dsh:     @deepseek-ai/dsh 0.2.1-rc.2（npm 安装，不是源码运行）
```

dsh 安装位置（POC 用）：

```text
/opt/dw/bin/dsh
```

## 2. POC 代码

```text
cmd/dw-local/main.go
```

它只做本机验证，不是生产控制面：

- 每个用户一个 dsh 进程；
- `DSH_HOME = <data>/homes/<slug>`；
- workspace = `<data>/workspaces/<slug>`；
- 子域名 `alice.localhost` / `bob.localhost`；
- 平台后端调 `POST /api/platform/launch` 拿 ticket；
- 浏览器带 ticket 进入，网关设置 `dw_session`；
- 网关用 dsh 的 `?token=` 换 `dsh-auth-*` cookie，浏览器看不到 dsh token；
- 反向代理 HTTP + WebSocket；
- 停止实例保留 home/workspace。

## 3. 启动

```bash
cd /path/to/deepwarper
go build -o dw-local ./cmd/dw-local

rm -rf /tmp/dw-poc
mkdir -p /tmp/dw-poc

./dw-local \
  -listen 127.0.0.1:7658 \
  -data /tmp/dw-poc \
  -dsh /opt/dw/bin/dsh \
  -domain localhost \
  -platform-token poc-token
```

## 4. 验证结果

### 4.1 平台 ticket → 子域名 → dsh UI

```bash
curl -sS -X POST http://127.0.0.1:7658/api/platform/launch \
  -H 'Authorization: Bearer poc-token' \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice","display_name":"Alice"}'
```

返回：

```json
{"url":"http://alice.localhost:7658/_dw/launch?ticket=..."}
```

浏览器流程：

```bash
curl -sS -L -c /tmp/alice.cookies -b /tmp/alice.cookies \
  'http://alice.localhost:7658/_dw/launch?ticket=...'
```

结果：

- 302 到 `/`；
- 只设置 `dw_session`，没有 dsh cookie；
- 页面 200，HTML 含 `<base href="./">`；
- 静态资源（`assets/*.js`、`favicon.svg`、`manifest.webmanifest`）200；
- `/plugins/...` combo 200；
- 上游 `Set-Cookie` 被网关剥掉。

### 4.2 WebSocket

用 Node `ws` 带 `dw_session` 连接：

```text
ws://127.0.0.1:7658/api/remote.mux
Host: alice.localhost:7658
Cookie: dw_session=...
```

结果：

```text
WS_OPEN
WS_CLOSE 1005
```

说明网关的 WebSocket 升级和 dsh cookie 注入正常。

### 4.3 两个用户，各自实例

启动 `bob` 后：

- `http://bob.localhost:7658/` 200；
- `alice` 的 cookie 访问 bob → 401；
- `bob` 的 cookie 访问 alice → 401；
- 两个 dsh 进程端口不同，`DSH_HOME` 不同。

### 4.4 持久化

停止 alice：

```bash
curl -X POST http://127.0.0.1:7658/api/local/stop \
  -H 'Authorization: Bearer poc-token' \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice"}'
```

结果：

- dsh 进程退出；
- `/tmp/dw-poc/homes/alice/profiles/web/` 仍在；
- `/tmp/dw-poc/workspaces/alice/` 仍在；
- 用同一 `DSH_HOME` 跑一次 headless（故意用无效 key）生成了一个 session 文件；
- 重新启动 web 实例后，session 文件仍在，页面 200。

session 文件示例：

```text
/tmp/dw-poc/homes/alice/sessions/--path-to-deepwarper--/
  session-74161dbd-.../session.v4.jsonl.zstd
```

> 这说明 dsh 的会话持久化依赖 `DSH_HOME`；DeepWarper 只要保证同一用户重启时复用同一个 home，就能复启用。

## 5. 本机验证暴露的问题

1. **dsh 设置 `--public-url` 后，打印的是公网 URL，不是监听端口。**
   - 不能从 `dsh web:` 行解析监听端口。
   - POC 改为网关自己分配空闲端口，再把 `--port` 传给 dsh；token 仍从打印行解析。
   - 生产实现也应该用端口池，不要依赖日志解析端口。

2. **dsh token 必须由网关交换。**
   - 直接让浏览器打开 dsh 的 `?token=` 会把进程凭据暴露给浏览器。
   - POC 已改为网关持有 token，交换 `dsh-auth-*` 后只注入上游。

3. **单机模式不是强隔离。**
   - POC 所有 dsh 进程是同一个 OS 用户。
   - 生产上用 systemd cgroup 和每个实例自己的目录视图。进程仍是同一个 OS 用户，这不是互不信任租户的边界。

## 6. POC 的边界

未验证/未实现：

- 自定义 distribution profile（POC 用 dsh 自带 `web` profile）；
- TOS/S3 目录挂载；
- systemd/prlimit 资源限制；
- 真实平台 SSO / 平台后端调用；
- 浏览器 Playwright 全链路（当前用 curl + Node WebSocket）。

这些是下一步 P0/MVP 的内容。

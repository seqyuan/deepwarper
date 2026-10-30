# Changelog

## v0.3.0 — 2026-10-30

- 控制面状态加固：Store revision/CAS、per-record 锁、dataDir 单实例锁、incarnation 归属端口、停止重试与 Pending/Stopping 生命周期。
- 输入/路径安全：user/dist/workspace 校验、安全 InstanceID、YAML patch、mount realpath/root containment、dsh realpath/PATH、workspaceRoots。
- 网关安全：ticket 原子消费与 janitor、Host allowlist、HTTPS Origin、登录限速、用户删除 tombstone/session 代际、dsh cookie 刷新、publicPort。
- 沙箱/运行：bwrap realpath bind、`/dev/shm`、`/etc/passwd` 等、stale rclone FUSE 自动清理、systemd unit stop 不再残留 failed。
- 真实 E2E：dsh 0.2.1-rc.2 + rclone 1.75.2 + systemd 255 + bwrap；HTTP/WS、只读 S3 FUSE、secrets 不泄漏、stop/unmount 与 stale FUSE 恢复。
- 行为变化：`dw del` 改为 tombstone；workspace 仅允许受限根；path mount 不能覆盖 `/workspace`；新增 `routing.publicPort` / `trustProxyHeaders` / `server.workspaceRoots` / 登录限速配置。

## v0.2.0 — 2026-10-30

- 管理员用 `dw user` 建账号，浏览器登录后进入该用户自己的 dsh
- 每个实例用 bubblewrap 拼目录视图；本地目录和 TOS/S3 bucket 前缀挂进去
- 共享 moniforge 只读，用户在 `/workspace/envs` 里用 `dw-env` 装软件
- 用户可从本机 catalog 启用额外 dsh 插件
- 去掉 SGE 任务和 k8s Pod 运行时

## v0.1.0 — 2026-10-27

- 初始版本：DeepWarper 单机 local 模式与子域名网关
- 每个用户一个 dsh 实例，独立 `DSH_HOME` / workspace / 端口
- 平台 server-to-server launch API + 一次性 ticket + HMAC `dw_session`
- 网关代持 dsh launch token；浏览器只持有 `dw_session`
- systemd-run --user cgroup（CPU/内存/Tasks），prlimit/direct fallback
- SGE backend：qsub/qstat/qdel/qalter、rendezvous、ssh -L、qalter 续期、控制面重启 adopt
- distribution materialize、持久化、启动 reconcile、idle reaper
- 本机验证：Playwright 页面、WebSocket、双用户隔离、控制面重启 adopt、systemd 真实 unit
- SGE 使用 fake Runner/Tunnel 验证；真实集群验证待站点配置

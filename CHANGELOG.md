# Changelog

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

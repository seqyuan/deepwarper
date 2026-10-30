# DeepWarper

> v0.4 增量方案见 [`plan-v0.4.md`](plan-v0.4.md)：登录即实例化、TOTP、插件版本化、挂载白名单/配额/审计、网络隔离。

每个用户一个本机 dsh 进程。管理员用 `dw user` 建账号，`dw serve` 提供登录页，登录后反代到这个用户自己的实例。

目录视图由 bubblewrap 拼出来：工作区和 home 可写，参考数据只读，bucket 前缀由宿主机上的 rclone 先挂成目录再 bind 进实例。共享的 moniforge/conda 前缀只读，用户在 `/workspace/envs` 里用 `dw-env` 安装软件。站点插件在 distribution 里，用户额外插件从本机 catalog 启用。

实例停掉之后，home、会话和工作区文件还在。没有调度器，也没有容器编排。

# deepwarper

把定制好的 DeepSeek Harness Web 做成可分发的运行时。一个 Go 二进制管账号、登录和每个用户自己的 dsh 进程。

- 运行时：本机一个用户一个 dsh 进程。systemd-run --user 做 cgroup，没有 user systemd 时降级为 prlimit 或普通进程。
- 入口：`dw user` 建账号，`dw serve` 提供登录页，登录后反代到该用户的 dsh。
- 挂载：bubblewrap 拼出每个实例的目录视图。本地目录 bind 进去，火山 TOS 和其他 S3 兼容 bucket 由宿主机 rclone 挂成目录后再 bind。
- 环境：共享 moniforge/conda 前缀只读。用户用 `dw-env` 在 `/workspace/envs` 里装软件。
- 插件：站点 distribution 是底座，用户从本机 catalog 启用额外插件。
- 配置：[`examples/config.local.yaml`](examples/config.local.yaml)、[`examples/storages.yaml`](examples/storages.yaml)、[`examples/distribution.yaml`](examples/distribution.yaml)
- 说明：[`docs/local-mode.md`](docs/local-mode.md)、[`docs/plan.md`](docs/plan.md)

## 快速开始

一个 `dw` 二进制。管理员用 CLI 建用户，`dw serve` 给出登录页，登录后反代到该用户自己的 dsh。共享的 moniforge/conda 前缀只读挂载，用户环境在 `/workspace` 下，用 `dw-env` 创建。本地目录和 bucket 前缀按用户挂载。额外 dsh 插件从本地 catalog 启用。

```bash
# 配置目录默认是可执行文件旁的 config/。dw user 和 dw passwd 会提示输入密码，终端上不回显。
# 非交互场景才设置 DW_PASSWORD。
dw user -d /etc/dw alice
dw remote add -d /etc/dw tos1 --provider tos --endpoint https://tos.example --region cn-beijing --bucket data --credential-file /etc/dw/cred
dw mount add -d /etc/dw alice --remote tos1 --prefix proj/raw --at /workspace/raw --ro
dw mount add -d /etc/dw alice --local /srv/ref:/data/ref:ro
dw catalog add -d /etc/dw office /opt/dw/catalog/office
dw plugin enable -d /etc/dw alice office

dw serve -c config.yaml
# 浏览器打开 /login，进入该用户自己的 dsh。
# 实例未运行时页面会说明需由管理员启动，不会自动拉起 dsh。
```

`config.yaml` 里加上（留空则保持原来的 ticket 入口，不挂 conda）：

```yaml
auth:
  usersDir: /etc/dw/users
runtime:
  moniforge: /opt/moniforge
```

本地路径挂载还要一份 `config/roots.yaml`，host 必须落在其中恰好一个 root 下：

```yaml
roots:
  - id: ref
    host: /srv
    allowRw: false
```

登录进实例后，环境不使用 base：

```bash
dw-env create analysis numpy pandas
dw-env run analysis -- python
```

`dw-env` 写在数据目录的 `bin/dw-env`。`dw doctor` 会打印 moniforge、bwrap、rclone 和 users 目录；缺可选工具只是警告。

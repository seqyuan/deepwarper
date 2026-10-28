# deepwarper

把定制好的 DeepSeek Harness Web 做成可分发的运行时：一次构建插件集，按需为每个用户/项目启动带工作区、数据挂载和资源上限的实例。

- 方案规划：[`docs/plan.md`](docs/plan.md)
- 配置草案：[`examples/config.sge.yaml`](examples/config.sge.yaml)、[`examples/storages.yaml`](examples/storages.yaml)、[`examples/distribution.yaml`](examples/distribution.yaml)
- 目标形态：Go 单二进制（控制面 + 网关 + 生命周期）+ 固定版本运行时镜像
- 首发生产后端：SGE（控制面在登录节点，`ssh -L` 到计算节点）；local 用于开发；k8s 待集群能力确认
- 入口：每个用户一个子域名（`https://<user>.dsh.example.com/`），平台按钮通过一次性 ticket 进入
- 挂载：宿主路径 / PVC / TOS、S3 bucket（优先 FUSE，也支持预挂载/CSI）
- 用户数据：`DSH_HOME` 按用户持久化，停止/重启保留会话、配置和用户自己的 API key
- 单机模式：每个用户一个 dsh 进程 + 独立 home/workspace；systemd-run --user cgroup（prlimit fallback）
- POC 记录：[`docs/poc-local.md`](docs/poc-local.md)
- 单机生产模式验证：[`docs/local-mode.md`](docs/local-mode.md)
- 综合审核与落地计划：[`plans/final-implementation-plan.md`](plans/final-implementation-plan.md)
- 第二轮规划：[`plans/round2/final-plan.md`](plans/round2/final-plan.md)
- SGE 模式说明：[`docs/sge-mode.md`](docs/sge-mode.md)
- 当前状态：**local（含 systemd cgroup）+ SGE 后端已落地（SGE 为 fake-runner 验证，真实集群待配置）；FUSE/k8s 未实现**

## 快速理解

```text
平台按钮 → dw serve → 每用户一个 dsh web 实例（子域名）
                    ├─ /workspace        rw
                    ├─ /data/ref         ro
                    └─ /data/raw         ro (TOS/S3 bucket)
```

一次构建：

```bash
dw dist build -f distribution.yaml
```

一次部署：

```bash
dw serve -c config.yaml
```

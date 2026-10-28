# SGE 模式（实现 + 验证边界）

> 代码：`internal/runtime/sge/`、`internal/config`、`internal/instance`  
> 配置：[`examples/config.sge.yaml`](../examples/config.sge.yaml)  
> 状态：**fake Runner/Tunnel 已通过；真实集群待站点配置**

## 1. 已实现

- `qstat -xml` 解析（不是 `-xml -j`），`qw/r/s/S/Eqw/d` 分类；
- suspended 视为 alive，不 reap、不 resubmit；
- 连续 3 次 "job not in qstat" 才判定 gone；
- 队列等待与启动等待分开：`launchWait: 0` 立即返回 `queued`，快速 tick 再 promote；
- qsub header：`-N`、`-j y`、`-o/-e`、`-cwd`、`-V`、`-pe`、`-l h_vmem,h_rt[,gpu]`、`-q`、`-P`、extra args；
- `h_vmem` 按 slot 计算并向上取整，`minHVmemPerSlot` 默认 4Gi；
- job script：在计算节点解析 `apptainer||singularity`，精确 bind（home/workspace/dist/control/extraBinds），不 `exec` 容器，不包含 token；
- `dw instance --spec` 在容器内运行；`spec.Port == 0` 时在计算节点选空闲端口；
- qsub 输出解析拒绝 "syntax error near line 3 → job 3"；
- `ssh -L`：`--` 结束选项、host 校验、`ExitOnForwardFailure=yes`、等待端口可 dial；
- 控制面重启：`Adopt` 复用/重建隧道，不因控制面掉线就 qdel 活着的 job；
- `qalter -l` 续期时重述 `h_vmem`（`qalter -l` 会替换整个资源列表）；
- SGE 默认 `idleReap: false`；
- manager 多 backend：新实例走 `runtime.default`，已有记录按 `inst.Backend` probe/stop/renew/adopt。

## 2. 验证

```text
go test ./internal/runtime/sge -v
  64 top-level tests, 76 including subtests, all pass
go test ./internal/instance -v
  queued -> promote, suspended, adopt, renewal dedup, backend routing, all pass
go test -race ./...    all pass
go vet ./...           clean
```

`dw doctor -c examples/config.sge.yaml` 在填好站点路径后输出：

```text
sge: ok (pe=smp slots/cores=cores queue=batch.q project=(none))
sge: tunnel=true directDial=false idleReap=false renewBefore=20m0s warnBefore=30m0s
```

## 3. 真实集群仍需验证

1. `qsub/qdel` 从 `dw serve` 所在主机可用（或 `qsub` 指向 ssh 到 submit host 的 wrapper）。
2. 真实 `qstat -xml` 样本（替换手写 fixture）。
3. `h_vmem` 是按 slot 还是按 job；真实 floor 用 probe job 测 `ulimit -v`。
4. 运行中 `qalter -l h_rt` 是否被站点允许。
5. `known_hosts` / `BatchMode=yes` / `ssh -L` 到计算节点。
6. `kill -9 dw serve` → 重启 → adopt 重建隧道。
7. GPU 资源拼写、NFS `flock`、dsh 长驻内存与 `minHVmemPerSlot`。
8. `DW_SGE_REAL=1` 的 gated e2e 尚未编写（需要真实 cluster）。

## 4. 当前边界

- 没有 FUSE；`containerArgs`/`extraBinds` 已能承载普通 bind，FUSE mount 需要 job-host prelude。
- 没有 k8s。
- 队列实例的前端目前是 202 进度页（`gateway` 对 `queued/starting` 返回自动刷新页面）。
- `dist` bind 采用同路径 identity bind，确保用户 home 里 `node_modules` 符号链接在容器内可解析。

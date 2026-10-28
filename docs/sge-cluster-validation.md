# SGE 真实集群验证清单

> 真实集群验证尚未执行：需要在具备 `qsub/qstat/qdel/qalter` 的 SGE
> 提交节点上运行。这份清单在拿到提交节点访问后按顺序跑。

## 0. 前置条件

- `dw serve` 所在主机能执行 `qsub/qstat/qdel/qalter`（或它们是指向提交主机的 wrapper）
- 共享文件系统同时挂载在登录节点和计算节点：
  - `server.dataDir`（至少 `dataDir/control` 必须可见）
  - `runtime.sge.submitDir`
  - `runtime.sge.rendezvousDir`（必须包含 `server.dataDir/control`）
  - `runtime.sge.apptainerImage`（计算节点可读）
  - `distributions.<name>.path`
- 登录节点 → 计算节点 `ssh -L` 可用，`BatchMode=yes`，known_hosts 已配置。
- 计算节点有 `apptainer` 或 `singularity`。

## 1. 只读探测（不提交作业）

```bash
hostname
command -v qsub qstat qdel qalter ssh
qstat -xml > /tmp/qstat-baseline.xml
head -30 /tmp/qstat-baseline.xml

# 共享目录在登录节点可见
ls -ld /data/dw/data /data/dw/sge/submit /data/dw/dist/default/1.0.0/profile
```

把 `/tmp/qstat-baseline.xml` 发回，用于替换仓库里的手写 fixture。

## 2. 最小 probe job

创建 `probe.sh`：

```bash
#!/bin/bash
#$ -S /bin/bash
#$ -cwd
#$ -j y
set -x
hostname
whoami
ulimit -v
command -v apptainer || command -v singularity || echo NO_CONTAINER_RUNTIME
readlink -f /data/dw/images/dw-runtime.sif 2>&1
test -r /data/dw/images/dw-runtime.sif && echo SIF_READABLE || echo SIF_UNREADABLE
touch /data/dw/sge/submit/.probe-ok && echo SHARED_FS_WRITABLE
```

提交（按站点替换 queue/PE）：

```bash
qsub -N dw-probe -pe smp 1 -l h_vmem=4G -l h_rt=00:05:00 -q batch.q probe.sh
qstat -j <jobid>
```

重点确认：

- `h_vmem` 是 **per slot** 还是 per job（在 `-pe smp 4` 下再测一次 `ulimit -v`）；
- 计算节点容器运行时路径；
- SIF 是否可读；
- 共享目录是否可写。

## 3. 配置校验

```bash
./dw doctor -c examples/config.sge.yaml
```

当前 `doctor` 只做配置/二进制检查，不提交作业；缺少路径时按提示填。

## 4. 真实启动

```bash
./dw serve -c examples/config.sge.yaml
```

平台入口：

```bash
curl -sS -X POST http://<login>:7658/api/platform/launch \
  -H "Authorization: Bearer <platform-token>" \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"sge-test","display_name":"SGE Test","distribution":"default"}'
```

预期：

1. 若 `launchWait: 0`，立即返回 `queued`，浏览器拿到 202 自动刷新页；
2. `qstat` 出现 `dw-sge-test-*.sh` 的 job id；
3. 作业 running 后，fast tick 执行 `promote`，`ssh -L` 建立，页面变成 200；
4. `http://sge-test.<hostSuffix>/` 页面加载，`<base href="./">`，`/api/remote.mux` WebSocket 可连；
5. `/api/local/status` 显示 `running`，`endpoint=127.0.0.1:<login-port>`。

## 5. 关键 SGE 问题验证

### 5.1 qstat 格式

```bash
qstat -xml > /tmp/qstat-running.xml
```

确认是 `<job_list>`，不是 `-xml -j` 的 `<detailed_job_info>`。  
把 fixture 加入 `internal/runtime/sge/testdata/` 后可补真实解析测试。

### 5.2 `h_vmem` 语义

提交 `-pe smp 4 -l h_vmem=4G`，在 job 里 `ulimit -v`。  
如果站点按 per-job 计算，需要调整 `resourceLimits` 或配置说明；当前实现按 per-slot。

### 5.3 `qalter` 续期

```bash
qsub -N dw-renew -pe smp 1 -l h_vmem=4G -l h_rt=00:10:00 -q batch.q sleep.sh
qalter -l h_vmem=4G,h_rt=00:30:00 <jobid>
qstat -j <jobid>
```

确认：

- 站点允许对 running job 改 `h_rt`；
- `qalter -l` 会替换资源列表，所以 `h_vmem` 必须重述（当前 `Renew` 已重述）。

### 5.4 控制面重启 adopt

1. 实例 running，浏览器 session 正常；
2. `kill -9 <dw serve pid>`；
3. 重新 `dw serve`；
4. 旧 `dw_session` 仍 200；
5. `ssh -L` 被重建，`qstat` 仍显示原 job id；
6. 不出现重复 job。

### 5.5 停止

```bash
curl -X POST .../api/local/stop -d '{"user_id":"sge-test"}'
```

确认 `qdel` 后 job 消失，隧道进程退出，端口释放。

## 6. 需要回传的材料

- `qstat -xml` 和 `qstat -xml -j <id>` 各一份；
- probe job 完整输出；
- `examples/config.sge.yaml` 的站点值（隐去密钥）；
- `dw serve` 日志、`/api/local/status`、`dataDir/control/<id>/{ready.json,token,state,exit_code}`（token 不要贴）；
- 任何 502/403/503 的浏览器 Network 面板请求与响应头。

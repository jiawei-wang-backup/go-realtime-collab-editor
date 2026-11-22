# WebTransport 轻量客户端与万人在线验收手册

## 目标和边界

本手册用于从已完成的真实 Chrome 2,000 会话结果继续扩展到 5,000/10,000 在线。5,000/10,000 必须使用轻量 Go WebTransport 客户端；现有 Chrome 脚本的 2,000 上限不能通过改数字外推为万人容量。

当前第一版客户端是 `cmd/webtransport-loadtest`，Windows 启动入口是 `scripts/run-webtransport-loadtest.ps1`。它使用真实 TLS CA、Origin、WebTransport AUTH/JOIN/Owner Redirect/EDIT/ACK 路径，不使用 `InsecureSkipVerify`，不关闭证书验证。

## 当前客户端覆盖

- REST 注册、登录、文档创建和写权限准备；
- WebTransport HTTP/3 建连；
- 流内 AUTH；
- JOIN 和一次有界 Owner Redirect；
- 4 字节大端长度 + JSON framing；
- JOIN 版本；
- EDIT 和 `edit_applied` ACK；
- 逻辑会话数、Transport 实例数、JOIN/编辑延迟；
- 意外断线与测试主动关闭区分；
- 结构化 JSON 结果。

当前已完成的第一轮真实隔离验证：

- 2 个客户端、1 个共享文档：JOIN 成功 2/2，Redirect 2 次，编辑 ACK 2/2，对端更新 2 次，最高版本 2，意外断线 0，fixture 清理通过。
- 50 个客户端、2 个文档：JOIN 成功 50/50，Redirect 25 次，编辑 ACK 50/50，对端更新 1200 次，最高版本 25，意外断线 0；JOIN P95 29.956ms，编辑 ACK P95 19.827ms，fixture 清理通过。

这两轮只证明协议冒烟和小规模基线，不代表 1,000/5,000/10,000 容量通过。

Mac 本地轻量客户端基线：

- 256 会话、20 文档：JOIN 256/256，Redirect 166，编辑 256/256，内容/版本收敛通过，意外断线 0；JOIN P95 83.784ms，编辑 ACK P95 59.5ms。准备阶段 76.14s，在线阶段 30.04s。
- 256 会话指标复跑：JOIN 256/256，Redirect 114，编辑 256/256，内容/版本收敛通过，意外断线 0；JOIN P95 142.855ms，编辑 ACK P95 118.831ms。`collab_webtransport_sessions_active` 两节点峰值分别为 142 和 114，Room 事件队列深度峰值 0，etcd lease renewal failures 为 0。
- 1,000 会话、100 文档：JOIN 1,000/1,000，Redirect 500，编辑 1,000/1,000，内容/版本收敛通过，意外断线 0；JOIN P95 151.322ms，编辑 ACK P95 137.094ms。准备阶段 360.92s，在线阶段 57.20s。
- 1,000 会话采样峰值：两节点各观察到 500 活跃 Room 客户端、50 个 Room；Room 事件队列深度峰值 0，etcd lease renewal failures 为 0；单节点 goroutine 峰值约 6,710，堆峰值约 68.1MiB，服务进程 RSS 峰值约 140.5MiB，CPU 峰值约 78.3%。
- 现有 `collab_quic_connections_active` 只覆盖原生 QUIC 客户端，不能代表 WebTransport 会话；已补充 `collab_webtransport_sessions_active`，下一轮采样使用该指标。轻量客户端报告也明确标记底层物理连接数未独立测量。

当前首个可复现瓶颈是测试准备阶段：1,000 个账号/权限串行准备耗时约 361 秒；在线 JOIN/编辑阶段本身未出现队列堆积、租约失败或意外断线。测试客户端已增加有界准备并发，256 会话在并发度 4 下准备耗时降至 27.82 秒，且 JOIN/编辑/收敛仍通过；1,000 会话并发度 4 的三轮准备耗时为 112.32s、103.46s、104.13s，均完整通过。

热点文档测试：5 个写入者、同一文档、每个写入者每秒 1 次编辑、每个写入者 20 次编辑：成功编辑 100/100，最终版本 100，最终内容长度 100，收敛通过，意外断线 0，编辑 ACK P95 26.284ms。

2,000 会话首次尝试尚未通过：在准备阶段使用并发度 4、200 文档时出现一次文档 ownership lost，随后权限请求返回 403；该轮没有进入在线容量阶段，不能作为 2,000 容量结论。降低准备并发度后复测通过：并发度 1、200 文档下 JOIN 2,000/2,000，Redirect 980，编辑 2,000/2,000，内容/版本收敛通过，意外断线 0；准备阶段 259.57s，JOIN P95 30.731ms，编辑 ACK P95 17.782ms。

2,000 会话资源峰值：node-a WebTransport 活跃会话 1,020、Room 客户端 1,020、102 个 Room；node-b WebTransport 活跃会话 980、Room 客户端 980、98 个 Room；两节点 Room 事件队列峰值 0，etcd lease renewal failures 为 0；goroutine 峰值约 13,624/13,091，Go heap 峰值约 137.1/125.3MiB，服务进程 RSS 峰值约 265.1MiB，CPU 峰值约 84.2%。

该结果支持进入下一档 5,000 前的专项评估，但不等同于 5,000/10,000 通过；2,000 推荐使用低准备并发或预置准备数据，避免把准备阶段 ownership 压力误判为在线容量瓶颈。

5,000 会话 Mac 低压力基线：

- 首轮因客户端总上下文超时配置不足，在准备耗时约 11 分钟后于 3,401 个 JOIN 时提前结束；这不是服务端容量结论。客户端已把大规模运行余量从 10 分钟增加到 30 分钟。
- 修复超时后，准备并发度 1、200 文档下通过：JOIN 5,000/5,000，Redirect 2,525，编辑 5,000/5,000，内容/版本收敛通过，意外断线 0；准备阶段 648.77s，JOIN P95 32.889ms，编辑 ACK P95 64.121ms。
- 资源峰值：node-a/node-b WebTransport 活跃会话 2,475/2,525，Room 客户端相同，Room 数 99/101；Room 事件队列峰值 0，etcd lease renewal failures 为 0；goroutine 峰值 32,533/33,189，Go heap 峰值约 345.6/359.9MiB，服务进程 RSS 峰值约 605MiB，CPU 峰值约 114.1%。

5,000 在线路径在本轮通过，但测试准备耗时和资源峰值已经明显上升；10,000 仍不能直接声称通过，下一步应先做 5,000 重复稳定性或预置准备数据，并明确 CPU/RSS 阈值后再决定是否进入 10,000。

5,000 独立稳定性复测：

- 第 2 轮：JOIN 4,997/5,000，3 个失败均为 `timeout: no recent network activity`；没有编辑阶段结果。
- 第 3 轮：使用更缓的 300 秒接入斜坡，JOIN 4,999/5,000，1 个失败同样为 `timeout: no recent network activity`；没有编辑阶段结果。
- 两轮失败期间 Room 事件队列峰值均为 0，`collab_etcd_lease_renewal_failures_total` 均为 0；但后续独立诊断轮在准备阶段观察到 node-b 的 etcd node lease lost 和多个 ownership lost，因此不能把所有 5,000 超时归为同一原因。

因此当前结论是：5,000 在无重试配置下仍有少量 Dial 超时，不能直接进入 10,000。采样脚本已支持可选 `CLIENT_PID`，并在诊断轮实际采到 Go 发压进程 PID 91863（CPU 峰值约 4.0%，RSS 峰值约 17.8MiB）；该轮服务节点 CPU 峰值约 88.6%/6.1%，RSS 约 38.1/36.3MiB。诊断轮最终观察到 node-b 的 etcd node lease lost 和多个 document ownership lost，随后 node-a REST 连接拒绝，说明至少有一轮失败是准备阶段集群 lease 丢失，不是单纯客户端 Dial 超时。

为验证 3 秒 lease 是否是主要因素，fixture 现在支持 `COLLAB_FIXTURE_ETCD_LEASE_TTL` 和 `COLLAB_FIXTURE_JWT_TTL`；仅放宽 lease 但仍使用默认 15 分钟 JWT TTL 的首轮在用户准备到 2,802 时因 owner token 过期返回 401，因此后续复测同时使用 1 小时测试 JWT。使用 `COLLAB_FIXTURE_ETCD_LEASE_TTL=10s`、`COLLAB_FIXTURE_JWT_TTL=1h` 的无重试复测没有再出现 node lease lost 或 ownership 大面积回收，Room 队列仍为 0、lease renewal failures 为 0；但准备阶段升至 1,174.99s，JOIN 为 4,996/5,000，仍有 4 个 `webtransport dial ... timeout: no recent network activity`。

客户端新增 `-connect-retries`，只对 WebTransport Dial 阶段重试并单独报告 `join_retries`。使用一次重试、10 秒 lease、1 小时 JWT 的两轮独立复测均通过：

- 第 1 轮：JOIN 5,000/5,000，`join_retries=5`，编辑 5,000/5,000，收敛通过，意外断线 0；准备 1,183.71s，JOIN P95 31.005ms，编辑 ACK P95 31.796ms。
- 第 2 轮：JOIN 5,000/5,000，`join_retries=2`，编辑 5,000/5,000，收敛通过，意外断线 0；准备 1,180.49s，JOIN P95 29.161ms，编辑 ACK P95 32.122ms。
- 两轮 Room 队列均为 0，etcd lease renewal failures 均为 0；服务 RSS 峰值约 628/549MiB，CPU 峰值约 147.8%/118.1%，goroutine 峰值约 33,841/34,497。

这证明 Mac 侧在明确记录重试的条件下已得到两轮完整 5,000 在线收敛结果；但准备阶段耗时约 19.7 分钟，且仍有原始 Dial 失败需要重试，不能把它等同于无重试的零失败稳定性。

当前尚未声称覆盖：

- Go 轻量客户端的 Owner Failover 后自动重连和 `RESUME`/`SYNC`/`RESYNC` 完整状态机；当前只具备 AUTH/JOIN/Owner Redirect/EDIT/ACK 和内容收敛验证，Failover 仍以既有 Chrome harness 结果为基线。

Mac Go RESUME smoke 已完成：2 个会话、1 个文档各先 JOIN，再执行一次计划内断线重连；`resume-count=2`，结果为 `resume_attempts=2`、`resume_success=2`、`resume_failures=0`、`resync_count=0`，随后编辑 2/2、内容/版本收敛通过、意外断线 0。该 smoke 验证了真实 WebTransport `AUTH → RESUME → SYNC → EDIT` 路径；尚未覆盖 Owner 进程失效触发的 `RESYNC_REQUIRED`。

Mac Owner Failover smoke 已完成：在 `OWNER_FAILOVER_DRILL=true` fixture 中终止 node-a，保持 node-b 继续运行；node-b 日志确认接管文档 ownership 并完成 room recovery，Go 客户端通过备用 endpoint 完成 `resume_attempts=2`、`resume_success=2`、`resume_failures=0`，随后编辑 2/2、内容/版本收敛通过、意外断线 0。该轮 `resync_count=0`，因为文档历史仍在可增量 SYNC 范围内；`RESYNC_REQUIRED` 仍需单独制造 snapshot/history floor 场景。

同一 Failover 流程已扩展到 50 会话、200 文档：终止 node-a 后 `resume_attempts=50`、`resume_success=50`、`resume_failures=0`，Owner Redirect 23 次，编辑 50/50，内容/版本收敛通过，意外断线 0；JOIN P95 10.088ms，编辑 ACK P95 7.422ms。该轮仍为 SYNC 恢复，尚未覆盖大规模 RESYNC_REQUIRED。

256 会话已完成两轮诊断：第一轮在 JOIN 阶段触发故障，`resume_success=256/256`、收敛通过，但有 17 次意外断线且 JOIN P95 被故障拉高至 5.065s；第二轮在 JOIN 全部完成后触发，`resume_success=256/256`、收敛通过，但 node-b ownership handoff 与在线编辑重叠导致 `edit_failures=107/256`、`unexpected_disconnects=131`。因此 256 目前只能作为恢复路径证据，不能作为无故障稳定性通过。客户端新增 `resume-settle` 参数，下一轮会在恢复后等待 ownership 稳定再发 EDIT。

1,000 会话试验已完成恢复路径验证：`resume_attempts=1000`、`resume_success=1000`、`resume_failures=0`、收敛通过；但只完成编辑 485/1000，`edit_failures=515`、`unexpected_disconnects=513`，即使 `resume-settle=10s` 仍不足以宣称 Failover 稳定。JOIN P95 22.945ms，编辑 ACK P95 26.707ms。下一步必须继续增加接管稳定等待并区分计划内旧连接关闭与真正恢复失败，不能直接推进 5,000 Failover 或 10,000。随后对 50 会话增加 `resume-settle=120s` 并启用 2 次 EDIT 重试，结果仍为 `resume_success=50/50` 但编辑仅 26/50；这表明问题不是单纯等待时长，而是 node-b 接管后的 owner 路由/错误响应需要继续诊断。客户端现在会消费服务端 `error` 消息并对瞬时 EDIT 错误做有限重试，避免把服务端拒绝误报成 ACK 超时。10 会话小规模复测（`resume-settle=30s`、2 次编辑重试）通过：`resume_success=10/10`、编辑 10/10、`edit_failures=0`、`unexpected_disconnects=0`。因此问题在更高并发的 owner handoff/房间恢复压力下出现，不能用 10 会话结果外推 5,000。随后另一轮 50 会话（同样 `resume-settle=30s`、无编辑重试）再次通过：`resume_success=50/50`、编辑 50/50、`edit_failures=0`、`unexpected_disconnects=0`。这说明 50 会话结果存在时序敏感性，必须用重复轮次和更大规模原始结果判断，不能把单轮通过当作稳定性证明。

5,000 Failover 复测尚未进入 WebTransport JOIN：准备并发 1 的运行持续约 27 分钟仍停留在 REST 准备阶段，已取消；准备并发 4、租约 30s 的重跑在用户 1,113 的权限授予阶段返回 500，也已取消。客户端现加入 `prepare-retries`（默认 1 次退避），下一轮需先验证准备阶段可重复完成，再触发 5,000 Owner Failover。

5,000 重试稳定后对 Go Failover 可测性的评估：

- 现有 Go 客户端没有保存 session token/doc version 后重新建立 WebTransport 会话的状态机，因此不能直接测量 Owner 失效后的自动 `RESUME`。
- 需要新增的最小观测顺序是：旧连接关闭并记录旧版本 → 新连接 `AUTH` → `RESUME(last_version)` → 接收 `SYNC` 或 `RESYNC_REQUIRED` → 应用快照/增量操作 → 恢复编辑并验证最终版本/内容。
- Owner Redirect 需要在 `RESUME` 前后都保留；旧连接必须标记为预期关闭，避免把 failover 关闭计为意外断线；报告至少应分开 `resume_success`、`resync_count`、`resume_failures` 和最终收敛。
- 因此 5,000 在线收敛已具备进入该客户端状态机开发的条件，但在 Go 客户端补齐前，不能声称 Mac 侧 Go Failover/RESUME 已验证。
- 多写入者 OT 冲突和最终内容收敛；
- 对端更新 P95；
- 独立观测底层 QUIC 连接数；
- 5,000/10,000 通过。

## 运行前提

Windows 只使用本轮专用账号、文档、CA 和 Origin。密码从当前 PowerShell 的 `WT_LOAD_PASSWORD` 环境变量读取，不放入命令行，不写日志。

```powershell
$env:WT_LOAD_PASSWORD = '<本轮专用密码>'
.\scripts\run-webtransport-loadtest.ps1 `
  -ApiUrl 'https://<Mac>:<REST_PORT>' `
  -Endpoints 'https://<Mac>:<WT_A>/api/webtransport,https://<Mac>:<WT_B>/api/webtransport' `
  -CaFile "$env:TEMP\collab-rootCA.pem" `
  -Origin 'https://<Mac>:<VITE_PORT>/e2e/webtransport-capacity.html' `
  -Clients 50 `
  -Documents 200 `
  -Online ([TimeSpan]::FromMinutes(1))
```

上面的命令块使用仓库相对路径，实际执行应使用仓库路径前的反斜杠形式：

```powershell
.\scripts\run-webtransport-loadtest.ps1 ...
```

Mac 侧按固定频率采集：

```bash
ADMIN_A_URL=http://127.0.0.1:<ADMIN_A> \
ADMIN_B_URL=http://127.0.0.1:<ADMIN_B> \
NODE_A_PID=<本轮PID> NODE_B_PID=<本轮PID> \
OUTPUT_DIR=/安全留档目录 \
INTERVAL_SECONDS=5 \
bash scripts/collect-collab-metrics.sh
```

## 分档顺序

### 50 会话协议冒烟

建议额外跑一组 `2 clients / 1 document / edit-count=1`，验证共享文档双向编辑；规模基线跑 `50 clients / 200 documents`。

必须确认：

- AUTH/JOIN 全部成功；
- TLS 和 Origin 校验正常；
- Owner Redirect 不循环；
- 编辑 ACK 正常；
- 主动关闭不计入意外断线；
- 退出只清理本轮进程和数据。

### 1,000 会话对照

分别执行轻量 Go 客户端和既有真实 Chrome 客户端。分别记录，不把两个客户端的资源占用直接合并比较。

### 2,000 会话

轻量客户端先跑一轮，与既有 Chrome 两轮结果对照。进入下一档前必须解释 Windows 发压端、Mac 两个节点和依赖的资源余量。

### 5,000 会话

初始工作负载冻结为：

```text
200 个文档 × 约 25 个会话
```

另开热门文档负载：

```text
5 个写入者
每人每秒 1 次
先持续 5 分钟
```

### 10,000 会话

初始工作负载冻结为：

```text
200 个文档 × 50 个会话
持续在线 30 分钟
热门文档负载单独记录
```

10,000 的阈值尚未由导师确认。在阈值确认前，只报告原始结果，不判定通过或失败。

## 每轮采集

Windows：

- 逻辑会话数；
- Transport 实例数；
- 当前客户端尚未独立观测底层 QUIC 连接数，报告必须标记为 `NOT MEASURED`；
- JOIN P50/P95；
- 编辑 ACK P50/P95；
- 意外断线和编辑失败；
- 发压进程 CPU、RSS；
- 实际建连和编辑速率。

Mac：

- 每个节点 active clients；
- active rooms；
- Room event queue；
- QUIC connections；
- Go heap、goroutine、RSS；
- Redis/MySQL 操作延迟和失败；
- etcd lease renewal failures；
- 节点 CPU 和最忙节点。

## 失败规则

- 首次 JOIN 失败和最终 JOIN 失败分开记录；
- `Connection lost`、ACK 超时、意外断线、版本冲突分别记录；
- 不把 Windows CPU 饱和写成服务端容量瓶颈；
- 不在没有 Room/依赖指标证据时修改 Room；
- 一次只修改一个主要瓶颈；
- 前后对照必须保持相同提交、机器、房间分布、建连速率和写入负载；
- 未完成的档位写 `NOT RUN`，不得外推。

## 质量和安全

提交测试设施前执行：

```text
go test ./...
go test -race ./...
go vet ./...
go build ./...
bash -n scripts/*.sh
node --check scripts/webtransport-capacity.mjs
git diff --check
敏感信息扫描
```

控制 Token、Authorization、密码、Refresh Token、私钥和测试日志不得进入 Git 或普通报告。测试设施只能清理本轮明确创建的进程、Profile、容器、网络、账号和文档。

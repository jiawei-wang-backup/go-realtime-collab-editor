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

当前尚未声称覆盖：

- RESUME/SYNC/RESYNC 的完整断线恢复状态机；
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

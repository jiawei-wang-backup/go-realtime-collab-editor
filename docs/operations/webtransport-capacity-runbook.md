# WebTransport 压测执行手册

目标：先得到可复跑的 256／512 会话基线，再测双机 1,000／2,000。5,000／10,000 是单独的后续验收，不能用本机结果外推。当前可复跑入口是 `scripts/run-webtransport-benchmark.sh`，它调用已有的 `scripts/test-phase-11-distributed.sh` 和 `scripts/webtransport-capacity.mjs`。

## 0. 给执行模型的任务

> 先读本手册。只执行「本机流程」，每次只跑一轮，按 preflight → smoke → 50 → 256 → 512 顺序。成功才升档；出现失败、重试、资源残留或主机不稳时停在当前档，按失败处理记录证据。若机器重启，先报告，别自动复跑。不要改业务代码、真实 `.env`、证书校验或超时，也不要 commit、push、创建云资源或运行 Docker prune。双机或更高档需要先把准备条件落实，再单独执行。最后按本手册模板汇报。未经实测的项目填 NOT RUN。

## 1. 本机流程：周六优先做完

### 1.1 预检

```bash
cd '/Users/wangjiawei/Desktop/代码管理平台'
git status -sb
git rev-parse HEAD
df -h . /tmp
bash scripts/run-webtransport-benchmark.sh preflight
```

- 记下提交 SHA。基线期间不要切换或拉取分支。封装会拒绝除本手册和封装脚本以外的未提交项目改动。
- 需要 Go（`go.mod` 的 toolchain 为 `go1.26.10`）、Node 24.15.0 或以上、Python 3、Docker Desktop、真实 Google Chrome、Homebrew OpenSSL 3。缺前端依赖时，在磁盘空间充足的前提下执行 `(cd frontend && npm ci)`；缺镜像先报告，不让预检自动下载。镜像为 `mysql:8.4.13`、`redis:7.4.5-alpine`、`quay.io/coreos/etcd:v3.5.17`。
- 要求至少 **8 GiB** 可用空间，这是本次试跑的保守门槛；不达标就停，不擅自删文件或 Docker 资源。压测过程中如果空间低于 **4 GiB**、macOS 内存压力持续为红色或整机明显卡住，用 `Ctrl+C` 停止前台脚本，等清理，再检查资源归属。
- 只用可信、隔离的测试网络。原有 Go REST 服务按 `:PORT` 监听全部接口，虽然本机脚本使用 `127.0.0.1` 访问它；Vite、WebTransport、Admin、内部 RPC 和依赖按 fixture 配置绑定本机。不要在公共网络上运行，不配置公网转发。若不能隔离网络，先暂停。

### 1.2 先跑功能冒烟，再跑容量

以下命令**一条一条执行**。每行都有独立环境和报告，不要同时启动，别把它们接成一条无人值守的命令。

```bash
bash scripts/run-webtransport-benchmark.sh smoke
bash scripts/run-webtransport-benchmark.sh 50
bash scripts/run-webtransport-benchmark.sh 256
bash scripts/run-webtransport-benchmark.sh 512
```

| 步骤 | 实际测了什么 | 何时继续 |
| --- | --- | --- |
| `smoke` | 真实 Chrome，双节点 Owner Redirect、故障接管后继续协作 | Playwright 用例实际执行且通过，出现 `OWNER REDIRECT LOG: PASS`，退出码 0 |
| `50` | 验证注册、进入文档、WebTransport 编辑、环境清理都能走完 | 严格通过下表 |
| `256` | 自动经过 50、100、256，最后一档跑 30 次串行编辑 | 严格通过；保留每一级的连接数据 |
| `512` | 自动经过 50、100、256、512，复查之前出现过断连的档位 | 首轮严格通过、机器仍有余量时，再独立复跑一轮 `512` |

每次封装输出 `Evidence directory`。其中 `environment.txt` 有环境版本，`test.log` 有输出，`result.txt` 有真实退出码；更完整的 fixture 日志在输出提示的 `/tmp/collab-phase111-<RUN_ID>.log`。退出后复制需要留档的日志到自己的安全目录；不要提交日志、环境文件或证书。原 fixture 的临时目录会清掉，失败时仅将各服务日志尾部附到 fixture 报告。

```bash
# RUN_DIR 换成该次输出的 Evidence directory（不要用旧报告）。
cat "$RUN_DIR/result.txt"
rg -n 'TEST RUN:|provisionedUsers|"target"|"edits"|"editResult"|"shutdown"|FAIL|PASS' "$RUN_DIR/test.log"
```

### 1.3 通过标准与结果怎么读

| 检查项 | 定向判断 |
| --- | --- |
| 所有档位 | 每一级 `total == target`，`closed == 0`，`failures` 为空。严格基线还要求 `initialFailures` 为空；若重试后成功，记录「重试后通过」，先不升档。 |
| 最终档编辑 | `editResult.completed == 30`，最终 `editResult.online.total` 等于目标数、`closed == 0`、`version == 30`。当前是单个写入者每秒约 1 次编辑，共 30 次，不能当成多人同时写。 |
| 浏览器收尾 | `shutdown.unresolved == 0`，`profilesCleaned == browserProcesses`。`forced` 非零要记下来；若退出码 0、资源已清、编辑阶段无掉线，可记本轮业务通过、Chrome 强制收尾。 |
| Docker 收尾 | `Remaining test containers and networks` 后应无 ID，封装退出码为 0。原 fixture 打印的 `CLEANUP: PASS` 不能单独证明容器和网络已清理。 |
| P95 | 档位行的 `p95Ms` 是该档新增会话的连接／加入耗时，含建立连接和可能的跳转；`editResult.p95Ms` 是**发送者收到确认**的耗时。当前容量脚本不输出对端收到改动的 P95、逐节点内存、Room 队列或 etcd Lease 指标。没采集就写 NOT MEASURED。 |

**退出码提示：** 原容量脚本 `1` 表示测试失败，`2` 表示收尾检查有问题，也可能覆盖前面的业务失败；结合前后日志判断。封装还会因日志写入或 Docker 资源核对失败而失败。预检阻塞不算该档已经测试。

### 1.4 失败就停在当前档

- `429` 出现在建账号或发权限时：这是准备阶段被限流，不是压到容量上限。保留请求阶段与退出码，不去提高限额。
- `Connection lost`／`session is closed`：记第一个出错档位、是否重试成功、有没有对应的 `lease_lost`、`confirmation_timeout` 或服务退出。只看到时间接近时写「同时发生」，不要写成因果。
- `edit ack timeout`、版本不一致、运行中掉线：当前档失败，不升档。下一轮若要修复，先提出证据和改动范围，再用同一档复测。
- 仅 Chrome 收尾异常：分别写编辑结果和清理结果。不要用 `killall Chrome`。脚本 Hard Kill 或机器重启后，只用本轮已记录的 `RUN_ID` 查看资源；先报告有无残留和系统重启时间。不要清理身份不确定的资源。

```bash
# RUN_ID 只能填写本轮日志的 TEST RUN（12 位十六进制）。
docker ps -a --filter "label=com.collab-editor.test-run=$RUN_ID"
docker network ls --filter "label=com.collab-editor.test-run=$RUN_ID"
```

## 2. 双机：Windows 到手才做

**这一段是准备和验收清单。当前封装是本机版，不能把 `127.0.0.1` 填到 Windows 上直接跑。**

1. 固定同一提交。Mac 启动独立双节点环境；Windows 安装真实 Chrome、符合版本的 Node、并在仓库的 `frontend` 安装依赖。Windows 只发负载，MySQL／Redis／etcd 仍在 Mac。拿不到 Windows 就标记双机 NOT RUN，用 Mac 复核 256 或 512，不拿本机结果冒充双机数据。
2. 先准备可保持运行的双机 fixture，输出两个 REST origin、两个 WebTransport URL、一个浏览器页面 URL、WebTransport 证书 SHA-256 指纹和关闭命令。现有 shell fixture 会自己启动、自己收尾，不能直接拿它当常驻服务；此外它公布的是 loopback 的 WebTransport 地址。双机配置要同时改监听地址与 `NODE_BROWSER_ENDPOINT`，否则 Redirect 会把 Windows 指回自己的 `127.0.0.1`。管理口、MySQL、Redis、etcd、内部 RPC 和 Native QUIC 不开放给压测机。
3. Windows 能访问的浏览器页面使用 HTTPS，并让 Chrome 正常信任页面证书。测试 WebTransport 用符合浏览器要求的短期证书和正确的 SHA-256 指纹。Owner Redirect 到两节点的证书均须匹配当前脚本使用的**同一个**指纹。配置 Origin/CORS，放通测试页面和 REST 的 TCP 及 WebTransport 的 UDP，仅允许这台 Windows 发起访问。给 Windows 指纹即可，不复制私钥。先用 Chrome 验证页面与 WebTransport；`Test-NetConnection` 只能验证 TCP，不算 UDP 测通。
4. 准备完成后，在 Windows PowerShell 仓库根目录，把以下占位符替换为该轮 fixture 的实际值。同一档只启动一个压测进程。

```powershell
$env:REST_A_URL = '<REST A origin，无路径>'
$env:REST_B_URL = '<REST B origin，无路径>'
$env:WT_A_URL = '<https://Mac地址:端口/api/webtransport>'
$env:WT_B_URL = '<https://Mac地址:端口/api/webtransport>'
$env:CAPACITY_PAGE_URL = '<Chrome 信任的 HTTPS origin>/e2e/webtransport-capacity.html'
$env:WT_HASH = '<测试 WebTransport 证书的 Base64 SHA-256 指纹>'
$env:WEBTRANSPORT_LOAD_USERS = '50'
$log = Join-Path $env:TEMP ('collab-wt-' + (Get-Date -Format yyyyMMdd-HHmmss) + '.log')
node scripts/webtransport-capacity.mjs 2>&1 | Tee-Object -FilePath $log
$result = $LASTEXITCODE
"Exit: $result`nLog: $log"
```

5. 实测 50 通过才上 `1000`，再上 `2000`。保留双方 CPU、内存压力；Mac 侧另外采集两个管理口的 `/metrics` 和进程 RSS。每档使用同一套采样频率，记录每节点的活跃会话、Go heap、goroutine、RSS、Room 事件队列、Redis／MySQL 操作延迟、etcd 租约失效次数；Mac 的 RSS 从操作系统取得，不假设 Prometheus 进程指标一定存在。先复跑当前档，再比较，不把测试机 CPU 爆满解释成服务端容量不足。
6. 当前脚本会检查 Chrome 临时 profile 所属目录和名称，Windows 上还未验过；若关闭/清理报错，保留日志，先查实际 profile 和退出状态。脚本有 2,000 会话上限，也未给多个生成器协调同一批账号、热门文档和起跑时刻。**不要把上限改成 10,000 或同时开五次就宣称万人通过。**

双机 fixture 就绪后，下面是 Mac 上单次采样的命令。`ADMIN_A`、`ADMIN_B`、`NODE_A_PID`、`NODE_B_PID` 用该轮 fixture 的实际值；建议每 5 秒采一次，保留完整指标以便回看直方图。一次采样不能证明峰值。

```bash
STAMP=$(date -u '+%Y%m%dT%H%M%SZ')
curl -fsS --max-time 3 "http://127.0.0.1:$ADMIN_A/metrics" > "$RUN_DIR/node-a-$STAMP.prom"
curl -fsS --max-time 3 "http://127.0.0.1:$ADMIN_B/metrics" > "$RUN_DIR/node-b-$STAMP.prom"
ps -o pid,%cpu,rss -p "$NODE_A_PID,$NODE_B_PID" > "$RUN_DIR/process-$STAMP.txt"
rg '^(go_goroutines|go_memstats_heap_alloc_bytes|collab_room_clients_active|collab_room_event_queue_depth|collab_etcd_lease_renewal_failures_total)\b' \
  "$RUN_DIR/node-a-$STAMP.prom" "$RUN_DIR/node-b-$STAMP.prom"
```

## 3. 下一轮要补齐的验收

- **对端延迟和多写入者：**测发起编辑到另一会话收到对应 `edit_applied` 的耗时，按 `request_id` 或版本配对。单机同进程可用同一时钟；双机分别计时前先处理时钟偏差，或用同一负载进程内的一对会话。增加多个写入者并核对最终内容和版本。当前 `smoke` 验证对端能看到改动，没有量对端 P95。
- **持续在线和重连：**先在稳定档验证 20–30 分钟的连接保持，再定义一波要断开多少、在多长时间内回来；从实际断开算到恢复正确版本。`smoke` 只证明小规模接管，不能替代带负载的重连验收。保持在线与长时间编辑需要专门脚本或测试账户续期流程，现有 30 次编辑脚本不覆盖。
- **权限：**撤销读权限前后各发送一条可区分的编辑，确认被撤权会话看不到后续编辑，其他成员仍能收发。当前仓库的浏览器权限 E2E 位于 `frontend/e2e/collaboration.spec.ts`，需要另一个可用的浏览器测试环境；只有容量命令通过时，此项仍写 NOT RUN。
- **规模：**先和导师定清总在线会话、同时写入比例、热门文档人数、可接受的掉线率、对端延迟和恢复时限。两个节点的平均值不够，按最忙节点判断。双机 2,000 稳定、能区分生成端和服务端瓶颈后，再设计同步启动、多压测机的 5,000／10,000 验收。

## 4. 每轮报告模板

```text
日期／实际工时：
Commit／环境：单 Mac或双机；服务节点数；机器内存；Chrome/Go/Node 版本
档位／第几轮：
连接：各级目标/实际数、初次失败、最终失败、连接阶段掉线、连接 P95
编辑：完成数/30、最终版本、发送者确认 P95、对端 P95（没采集写 NOT MEASURED）
最忙节点：活跃连接、CPU、RSS、Go heap、goroutine、Room 队列、Redis/MySQL、租约异常
收尾：正常/强制关闭/未退出的 Chrome 数、profile 与容器网络清理结果
结论：PASS / PASS WITH RETRY / FAIL / BLOCKED / NOT RUN
第一处异常和证据：
日志路径／退出码：
下一步：该修测法、查服务端，还是上下一档
```

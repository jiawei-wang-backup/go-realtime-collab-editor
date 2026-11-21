# 1,000 会话 Owner 宕机专项验收

## 目的

在 Windows 保持 1,000 个真实 Chrome + WebTransport 会话在线时，由 Mac 停止当前文档 Owner 节点，验证客户端重连、Owner 接管、编辑连续性和节点恢复。该专项不能用普通容量脚本的最终在线数替代。

## 前置条件

- Windows IP 已固定且不切换网络、VPN 或热点。
- Windows 退出游戏和其他高负载程序，并禁用测试期间休眠。
- Mac fixture 使用全新临时端口、证书和 Root CA；不得混用上一轮参数。
- Chrome 已信任本轮 Root CA；不使用 `-k`、`NODE_TLS_REJECT_UNAUTHORIZED=0` 或关闭证书验证。
- 两个服务节点、MySQL、Redis、etcd 和 HTTPS Vite 页面均已就绪。
- 启动 fixture 时必须设置 `OWNER_FAILOVER_DRILL=true`，让 fixture 把目标 Owner 节点的退出视为预期事件；否则监控循环会在触发故障后清理整个环境。
- Windows 端专项 harness 必须报告 `READY` 后，Mac 才能停止 Owner。

## 控制协议

`scripts/webtransport-capacity.mjs` 的专项模式（`WEBTRANSPORT_FAILOVER_MODE=true`）通过 Mac 控制面切换故障：

- `POST /ready`：Windows 报告 READY；控制面记录并拒绝重复 READY。
- `GET /state`：读取是否 READY、是否已触发、触发时间和 Owner PID。
- `POST /trigger`：仅在 READY 后允许一次，向本轮绑定的 Owner PID 发送 SIGKILL，重复触发返回 409。
- 所有请求必须来自本轮 Windows IP 且携带 `X-Failover-Token`，否则返回 403。

Windows 侧环境变量：

```text
WEBTRANSPORT_FAILOVER_MODE=true
FAILOVER_CONTROL_URL=http://<Mac-IP>:<control-port>
FAILOVER_CONTROL_TOKEN=<本轮一次性 token>
FAILOVER_TRIGGER_MODE=harness   # harness 自测触发；external 等待外部触发
WEBTRANSPORT_CONNECT_CONCURRENCY=1
WEBTRANSPORT_CONNECT_INTERVAL_MS=100
```

READY 必须同时满足：

```text
active == 1000
initialFailures == {}
failures == {}
所有会话已完成 join/sync
```

READY 后 harness 等待 3 秒（`harness` 模式）或等待外部 `POST /trigger`（`external` 模式），随后最多等待 180 秒检查恢复。不得通过固定延迟猜测会话是否已准备好。

## 故障步骤

1. Windows 启动专项 harness，建立 1,000 会话。
2. Windows 报告 READY，并记录 READY 时间。
3. Mac 记录当前 Owner 节点、Owner incarnation 和 fencing token。
4. Mac 只停止当前 Owner 节点一次，并记录停止时间、PID、节点日志。
5. Windows 持续记录：
   - 首个断线时间
   - 断线会话数量
   - 重连开始和完成时间
   - Owner Redirect 数量
   - `SERVICE_UNAVAILABLE`、`join_response`、`lease_lost` 和其他首条错误
6. 新 Owner 恢复后，Windows 对所有会话执行 resume/sync 校验。
7. Windows 发送一组带唯一 request ID 的编辑，验证确认、最终版本和内容连续性。
8. Mac 恢复原节点，等待 etcd 成员数回到 2。
9. Windows 再执行一次编辑确认，验证恢复后的双节点仍能工作。
10. Windows 关闭会话、Chrome 和临时 Profile；Mac 清理服务、容器、网络和临时证书。

## 严格通过条件

专项模式下 `closed` 是累计关闭数，Owner 主动停止导致的关闭属于预期事件，不能用 `closed == 0` 判断；恢复后应以活动会话数为准：

```text
READY 时 active == 1000
Owner 停止前 initialFailures == {}
Owner 停止前 failures == {}
disconnects > 0 且 reconnects > 0（证明确实发生过故障接管）
恢复后 active == 1000
reconnectFailures == 0
恢复后版本 >= READY 版本
30 次编辑全部确认，版本从恢复版本开始连续递增
shutdown.unresolved == 0
profilesCleaned == browserProcesses
脚本退出码 0
无残留测试进程、容器、网络或临时 CA
```

任何会话永久丢失、版本跳跃、内容不一致、无法恢复或出现无法解释的首条错误，均为 FAIL；不得用后续重试覆盖首次失败。

## Mac 控制面

`scripts/owner-failover-control.py` 提供受限的单次故障控制面。它只允许指定 Windows IP 携带一次性 `X-Failover-Token` 访问；未 READY 时拒绝触发，重复 READY 或重复触发也会拒绝。启动时必须绑定 Mac 的局域网 IP，不能绑定公网接口，并且 `--owner-pid` 必须是本轮 fixture 创建的测试节点 PID。

示例参数（端口、IP、token 和 PID 必须替换为本轮值）：

```bash
python3 scripts/owner-failover-control.py \\
  --bind 172.16.30.211 \\
  --port <control-port> \\
  --allowed-ip 172.16.30.107 \\
  --token <one-time-token> \\
  --owner-pid <owner-pid>
```

本地验证已覆盖：错误 token 返回 403、重复 READY 返回 409、READY 后单次 trigger 返回 200、重复 trigger 返回 409，并确认只终止指定测试 PID。

## 当前状态

- 2 浏览器 Owner 接管冒烟：PASS。
- Mac 控制面：已实现，本地已验证认证拒绝、单次 READY、单次 trigger 和 PID 终止。
- 专项 harness：已在 `scripts/webtransport-capacity.mjs` 实现 READY、单次故障触发、断线/重连统计、重连 P50/P95/P99、恢复版本和编辑连续性校验。
- 本地 50 会话故障自测：PASS。50/50 会话断线并全部重连，`reconnectFailures=0`，编辑 30/30，最终版本 30，退出码 0。
- 本地 256 会话故障自测：PASS。READY 时 `active=256`、`ownerSessions=256`、仅 Owner 端点；断线 256、重连 256、`reconnectFailures=0`；重连 P50 364ms、P95 5280ms、P99 5940ms；编辑 30/30，版本 30，退出码 0。该轮有 2 个 Chrome 进程被强制收尾，但 `unresolved=0` 且 Profile 3/3 清理，按手册记为业务通过、Chrome 强制收尾。

专项模式的关键行为：

- 文档 ID 通过 rendezvous 哈希筛选，确保全部优先归属目标 Owner（默认 `node-a`），否则单节点宕机只能影响约一半会话。
- READY 统计跨全部浏览器页汇总，避免多页时只读取第一页导致误判。
- 初始会话全部连接到 Owner 端点；Owner 终止后客户端优先连接存活节点，并把重定向回已终止 Owner 视为可快速重试。
- 重连每个页面最多 8 个并发并带 0–300ms 抖动，避免 Chrome 同时重建大量 QUIC 连接。
- 观察窗口默认 90 秒，覆盖 QUIC 空闲超时；之后要求计数稳定 5 秒才判定恢复。

fixture 的文档由 Node A 创建且哈希筛选后归属 Node A，因此本轮 Owner 固定为 Node A，控制面绑定 `NODE_A_PID`。若后续测试需要杀死其他节点，必须显式传入对应 PID 和 `FAILOVER_OWNER_INDEX`，不能猜测。

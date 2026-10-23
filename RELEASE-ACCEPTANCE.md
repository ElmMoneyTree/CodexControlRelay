# Device Claim 发布验收矩阵

本文件把 Device Claim 目标拆成可验证门槛。自动化通过只证明对应范围；生产发布必须同时满足最后两项外部验收，不能用本地测试代替。

## 需求与证据

| 目标 | 权威实现/证据 | 当前状态 |
| --- | --- | --- |
| 插件首次启动只生成并证明设备身份，不猜用户归属 | `create-device-claim.mjs` 生成 Ed25519 密钥、nonce 和签名；Relay 验签；插件请求不含私钥 | 自动化通过 |
| 已登录用户用二维码/链接选择个人或团队空间 | iPhone 扫码/深链与原生确认页；Relay 托管纯二维码页；Chrome E2E 通过现有 API 模拟手机选择团队并批准 | 自动化通过 |
| 一次性短期认领 | 五分钟默认 TTL、独立 claim secret 与 poll token、重复批准/过期/错误 secret 测试 | 自动化通过 |
| 认领密钥不进入服务端 URL 日志 | 浏览器 secret 位于 fragment，页面立即清除；二维码用 `X-Claim-Secret`；Chrome E2E 验证清除 | 自动化通过 |
| 关闭页面后友好恢复同一认领 | 插件用 Windows CurrentUser DPAPI 临时保存 secret；重试只重开现有 claim，不重复创建；默认输出不含 URL | PowerShell 恢复夹具通过；Windows DPAPI 待真机确认 |
| 统一服务完成租户绑定并下发目标 Relay | 批准事务验证 workspace 角色、绑定设备、轮换 token；poll 返回 device token、workspace 与 `relayUrl` | 自动化通过 |
| 设备级最小权限凭据 | 设备 token 只能同步自身状态、领取自身命令和 ack；控制 API 需账号 session | 自动化通过 |
| 多租户与团队角色隔离 | workspace/membership；Owner/Admin/Member/Viewer；跨租户读取/写入与 Viewer 写入测试 | 自动化通过 |
| 撤销与审计 | 撤销立即使 token 失效、清除状态并失败化命令；认领/迁移/成员/命令均写审计 | 自动化通过 |
| 旧配对原位迁移 | 旧 token 发起 claim；保留 device ID、任务与待办；批准后轮换 token；过期不改旧配置 | 自动化通过 |
| 保持任务查看和消息发送 | Agent 状态同步、App 列表、消息命令、Agent ack；跨组件 E2E | 自动化通过 |
| 保持命令/文件/权限审批 | App 编码完整 request/turn 身份；插件校验实时请求与可选决定；审批 E2E 与权限单测 | 自动化通过 |
| iPhone 端账号、空间、认领、撤销和审计 | SwiftUI 实现与 API/模型 XCTest；签名 App 已真机安装 | 运行时测试通过；生产账号接口已部署（2026-10-23 只读确认） |
| 电脑认领页仅提供二维码 | 无头 Chrome 验证二维码、无账密控件、状态轮询、批准后成功提示、受限关闭与弹窗实际关闭、断网恢复；现有 API 模拟手机批准并验证审计 | 自动化通过 |
| CentOS 与 Windows 插件发行包完整 | 静态 Linux amd64 Relay、systemd 安装器；插件 manifest、逐文件 SHA-256 和 ZIP 完整性验证 | 自动化通过 |
| 生产 Relay 已升级 | `/health`、`/ready` 为 `200`，`/v1/me` 未登录为 `401` | **2026-10-23 只读确认；不代表本次 0.2.6 已部署** |
| Windows Codex Desktop 真实链路 | PowerShell 5.1、DPAPI、内置 Node、Windows named pipe、首次同步；运行 `verify-device-claim.ps1 -BeginClaim` | **待 Windows 真机执行** |

## 可重复自动化

在上层工作区执行：

```bash
cd CodexControlRelay
go test ./...
go test -race ./...
go vet ./...
cd ..
node browser-claim-e2e.mjs
node e2e-device-claim.mjs
cd CodexControlPluginCode
node --test test/*.test.mjs
cd ../CodexControlPlugin
node scripts/verify-release.mjs
```

iOS：

```bash
cd CodexControlApple
xcodebuild -project CodexControlApple.xcodeproj \
  -scheme CodexControl \
  -destination 'platform=iOS Simulator,name=iPhone 17 Pro' \
  test
```

## 生产门槛

CentOS 升级后执行：

```bash
curl -fsS https://codex-relay.kuajingvs.com/health
curl -fsS https://codex-relay.kuajingvs.com/ready
curl -sS -o /dev/null -w '%{http_code}\n' https://codex-relay.kuajingvs.com/v1/me
```

结果必须依次为成功、成功、`401`。若最后仍是 `404`，说明 Nginx upstream 后仍运行旧 Relay。

Windows 10/11 x64 上保持 Codex Desktop 与至少一个任务打开，在插件目录执行：

```powershell
.\scripts\verify-device-claim.ps1
.\scripts\verify-device-claim.ps1 -BeginClaim
```

在手机 App 批准后，第二条必须输出 `"ok": true` 和 `"phase": "connected"`。随后从 iPhone 对该任务发送一条消息，并处理一条真实审批；两条命令都必须变为 completed 且出现在审计记录中，才可签署生产验收。

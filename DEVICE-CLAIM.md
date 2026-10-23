# Device Claim 设备认领

Device Claim 是 Codex Control 的默认连接模型。插件只声明“我是哪台设备”，用户归属由已登录的人在手机 App 中明确选择，因此同一 Relay 可安全服务多个用户和团队。

## 完整流程

1. 插件本地生成 Ed25519 密钥对和随机 nonce，签名规范化的设备名、公钥与 nonce。私钥不发送到 Relay，并立即使用 Windows 当前用户的 DPAPI 加密落盘。
2. Relay 验证持钥证明，创建五分钟有效的 `pending` 认领，分别签发认领密钥和仅供插件轮询的 token。
3. 认领密钥仅放在链接 fragment 中；fragment 不会随 HTTP 请求发送。电脑页面仅展示二维码与扫码提示，不提供账号密码登录或认领批准入口。页面轮询只读状态接口，批准后显示成功并尝试关闭窗口；关闭受限时保留成功提示，清除临时密钥并停止轮询。页面用 `X-Claim-Secret` 请求二维码，避免 Nginx 访问日志记录密钥。
4. 已登录用户用手机 App 扫码，查看设备名与公钥指纹，选择自己有写权限的个人或团队空间并确认。手机 App 在确认前要求 Face ID、Touch ID 或设备密码。
5. Relay 在同一数据库事务中复核成员角色、消费认领、绑定 workspace、创建或迁移设备、生成设备级 token，并写入审计事件。
6. 插件用独立 poll token 领取设备 token 与目标 Relay。设备 token 只可同步该设备状态、领取分配给该设备的命令并确认执行结果。

认领只能批准一次。认领与尚未领取的设备 token 会在同一个短期窗口结束时失效；网络中断可在窗口内安全重试。

## 空间和角色

| 角色 | 查看任务 | 发送消息/审批 | 认领设备 | 撤销设备 | 管理成员 |
| --- | --- | --- | --- | --- | --- |
| Owner | 是 | 是 | 是 | 是 | 是 |
| Admin | 是 | 是 | 是 | 是 | 是，但不能修改 Owner 或其他 Admin |
| Member | 是 | 是 | 是 | 否 | 否 |
| Viewer | 是 | 否 | 否 | 否 | 否 |

设备、任务、待办、命令与审计均按 workspace 查询。命令创建会在事务内同时验证用户角色和设备归属，防止并发成员变更或跨租户设备 ID 造成串单。

## 凭据与存储

- 账号密码：Argon2id。
- 账号 session、设备 token、认领密钥、poll token：数据库只保存 SHA-256。
- 待领取设备 token：AES-GCM 包裹，密钥由服务器管理令牌派生；领取窗口结束即删除。
- Windows 设备 token、私钥、poll token 和认领有效期内用于重新打开页面的认领密钥：Windows DPAPI CurrentUser 加密，并收紧为当前用户 ACL；认领批准或过期后删除临时记录。
- iPhone 账号 session：系统 Keychain。
- HTTP 日志：Relay 只记录路径，不记录查询或请求体；认领密钥不使用查询参数。

撤销设备会立即让设备 token 失效、删除其任务与待办，并把未完成命令标记失败。登录退出会删除对应 controller session。

## 主要 API

- `POST /v1/auth/register`、`POST /v1/auth/login`、`POST /v1/auth/logout`
- `GET /v1/me`
- `GET|POST /v1/workspaces`
- `GET|POST /v1/workspaces/{id}/members`
- `POST /v1/device-claims`
- `GET /v1/device-claims/{id}`
- `POST /v1/device-claims/{id}/approve`
- `GET /v1/device-claims/{id}/poll`
- `GET /v1/device-claims/{id}/qr`
- `GET /v1/device-claims/{id}/status`：使用 `X-Claim-Secret`，仅返回状态和有效期，不返回设备凭据，也不消费插件的凭据领取流程。
- `GET /v1/state?workspaceId=...`
- `POST /v1/commands?workspaceId=...`
- `POST /v1/devices/{id}/revoke`
- `GET /v1/audit-events?workspaceId=...`

管理令牌保护的 `/control/accounts` 仅用于账号 bootstrap。`/control/state`、`/control/commands`、`/control/pairing-codes` 和 `/agent/enroll` 是迁移兼容接口，可通过 `security.legacy_api_enabled: false` 关闭。

## 旧设备原位迁移

新版插件检测到 schema 1 的旧配置时，不会先删除它。它使用旧设备 token 调用认领接口，并在认领批准前继续保持旧连接。批准事务会：

1. 验证该设备尚未被账号管理；
2. 失败化迁移前遗留的 queued/claimed 命令，避免旧控制端命令跨边界执行；
3. 保留 device ID、任务和待办；
4. 轮换设备 token、写入公钥指纹并移动到选定 workspace；
5. 记录 `device.migrated` 审计事件。

插件领取新凭据后原子覆盖本地配置并重启 Agent。若认领过期，旧 token 与旧配置均不会改变。

## 验收门槛

- 非法设备签名、错误认领密钥、错误 poll token、过期或重复批准必须失败。
- 两个账号不能查看或操作彼此空间；Viewer 写入必须失败。
- 幂等重试只能产生一个命令和一条 `command.created` 审计。
- 命令完成或失败、设备认领/迁移/撤销、空间和成员变更均可审计。
- 旧数据库升级后设备 token 与数据保持有效。
- 新设备与旧设备迁移都能完成：插件证明 → 手机 App 批准 → token/Relay 下发 → Agent 同步 → App 发消息/审批 → Agent ack。

在包含 Relay、插件和手机端三个项目的上层工作区中，可重复运行：

```bash
node e2e-device-claim.mjs
node browser-claim-e2e.mjs
```

第一条验证设备密钥证明、账号/空间绑定、任务同步、消息与审批命令、ack 和审计；第二条使用真实无头 Chrome 执行认领页 JavaScript，验证页面仅展示二维码、无账密和批准控件、不调用账号 API、URL fragment 清除、带旧会话刷新及失效链接提示，并通过现有 API 模拟手机端选择团队空间和批准认领，验证成功提示、受限关闭、弹窗实际关闭及断网恢复。认领批准时设备仍为离线，只有设备 Agent 首次同步后才变为在线。

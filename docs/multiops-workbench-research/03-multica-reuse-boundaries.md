# Multica 能力复用与边界分析

## 1. 当前可复用骨架

### Workspace 与权限边界

**FACT**

- Multica 的主要数据按 `workspace_id` 隔离。
- API 客户端通过 workspace header 选择上下文。
- Issue 创建、父子关系和 Project 归属在服务层检查 workspace 边界。
- Agent、Runtime、Autopilot 和配置均具有 workspace 归属。

**复用判断：强复用。** Connector 实例、外部绑定、凭据引用、事件和审批均应继承 workspace 边界，不能另建全局默认租户。

### Issue 与协作时间线

**FACT**

现有 Issue 支持：

- 标题、描述、状态、优先级
- 人或 Agent 多态指派
- Project、父子 Issue、日期、附件
- creator 和有限 origin provenance
- 评论、回复、reaction、附件
- Agent 创建 Issue 和评论

`server/internal/handler/issue.go` 的创建入口已经将跨 workspace 校验委托给 `IssueService.Create`；`server/internal/integrations/channel/engine/router.go` 也通过服务接口创建 Issue，而不是直接写表。

**复用判断：强复用作为协作对象；弱复用作为外部系统账本。**

Issue 适合承载标题、描述、负责人、协作和对用户可见的处理状态，但不应承载外部 Payload、游标、每次同步尝试和审批授权快照。

### Agent、Runtime 与 Daemon

**FACT**

- Agent 绑定 Runtime，支持 instructions、skills、环境变量、MCP 配置和 provider 配置。
- Daemon 注册 workspace runtime、领取任务、准备隔离工作目录、注入 Issue/Comment/Project/Autopilot/Initiator 上下文并执行 CLI Agent。
- 任务强制要求 `workspace_id`，防止落入用户全局配置。
- Agent 执行已经具有进度、重试、恢复、心跳和孤儿任务恢复等机制。

**复用判断：强复用执行面。** 不应另建第二套 Agent 调度器。外部工单应通过标准 Issue/Task 或明确的新任务入口进入现有 Runtime。

**限制：** Daemon 的任务可靠性不等于外部 API 副作用可靠性；外部写操作仍需幂等键、effect ledger、审批 Token 和 Outbox。

### Autopilot、Webhook 与定时触发

**FACT**

Autopilot 已表达：

- workspace、标题、描述、负责人
- 启用状态和 execution mode
- Issue 模板
- cron/webhook/manual 等触发来源
- run 状态、下次运行时间和订阅者
- Autopilot 上下文被注入 Agent 执行环境

**复用判断：中到强复用。** 可复用为“触发并创建/执行工作”的入口，但不能直接等同于 Integration Sync：Connector 还需要 cursor、外部对象身份、Payload、delivery attempt 和 writeback 状态。

### Skills、Tools 与 MCP

**FACT**

Multica 已把 Skills 和 MCP 配置下发到 Agent Runtime，并具有内置 Skills 文档和 CLI 行为约束。

**复用判断：强复用 Agent 工具入口。** 外部系统能力应包装为稳定的 Workbench Tool，例如：

- `external_item_get`
- `external_comment_add`
- `external_status_update`
- `approval_request`
- `approval_execute`
- `notification_send`

Agent 不应直接获得 Ferry、Jira 或企业微信凭据。

### WebSocket 与前端共享层

**FACT**

- 前端已有 workspace-scoped WebSocket 客户端和重连机制。
- TanStack Query 是服务器状态来源，WebSocket 应 patch 或 invalidate cache。
- `packages/core` 提供 API、schemas、queries、stores；`packages/views` 提供 Web/Desktop 共享业务 UI。
- Web 和 Desktop 的平台路由分别留在 app/platform 层。

**复用判断：强复用。** Integration Center、Delivery Center、Approval Timeline 若同时服务 Web/Desktop，应遵守 `views -> core + ui` 边界。

## 2. 缺失的领域原语

当前代码没有形成下列完整能力：

| 原语 | 目的 | 不应塞入的位置 |
| --- | --- | --- |
| Connector Definition | 声明类型、版本、能力和配置 Schema | Agent instructions |
| Connector Installation | workspace 内的连接实例和状态 | Workspace settings 大 JSON |
| Credential Reference | 引用密钥，不向 Agent 暴露值 | Issue metadata |
| External Record | 保存来源对象身份、版本、摘要和原始快照引用 | Issue description |
| Issue Binding | Issue 与一个或多个外部对象的关系 | Issue origin 单字段 |
| Integration Event | 入站事件、schema version、幂等键和处理状态 | WebSocket 消息历史 |
| Sync Cursor | 拉取游标与 checkpoint | Autopilot last_run_at |
| Outbox Message | 可靠写回、通知和发布 | Agent comment |
| Delivery Attempt | 每次请求、错误分类、重试和响应摘要 | 普通日志 |
| Approval Request | 输入快照、风险、审批人、过期和状态 | Issue status |
| Policy Decision | 使用的策略版本和决策理由 | LLM 输出 |
| Effect Ledger | 外部副作用、供应商 request id 和幂等键 | Agent task result |

## 3. 建议 bounded contexts

### Work Management（现有 Multica 核心）

Issue、Comment、Project、Member、Agent、Squad、用户协作状态。

### Agent Execution（现有 Multica 核心）

Runtime、Daemon、Agent Task、Session、Skill、MCP、执行日志和进度。

### Integration（新增边界）

Connector Definition/Installation、Credential Reference、External Record、Binding、Mapping、Cursor。

### Delivery（新增边界）

Inbound Event、Outbox、Attempt、DLQ、Replay、Notification Delivery。

### Approval & Policy（新增边界）

Action Proposal、Risk Classification、Policy Decision、Approval Request、Approval Token、Resume Signal、Effect Ledger。

### Automation（演进边界）

Trigger、Condition、Action、Template 和版本化流程。初期可复用 Autopilot，避免立即创建通用 DAG 引擎。

## 4. 上游合并风险

### 高冲突区域

- 直接扩展 `issue` 核心表加入大量外部字段
- 修改通用 Issue 状态枚举表达同步/审批/执行状态
- 在 `packages/core/types/issue.ts` 和共享 Issue API 中加入 Ferry/Jira 字段
- 在现有 Daemon 内实现每个外部系统协议
- 在 `packages/views/issues/` 中硬编码来源系统 UI
- 修改通用 Agent task 生命周期来表达外部 delivery

### 较低冲突路径

- 新建独立 integration/delivery/approval 表和服务包
- 通过 `IssueService`、Task enqueue 和事件接口集成
- 在 `packages/core` 新建领域 API/schema/query 模块
- 在 `packages/views/integrations`、`delivery`、`approvals` 新建共享页面
- 平台 app 只做路由 wiring
- 外部 Connector 通过进程外 HTTP/gRPC 或稳定 adapter interface 运行

## 5. 复用矩阵

| 能力 | 复用程度 | 说明 |
| --- | --- | --- |
| Workspace / membership / RBAC 基础 | 高 | 新域必须继承 workspace 隔离 |
| Issue / Comment / Attachment | 高 | 作为用户协作和呈现层 |
| Agent / Runtime / Daemon | 高 | 继续承担 Agent 执行 |
| Skills / MCP | 高 | 提供统一受控工具 |
| WebSocket / Query invalidation | 高 | 新域事件接入现有实时链路 |
| Web/Desktop shared packages | 高 | 新页面遵守现有共享边界 |
| Autopilot trigger | 中高 | 可承担早期 schedule/webhook/manual trigger |
| Issue origin 字段 | 低到中 | 可做 provenance，不足以替代多绑定模型 |
| Issue metadata | 低 | 不应成为无模式外部数据仓库 |
| Issue status | 低 | 不能承载 source/run/approval/sync 全部状态 |
| Daemon retry | 低 | 不能替代外部副作用幂等和 Outbox |

## 6. 架构无法回答的产品问题

1. 用户首先需要统一收件箱，还是自动完成低风险动作？
2. Ferry 工单进入 Multica 后，Ferry 仍是事实源还是 Multica 成为主系统？
3. 用户是否需要在 Multica 编辑外部字段，还是只需要查看和回写评论/状态？
4. 一个 Issue 绑定多个外部对象是常见场景还是未来想象？
5. Approval 是 Workbench 原生对象，还是只展示外部审批状态？
6. 哪些动作可自动批准，谁对策略负责？
7. 用户愿意维护 Mapping/Template DSL 吗，还是希望系统提供垂直模板？
8. Delivery Center 是日常操作页面还是仅管理员排障页面？
9. 是否真的存在第二个 Connector 的近期需求？
10. Web、Desktop 和 Mobile 哪个是首个主要运维入口？

## 7. 推荐发现顺序

1. 用 Ferry 样本建立 External Record、Binding、Approval 和 Delivery 状态表。
2. 用一个非 Ferry 的模拟来源检验模型是否通用。
3. 先验证入站幂等、审计和可重放，不做真实写回。
4. 再验证评论或状态的一种单向写回。
5. 最后才评估声明式 Connector、Temporal、Cedar 或外部 Connector Service。

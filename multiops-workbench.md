对，这个定位已经比较完整了。更准确地说，你要建设的是：

> **以 Multica Issue 为统一工作对象，以 Agent 和自动化为执行能力，以 Connector Framework 连接外部任务、工单、审批、通知和结果系统的智能运维 Workbench。**

Ferry、禅道、Jira、自研工单、GitLab Issue 都只是 Connector 实现，不应该进入 Workbench 核心业务代码。

## 一、建议把系统拆成四层

```text
┌─────────────────────────────────────────────┐
│                  前端 Workbench              │
│  统一任务箱 / 工单详情 / Agent运行 / 审批 / 集成管理 │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│                 Workbench Core              │
│ Issue / Comment / Run / Approval / Automation│
│ Template / Routing / Audit / Permission      │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────▼──────────────────────┐
│             Integration Framework           │
│ Connector / Mapping / Event / Sync / Outbox  │
└───────────┬─────────────┬─────────────┬─────┘
            │             │             │
        工单系统        审批系统       通知系统
     Ferry/禅道/Jira    OA/原工单API    企业微信
```

核心原则是：

* 外部系统负责产生业务对象
* Workbench 负责统一处理和执行
* Agent 负责分析、决策和调用工具
* Connector 负责协议适配
* Mapping 和 Template 负责把外部数据变成可执行 Issue
* Outbox 负责可靠回写和通知

## 二、Connector 不要按系统分类，而要按能力分类

同一个外部系统可能同时提供工单来源、审批和结果回写。例如 Ferry 可以同时具备：

* `IssueSource`
* `CommentSink`
* `StatusSink`
* `ApprovalProvider`

企业微信则可能具备：

* `NotificationSink`
* `InteractiveActionProvider`

所以不要定义一个非常庞大的 `FerryConnector` 接口，建议采用能力接口：

```go
type IssueSource interface {
	Pull(ctx context.Context, cursor string) (*PullResult, error)
	Get(ctx context.Context, externalID string) (*ExternalItem, error)
	ParseWebhook(ctx context.Context, input WebhookInput) ([]ExternalEvent, error)
}

type IssueWriter interface {
	AddComment(ctx context.Context, externalID string, content string) error
	UpdateStatus(ctx context.Context, externalID string, status string) error
	AddAttachment(ctx context.Context, externalID string, file FileRef) error
}

type ApprovalProvider interface {
	CreateApproval(ctx context.Context, req ApprovalRequest) (*ApprovalRef, error)
	GetApproval(ctx context.Context, approvalID string) (*ApprovalState, error)
	ExecuteApprovalAction(ctx context.Context, action ApprovalAction) error
}

type NotificationSink interface {
	Send(ctx context.Context, message NotificationMessage) error
}
```

每个 Connector 通过 Manifest 声明自身能力：

```json
{
  "name": "zentao",
  "version": "1.0.0",
  "capabilities": [
    "issue.pull",
    "issue.webhook",
    "issue.comment.write",
    "issue.status.write"
  ]
}
```

Workbench 根据能力决定前端展示和自动化动作，而不是判断：

```go
if connector.Type == "ferry" {
    // 不建议
}
```

## 三、建议提供三种接入模式

不是所有用户都会开发 Go Connector，因此可以提供三个层级。

### 1. 通用 Webhook Connector

外部系统按照统一协议推送：

```json
{
  "event_id": "evt-20260710-001",
  "event_type": "issue.created",
  "source": "custom-ticket",
  "data": {
    "id": "1024",
    "title": "测试环境 vmagent 无数据",
    "content": "从 10:30 开始没有监控数据",
    "status": "approved",
    "priority": "high",
    "labels": ["monitoring", "test"],
    "fields": {
      "environment": "test",
      "service": "vmagent"
    }
  }
}
```

这是接入成本最低的方式。

### 2. 声明式 REST Connector

用户通过配置描述：

* 拉取 URL
* 认证方式
* 分页方式
* 增量游标
* 字段提取表达式
* 状态映射
* 回写接口

例如：

```yaml
source:
  list:
    method: GET
    path: /api/tickets
    query:
      updated_after: "{{ cursor }}"
  detail:
    method: GET
    path: /api/tickets/{{ external_id }}

mapping:
  id: $.id
  title: $.subject
  content: $.description
  status: $.state
  priority: $.priority
  updated_at: $.updatedAt
```

常规工单系统可以不写代码直接接入。

### 3. 自定义 Connector SDK

复杂系统使用 Go SDK 或独立 Connector Service。

不建议长期依赖 Go `plugin` 机制，因为它和 Go 版本、依赖版本、编译环境耦合严重。可以采用：

```text
Workbench
    │ HTTP/gRPC Connector Protocol
    ▼
独立 Connector Service
```

这样用户可以用 Go、Python、Java 编写适配器，也不需要重新编译 Multica。

## 四、统一数据模型不要直接等于 Multica Issue

外部数据先标准化为 `ExternalItem`：

```go
type ExternalItem struct {
	ExternalID string
	Key        string
	URL        string
	Source     string

	Title       string
	Content     string
	Type        string
	Status      string
	Priority    string
	Requester   UserRef
	Assignee    *UserRef
	Labels      []string
	Attachments []Attachment
	Comments    []ExternalComment
	Fields      map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
	Raw        json.RawMessage
}
```

然后通过 Issue Template 转换成 Multica Issue。

建议数据关系为：

```text
ExternalItem
     │
     │ Mapping + Template
     ▼
Multica Issue
     │
     ├── Agent Runs
     ├── Approval Requests
     ├── Automation Executions
     └── External Bindings
```

不要把全部外部字段直接加到 Issue 表。应保留：

```text
external_records      外部对象快照
issue_bindings        Issue 与外部对象关联
integration_events    入站事件
sync_cursors          拉取游标
outbox_messages       回写队列
delivery_attempts     回写执行记录
```

而且一个 Issue 最好支持绑定多个外部对象：

```text
Multica Issue OPS-1024
├── 禅道 Bug #481
├── Ferry 审批单 #239
└── 企业微信群消息 #message-id
```

这对于跨系统协作很有价值。

## 五、Issue Template 是整个系统的关键

Connector 只负责“把数据带进来”，Issue Template 决定“进来以后怎么处理”。

一个模板至少应该包含：

```yaml
name: test-monitoring-troubleshooting

match:
  source:
    - ferry
    - zentao
  fields:
    environment: test
  labels:
    any:
      - monitoring
      - vmagent

issue:
  title: "[{{ source }}:{{ key }}] {{ title }}"
  description_template: monitoring-troubleshooting.md
  project: monitoring
  priority: "{{ priority }}"

routing:
  agent: vm-troubleshooter
  skills:
    - linux-diagnostics
    - victoria-metrics
  tools:
    policy: readonly

automation:
  on_created:
    - assign_agent
    - start_run
    - notify_wecom

writeback:
  on_run_started:
    status: processing
  on_run_completed:
    status: waiting_confirmation
    add_comment: true
```

这样用户的使用流程就是：

```text
创建 Connector
    ↓
配置字段映射
    ↓
选择 Issue Template
    ↓
选择 Agent / Skill / 自动化策略
    ↓
开始接收工单
```

## 六、状态必须拆开管理

不要只用一个 `status` 字段承载所有含义。

建议至少拆成：

```text
source_status       外部工单状态
workbench_status    Workbench处理状态
run_status          Agent运行状态
approval_status     审批状态
sync_status         同步状态
```

例如：

```text
source_status: approved
workbench_status: in_progress
run_status: running
approval_status: passed
sync_status: synced
```

前端可以综合展示，但数据库和自动化规则必须分开。

否则双向同步时很容易互相覆盖、重复执行或者形成状态回环。

## 七、自动化应该围绕事件构建

建议内部统一事件：

```text
external.issue.created
external.issue.updated
issue.created
issue.assigned
run.started
run.completed
run.failed
approval.requested
approval.approved
approval.rejected
writeback.failed
```

然后由自动化规则消费：

```yaml
trigger:
  event: run.completed

conditions:
  issue.fields.environment: test
  run.result: success

actions:
  - external.add_comment
  - external.update_status
  - wecom.send_notification
```

Webhook 和定时拉取最终都转换成相同的内部事件：

```text
Webhook ─┐
         ├─▶ ExternalEvent ─▶ Normalize ─▶ Template ─▶ Issue
Poller ──┘
```

这样不会出现 Webhook 一套逻辑、轮询另一套逻辑。

## 八、Agent 操作外部系统也应该通过 Workbench Tool

不要把 Ferry、禅道或企业微信凭据直接暴露给 Agent。

提供统一工具：

```text
external_issue_get
external_issue_add_comment
external_issue_update_status
approval_create
approval_get_status
approval_execute_action
notification_send
```

Agent 只传递：

```json
{
  "issue_id": "issue-123",
  "action": "add_comment",
  "content": "已定位为 vmagent 配置加载失败。"
}
```

Workbench 再负责：

* 解析绑定的外部系统
* 检查 Agent 权限
* 检查操作策略
* 写入 Outbox
* 调用 Connector
* 记录审计日志
* 重试失败操作

这样将来更换工单系统时，Agent 和 Prompt 都不需要修改。

## 九、审批自动化需要增加策略层

调用原系统审批 API 没问题，但建议区分：

```text
自动提交审批
自动查询审批状态
自动执行低风险审批
人工审批后继续自动化
```

不要让 Agent 根据自然语言直接决定是否审批。

可以设计审批策略：

```yaml
approval_policy:
  auto_approve:
    environments:
      - local
      - test
    actions:
      - read_logs
      - restart_single_stateless_instance
    max_risk: low

  human_required:
    environments:
      - production
    actions:
      - deploy
      - change_firewall
      - database_operation
```

每次自动审批必须保存：

* 使用的策略版本
* Agent 提供的理由
* 风险等级
* 实际参数
* 操作身份
* 外部审批结果
* 完整审计记录

## 十、前端建议形成六个主要模块

### 统一工作台

显示所有来源的任务：

```text
来源    编号       标题                  状态       执行者
禅道    BUG-481    登录接口响应慢         排查中     API Agent
Ferry   OPS-239    测试环境服务重启       待审批     自动化
Webhook EXT-991    vmagent 无监控数据      已完成     VM Agent
```

### Issue 详情

建议包含：

* 工单概览
* 外部来源
* Agent 运行时间线
* 评论和附件
* 审批记录
* 自动化操作
* 回写记录
* 同步日志

### 集成中心

管理：

* Connector 实例
* 凭据
* Webhook 地址
* 定时拉取
* 过滤条件
* 连接测试
* 最近错误

### 映射与模板

管理：

* 字段映射
* 状态映射
* Issue Template
* Agent 路由
* 写回规则

### 自动化中心

管理：

* Trigger
* Condition
* Action
* Agent
* Tool Policy
* Approval Policy

### Delivery Center

这是很重要但经常被忽略的页面：

* 入站 Webhook
* 拉取记录
* 回写记录
* 企业微信通知记录
* 失败原因
* 重试
* 手动重放
* 原始 Payload

## 十一、企业微信不要写死为特殊功能

企业微信应当作为：

```text
Notification Connector
Interactive Action Connector
```

通知模板由 Workbench 管理：

```yaml
event: approval.requested

recipients:
  - issue.requester
  - project.owners

template:
  title: "运维审批：{{ issue.title }}"
  content: |
    环境：{{ issue.fields.environment }}
    操作：{{ approval.action }}
    风险：{{ approval.risk }}
```

后续可以自然增加：

* 钉钉
* 飞书
* Slack
* Email
* Telegram
* Webhook

而不需要修改 Workbench Core。

## 十二、推荐的实际开发顺序

### 第一阶段：建立 Workbench 骨架

优先完成：

1. Integration 数据模型
2. Connector Manifest 和 Capability
3. 通用 Webhook Connector
4. `ExternalItem` 统一模型
5. Issue Binding
6. Issue Template
7. 自动 Agent 路由

这一阶段先做到“外部任务可以进入 Multica 并自动执行”。

### 第二阶段：双向处理

增加：

1. Comment 回写
2. Status 回写
3. Outbox 和重试
4. 同步日志
5. 企业微信通知
6. Delivery 管理页面

### 第三阶段：通用适配能力

增加：

1. 声明式 REST Connector
2. 定时增量拉取
3. 游标和分页
4. 字段映射页面
5. 状态映射页面
6. Connector SDK

### 第四阶段：审批与自动化

增加：

1. Approval Provider
2. Approval Policy
3. Agent Tools
4. 自动审批和人工审批混合流程
5. 外部审批状态恢复 Agent Run

## 最值得坚持的几个架构原则

**Workbench Core 不认识 Ferry、禅道或 Jira。**

**Connector 只负责系统协议，Template 负责业务语义。**

**Agent 只调用统一 Workbench Tool，不直接调用外部系统。**

**Webhook、轮询、人工导入最终都进入同一个事件处理管道。**

**回写、审批、通知必须经过权限、策略、Outbox 和审计。**

**内置 Connector 和外部 Connector Service 同时支持，避免所有适配都必须修改 Multica 源代码。**

按这个方向做，最终它就不只是“运维工单平台”，而会成为一个通用的：

> **多数据源任务接入 + Agent 执行 + 人机审批 + 自动化回写的 AI Workbench。**


# 外部框架与协议调研

> 这是技术选项地图，不是依赖清单。版本、许可证和托管限制在采用前必须再次核对官方资料。
>
> 基准日期：2026-07-10

## 1. 选择原则

优先级从高到低：

1. 采用稳定协议和数据模式
2. 复用小型库或清晰的进程外服务
3. 借鉴成熟产品的 UX 和领域模型
4. 最后才引入大型平台

避免同时引入多个拥有自己 credential、cursor、retry、workflow 和 schema 语义的平台。

## 2. Connector / Integration

### Nango

价值：OAuth、Token refresh、多租户 connection、API proxy、同步和 TypeScript integration functions。

风险：主仓库采用 Elastic License 2.0；不得未经审查将其主要功能作为托管或受管服务提供。不能默认当作可自由白标嵌入的开源底座。

建议：作为候选侧车和产品设计参考；只有部署边界、商业模式和许可证经过确认后才进入技术选型。

官方来源：

- `https://github.com/NangoHQ/nango`
- `https://docs.nango.dev/`

### Singer / Meltano

价值：tap/target 流式协议简单，适合批量同步、数据导入和独立插件。

限制：偏 ELT，不擅长实时业务 Action、审批和低延迟双向写回。

建议：如果未来需要把大量外部对象同步到本地索引或仓库，可复用协议思想；不作为默认 Connector Action Runtime。

官方来源：

- `https://www.singer.io/`
- `https://github.com/meltano/meltano`

### Airbyte

价值：连接器生态、CDK、数据库和 API 数据复制能力。

风险：Elastic License 2.0；Python/容器运行模型较重，产品嵌入和再分发需审查。

建议：只在大规模数据复制成为明确需求时评估；不用于普通工单 Action。

官方来源：`https://github.com/airbytehq/airbyte`

### Pipedream

价值：组件元数据、Action 输入输出和开发者体验值得借鉴。

限制：核心价值依赖托管平台，组件源码不等于完整自托管运行时。

建议：学习设计，不作为基础依赖。

官方来源：`https://github.com/PipedreamHQ/pipedream`

## 3. Durable Workflow / Automation

### Temporal

价值：Go-first durable execution、Activity retry、Timer、Signal、Cancellation、Workflow history 和 replay testing。

适用阈值：

- 流程跨小时或天
- 需要审批后恢复同一执行
- 需要可靠 Timer、取消和补偿
- 多个外部副作用需要可追踪编排

代价：独立服务、数据库、worker、namespace、版本治理和确定性约束。

重要反例：Temporal Workflow 成功不代表外部 API exactly-once。Worker 可能在外部请求成功后、记录结果前崩溃；仍需供应商幂等键或自有 Effect Ledger。

建议：不要在第一阶段默认引入。先证明普通数据库状态机 + worker + outbox 不足以满足需求。

官方来源：

- `https://docs.temporal.io/`
- `https://github.com/temporalio/temporal`

### Windmill / n8n / Kestra

价值：可视化流程、节点 Schema、Credential UI、执行检查、脚本资源化。

风险：

- n8n 使用 Sustainable Use License
- Windmill 社区版包含 AGPL/Apache/商业边界
- Kestra 引入 Java 服务体系

建议：作为 Automation UX 和节点模型参考，不直接白标嵌入。可视化 DAG 不是可靠性边界，执行语义仍需版本化领域模型。

### StackStorm / Rundeck

价值：Ops trigger/action/rule、Runbook、RBAC、节点、审计和自助操作。

限制：平台较重，产品模型偏基础设施自动化，不是 Agent-first 协作 Workbench。

建议：借鉴 Action Pack、审批、审计和 Execution 页面；不作为核心运行时。

官方来源：

- `https://github.com/StackStorm/st2`
- `https://github.com/rundeck/rundeck`

## 4. Agent Runtime

### LangGraph

价值：Agent 内部有状态图、checkpoint、thread、interrupt 和人工恢复。

边界：适合 Agent 内部推理/工具循环，不应成为租户级业务账本、全局调度器或外部副作用授权系统。

建议：仅在现有 Multica Agent provider 无法表达复杂可恢复 Agent 流程时作为独立 Agent Service 评估。

官方来源：`https://docs.langchain.com/oss/javascript/langgraph/persistence`

### OpenHands

价值：Sandbox、Runtime image、Action/Observation 和隔离执行模型。

建议：学习其代码执行隔离；若 MultiOps 允许 Agent 执行 shell 或修改基础设施，应使用 rootless container、网络出口白名单、资源限制和短期凭据。不要在 Go API 进程中直接执行 Agent 生成命令。

官方来源：

- `https://docs.openhands.dev/openhands/usage/architecture/runtime`
- `https://github.com/OpenHands/OpenHands`

### Dify / Flowise

价值：快速 Agent/Workflow 原型。

风险：产品边界、运行时重复和许可证；Dify 对多租户使用有额外限制。

建议：仅用于独立实验，不作为 MultiOps 多 workspace 后端。

## 5. 事件与契约

### OpenAPI

用于同步 REST 管理面、Connector Action 和配置 API。

### CloudEvents

用于统一事件 Envelope：

```json
{
  "specversion": "1.0",
  "id": "evt_example",
  "source": "connector/ferry/installation/example",
  "type": "external.item.updated",
  "subject": "ticket/1234",
  "time": "2026-07-10T00:00:00Z",
  "data": {}
}
```

建议扩展字段：workspace、correlation、causation、schema version 和 idempotency key。

CloudEvents 只定义 Envelope，不定义业务 Payload、权限或事件存储。

官方来源：`https://github.com/cloudevents/spec`

### AsyncAPI

用于异步事件契约、消费者文档和测试；不替代 Broker、Schema Registry、事件账本或权限。

官方来源：`https://www.asyncapi.com/docs/reference/specification/latest`

### MCP

用于 Agent 访问受控 Tool、Resource 和 Prompt。不要将内部管理 API 未经授权直接暴露给 Agent。

官方来源：`https://github.com/modelcontextprotocol/modelcontextprotocol`

### A2A

只有出现跨组织或跨独立 Agent Runtime 的能力发现和委派需求时才采用。当前不应先行引入。

官方来源：`https://github.com/a2aproject/A2A`

## 6. 可靠性模式

这些语义应属于 Workbench 自身：

- Transactional Outbox：业务状态和待发布记录同一数据库事务
- Inbox：以 consumer + event id 唯一约束消费
- Idempotency Key：区分消息幂等和外部操作幂等
- DLQ：保存失败分类、原始 Envelope 引用和处置历史
- Replay：支持 dry-run、限流、指定 handler version
- Retry classification：仅瞬时故障自动重试
- Correlation / Causation：从 Root Issue/Action 追踪全部事件
- Effect Ledger：记录外部请求 ID、幂等键、结果和补偿动作

不要声称 exactly-once；目标应是至少一次传递 + 幂等处理 + 可审计副作用。

## 7. Policy / Approval

### Cedar

适合产品内 RBAC/ABAC 授权，例如谁能批准哪个 workspace、环境、Action 和风险等级。

边界：它只做授权决策，不做认证、审批请求生命周期、通知、执行或审计。

官方来源：`https://docs.cedarpolicy.com/`

### OPA / Rego

适合已有 Rego 资产的基础设施、部署、网络和平台 Guardrail。

建议：不要让 Cedar 和 OPA 同时管理同一权限域。没有复杂策略需求前，数据库权限模型和明确服务代码可能更简单。

官方来源：`https://www.openpolicyagent.org/docs`

## 8. 工作管理 / Helpdesk 参考

Plane、OpenProject、Zammad、GLPI 可用于学习：

- Issue/Work Package
- 队列、SLA 和服务目录
- 审计时间线
- Triage 和项目视图
- 插件治理

不建议直接 fork 为核心产品：它们的许可证、语言栈和领域模型都会放大维护成本，并与 Multica Agent Runtime 重复。

## 9. 当前 shortlist

### 现在即可采用的思想

- OpenAPI + CloudEvents + AsyncAPI
- MCP 受控工具接口
- Outbox / Inbox / Idempotency / DLQ / Effect Ledger
- Connector capability manifest
- 进程外 Connector 边界

### 达到阈值后再评估

- Temporal：长流程和审批恢复已证明需要 durable execution
- Cedar：规则数量和授权复杂度已超过普通服务代码
- Nango：SaaS OAuth 连接数量足够多，且许可证/商业边界已确认
- Singer/Meltano：批量数据同步成为明确产品需求
- LangGraph：Agent 内部可恢复图成为瓶颈

### 明确不建议先做

- 同时引入多个 Connector 平台
- 白标嵌入 n8n/Windmill
- 用 Airbyte 处理工单 Action
- 用 Dify 替代 Multica Agent Runtime
- A2A 先行
- 将可视化 DAG 当作业务事实源

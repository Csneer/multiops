# 产品原型与架构候选

> 本文档用于脑暴和评审，不构成实施授权。

## 1. 先重新定义问题

当前构想容易从“已经跑通 Ferry 接入”直接跳到“建设通用 Connector + Automation + Approval 平台”。这中间至少有四个未验证跳跃：

1. Ferry MVP 的价值来自通用平台，还是来自特定值班流程自动化？
2. 用户需要统一任务协作，还是需要安全执行低风险操作？
3. 第二个外部系统是否近期真实存在？
4. Multica Issue 是否足以作为主对象，还是只适合用户协作层？

## 2. 候选产品原型

### Option A：Ferry Ops Copilot

定位：把当前 MVP 产品化，专注 Ferry 工单的排障、审批和回写。

范围：

- Ferry Installation
- 工单入站与绑定
- 只读排障
- 白名单审批
- Delivery/Audit 页面
- 企业微信通知

优点：最快利用真实证据，用户和场景明确。

反对意见：容易把 Ferry 特例写进核心；若没有第二个系统，通用能力价值无法验证。

适用条件：首要目标是尽快提高当前值班效率。

### Option B：Unified Ops Inbox

定位：不同来源任务进入统一 Issue 协作箱，Agent 提供分析和建议，默认不做自动外部写操作。

范围：

- 通用 Webhook
- External Record + Binding
- Template/Mapping
- Agent 路由
- 来源、同步和审计时间线

优点：副作用风险较低，可验证统一工作对象和多来源价值。

反对意见：可能只是“复制工单到另一个工单系统”，用户需要来回切换；没有写回会形成数据孤岛。

适用条件：信息聚合和协作是主要痛点。

### Option C：Safe Ops Action Workbench

定位：以 Action Proposal、Policy、Approval 和 Effect Ledger 为核心，Issue 是上下文和协作入口。

范围：

- Action proposal
- 风险分级
- 人工审批
- 受控工具执行
- 副作用审计与回滚信息

优点：直接解决 Agent 自动化最关键的安全与治理问题，差异化强。

反对意见：产品抽象较新，用户心智和审批体验尚未验证；实现复杂度高。

适用条件：用户真正愿意让 Agent 进行生产副作用操作。

### Option D：Connector Platform

定位：用户可通过 Webhook、声明式 REST 或 SDK 自行接入任意系统。

优点：平台化和生态潜力最大。

反对意见：在第二个真实 Connector 前极易过度设计；认证、分页、游标、Webhook、写回和版本兼容差异巨大。

适用条件：已有多个接入团队和明确的连接器开发需求。

### Option E：Automation Builder

定位：Trigger → Condition → Agent/Action → Approval → Writeback 的可视化编排。

优点：表达能力强，业务人员可配置。

反对意见：会同时引入 DSL、版本、运行时、调试器、权限、循环、并发和迁移问题；与 Autopilot 及外部工具重叠。

适用条件：重复流程数量和配置需求已被实际使用证明。

## 3. 产品候选对比

| 方向 | 验证速度 | 通用性 | 安全风险 | 平台复杂度 | 上游冲突 | 当前证据 |
| --- | --- | --- | --- | --- | --- | --- |
| A Ferry Ops Copilot | 高 | 低 | 中高 | 低中 | 中 | 高 |
| B Unified Ops Inbox | 高 | 中 | 低 | 中 | 低 | 中 |
| C Safe Ops Action Workbench | 中 | 中高 | 可治理但设计难 | 高 | 低中 | 中低 |
| D Connector Platform | 低 | 高 | 高 | 很高 | 中 | 低 |
| E Automation Builder | 低 | 高 | 高 | 很高 | 中高 | 低 |

## 4. 候选技术架构

### Architecture 1：模块化单体优先

```text
Multica Server
├── Work Management（现有）
├── Agent Execution（现有）
├── Integration Module（新增）
├── Delivery Module（新增）
└── Approval Module（新增）
        │
        └── Connector Adapter / HTTP
```

特点：同一 PostgreSQL，模块独立表和服务接口；Background worker 处理 Outbox。

优点：部署简单、事务边界清晰、可快速验证。

风险：若边界不严格，Ferry 逻辑会渗入核心 handler/service。

推荐：第一阶段首选。

### Architecture 2：核心单体 + Connector Sidecar

```text
Multica Server
  ├── Integration / Delivery / Approval
  └── Connector Protocol Client
             │ HTTP/gRPC
             ▼
      Connector Service(s)
```

特点：凭据和供应商 SDK 可隔离；Connector 可用 Go/Python/Java 编写。

优点：减少依赖冲突，便于独立升级和外部生态。

风险：分布式认证、版本协商、网络重试、部署和可观测性成本增加。

采用阈值：至少两个复杂 Connector，或供应商 SDK/安全边界确实要求进程隔离。

### Architecture 3：Durable Workflow Platform

```text
Multica Server → Temporal
                    ├── Integration Worker
                    ├── Approval Worker
                    └── Agent/Action Worker
```

优点：长流程、Timer、Signal、恢复和历史强。

风险：基础设施和确定性约束显著增加；仍不能替代 Outbox 和外部幂等。

采用阈值：审批等待、长时间执行、补偿和跨系统恢复已经成为主要复杂度。

## 5. 当前更稳妥的组合假设

**HYPOTHESIS**

产品上先结合 Option A + B：

- 使用真实 Ferry 闭环验证
- 模型按来源无关方式设计
- 默认只读/建议
- 只选择一种低风险写回做受控验证
- Delivery/Audit 从第一天可见

架构上先采用 Architecture 1：

- 独立 Integration/Delivery/Approval 模块
- 复用现有 Issue、Agent、Runtime、Autopilot、Realtime
- PostgreSQL Outbox/Inbox
- 不先引入 Temporal、Nango、Cedar 或可视化 DSL
- Connector adapter 保留未来进程外演进可能

这只是当前置信度最高的候选，不是正式决策。

## 6. 必须认真考虑的反对意见

### “不要再造工单系统”

如果外部系统仍是事实源，Workbench 应聚焦 Agent 执行、审批和交付，不复制完整 ITSM/Jira 功能。

### “Issue 可能不是正确的统一对象”

事件、审批和外部对象拥有独立生命周期。Issue 可以是协作视图，但底层不能丢失它们的身份和状态。

### “通用 Connector SDK 可能永远用不到”

若 Connector 主要由内部团队维护，一个清晰的内部 adapter interface 可能长期足够。

### “Agent 不是审批者”

Agent 可提出 Action 和理由，但授权必须来自确定性策略或人类批准。

### “平台越通用，首次价值越晚”

Integration、Automation、Policy、Workflow、SDK 和 Marketplace 同时建设会把验证周期推迟数月。

### “继续合并上游可能比功能开发更贵”

如果大量修改 Issue、Daemon 和共享核心文件，未来每次合并 Multica 上游都会持续冲突。新增 bounded context 更安全。

## 7. 待验证问题清单

### 用户与价值

1. 谁每天使用？频率和当前耗时是多少？
2. Ferry MVP 哪一步节省时间最多？
3. blocked 通知是否真的促使请求人补齐信息？
4. 用户是否愿意主要在 Multica 中处理，而不是回 Ferry？
5. 结果成功的指标是什么：响应时间、人工步骤、审批耗时还是故障恢复时间？

### 数据和状态

6. 外部系统和 Multica 谁是事实源？
7. 是否需要双向字段编辑？
8. 同一 Issue 多绑定是否有真实样本？
9. 来源状态、Workbench 状态、Run 状态、Approval 状态和 Sync 状态如何展示？
10. 原始 Payload 保存多久，谁可以查看？

### 安全与审批

11. 哪些动作可以自动批准？由谁维护 allowlist？
12. Approval 必须绑定哪些不可变参数？
13. 凭据属于用户、workspace、connector 还是 runtime？
14. Agent 能看到哪些字段和附件？
15. 如何阻止 SSRF、越权、命令注入和敏感数据外发？

### Connector

16. 第二个真实 Connector 是什么？
17. Webhook、轮询还是二者都需要？
18. 是否需要 OAuth，还是内部 Token/账号密码为主？
19. Connector 由平台团队还是最终用户开发？
20. 供应商 API 失败时谁负责人工处置？

### 运行与商业

21. 单机 self-host 是否仍是核心部署形态？
22. 用户愿意运维 Temporal/Nango 等附加服务吗？
23. 外部依赖许可证是否允许未来商业托管？
24. Mobile 是否需要审批和处置能力？
25. 与 Multica 上游保持多高频率同步？

## 8. 下一轮材料建议

在进入实施计划前，建议补齐：

1. 访谈或复盘 5–10 个真实 Ferry 工单
2. 一张当前人工流程泳道图
3. 每一步耗时、失败和回退数据
4. 第二个来源系统的三个真实 Payload 样本（脱敏）
5. 低风险与高风险 Action 分类表
6. 外部字段、状态和身份映射表
7. 一份 Approval immutable snapshot 示例
8. 一份入站重复、乱序和写回失败的演练结果
9. 一次上游合并冲突基线评估
10. 由用户明确批准的首个产品原型选择

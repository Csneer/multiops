# MultiOps Workbench MVP Spec v1

> 状态：Draft / Planning only。本文档用于明确 MVP 范围、边界与验收思路，不构成实施授权。
>
> 基准日期：2026-07-10

## 1. Goal

在不改造 Multica 核心协作模型的前提下，验证一个最小可用的 MultiOps Workbench 基线：

1. 能接收一个真实外部来源（先以 Ferry 为参考）的入站对象或事件
2. 能在 Multica 内为其建立独立的外部记录与 Issue 绑定
3. 能向用户展示来源、同步、处理和审计信息
4. 能把该对象路由到现有 Agent / Runtime / Issue 协作链路
5. 默认以只读和建议为主，不默认开放任意副作用写回

这不是“通用 Connector 平台 MVP”，而是“真实来源 + 受控领域模型 + 可见审计”的 MVP。

## 2. Product Thesis

### 2.1 要验证的核心命题

- Multica 是否适合作为外部运维事项的协作与执行工作台，而不只是内部 Issue 系统
- 外部对象生命周期是否应独立建模，而不是直接塞进 Issue 核心表
- 用户是否愿意在一个统一界面里查看来源对象、Agent 分析和处理状态
- 在不先建设大型平台能力的情况下，是否已经能形成可验证的产品闭环

### 2.2 当前不验证的命题

- 任意 REST 系统都能零代码接入
- 用户需要可视化 Automation Builder
- 需要多 Connector SDK / Marketplace
- 需要长时 durable workflow 平台
- Agent 已被授权执行广泛生产写操作

## 3. MVP Users

### Primary user

- 使用 Ferry 类外部系统处理日常运维事项的值班或支持人员

### Secondary user

- 需要查看审计、失败、重试和同步状态的管理员或平台维护者

### Not in v1

- 终端业务人员自助接入任意系统
- 非技术用户自行编排流程
- 多组织跨平台 Agent 委派

## 4. Scope

### 4.1 Included

1. 一个来源类型的入站接收能力
2. 外部记录（External Record）最小模型
3. Issue 与外部记录的绑定模型
4. 同步/处理状态的最小可见性
5. 只读详情展示与时间线摘要
6. 基于现有 Agent / Runtime 的分析或建议路由
7. 审计友好的事件/尝试记录最小面
8. 管理员可见的失败与重试入口定义（即使首版只先展示，不一定提供真实重放按钮）

### 4.2 Explicitly excluded

1. 通用 Connector SDK
2. 用户自定义 Mapping DSL
3. 可视化 Automation Builder
4. 多来源统一收件箱的完整产品化体验
5. 通用审批引擎
6. 高风险写回自动化
7. Temporal / Cedar / Nango 等额外平台依赖的正式引入

## 5. MVP Outcome

如果 MVP 成功，应能证明以下事情至少有初步证据：

- 外部对象需要独立于 Issue 的领域模型
- Multica 现有 Issue + Agent + Runtime 骨架可承载该工作流
- 用户能理解来源状态、Workbench 状态与协作状态之间的差异
- 只靠 bounded context 扩展，而非大改核心 Issue 域，也能形成最小闭环

## 6. Functional Requirements

### FR-1 Inbound intake

系统应能接收一个外部来源的最小入站对象或事件，并完成：

- 来源身份解析
- workspace 归属决策
- 幂等识别
- 原始载荷引用保存策略
- 标准化摘要提取

### FR-2 External Record

系统应能为每个外部对象创建独立记录，至少包含：

- source type
- external id / stable identity
- source title / summary snapshot
- source status snapshot
- schema version
- last seen time
- raw payload reference
- workspace scope

### FR-3 Issue Binding

系统应能把一个外部记录绑定到一个 Multica Issue，而不是把所有来源字段直接存入 Issue 本体。

绑定关系至少应支持：

- one issue ↔ one external record
- later extensible to one issue ↔ many external records
- provenance / source indicator
- source deep link or canonical reference placeholder

### FR-4 Read-only visibility

用户应能在 Multica 中查看：

- 该 Issue 关联的来源对象摘要
- 来源状态快照
- 最近同步时间
- 最近处理结果
- 失败或阻塞原因摘要

### FR-5 Agent routing

系统应能把已绑定的外部对象上下文提供给现有 Agent 执行链路，用于：

- 生成建议
- 请求补充信息
- 输出分类或处理意见
- 更新协作层 Issue/comment

### FR-6 Delivery/Audit minimum surface

系统应至少记录并展示：

- 入站事件或处理尝试
- 成功 / 失败状态
- 错误分类摘要
- 时间戳
- 关联 issue / external record

### FR-7 Controlled writeback pilot contract

即使首版默认不实施真实写回，也要在领域模型里为“单一低风险写回试点”预留边界：

- outbox-like message concept
- delivery attempt concept
- idempotency key field
- effect ledger placeholder

注意：这只要求模型和接口边界清晰，不要求 v1 真的执行写回。

## 7. Non-Functional Requirements

### NFR-1 Workspace isolation

所有新域对象都必须显式归属于 workspace，并遵守现有权限边界。

### NFR-2 Upstream merge safety

MVP 应尽量通过新增 bounded context 实现，避免大量修改：

- issue 核心表
- 通用 issue 状态枚举
- daemon 通用生命周期
- shared issue types

### NFR-3 Auditability

所有关键处理路径必须可追踪到：

- 来源对象
- 绑定 issue
- 处理时间
- 结果摘要
- 错误摘要

### NFR-4 Security boundary

Agent 不直接拥有外部系统凭据；外部系统访问应通过受控服务/工具边界暴露。

### NFR-5 Incremental delivery

首版应支持先只做 read-only 闭环，再决定是否进入低风险写回试点。

## 8. Proposed Domain Slice

### Integration context

负责：

- connector definition（最小占位）
- installation / source identity（可最小化）
- external record
- issue binding
- cursor / ingest position（如首版需要）

### Delivery context

负责：

- inbound event or ingest attempt log
- outbox placeholder
- delivery attempt summary
- replay-ready metadata placeholder

### Approval context

v1 不做完整实现，但要明确：

- 不把审批语义塞进 issue status
- 为后续 action proposal / approval request / effect ledger 预留独立边界

## 9. UX Surface v1

### 9.1 Shared Issue surface

在现有 Issue 视图中，以增量方式增加“外部对象摘要区块”，展示：

- 来源类型
- 外部标识
- 最近同步时间
- 来源状态
- 最近处理结果

### 9.2 Admin / operator surface

增加最小管理页或列表页，用于查看：

- 外部记录列表
- 绑定状态
- 最近失败
- 最近处理尝试

### 9.3 Explicitly avoid in v1

- 大量新导航结构
- 复杂多栏工作台
- 可视化 flow builder
- 面向终端用户的 connector self-service

## 10. Success Metrics

### Product signals

1. 至少一个真实来源对象链路能稳定进入 Multica
2. 用户能在一个界面中看懂来源摘要、协作状态和处理结果
3. 用户能用现有 Agent 流程完成至少一种只读分析闭环
4. 失败可见，不靠日志 grep 才知道发生了什么

### Engineering signals

1. 新能力主要通过新增表/服务/页面实现
2. 不需要大改 issue/daemon/shared core types
3. 新接口遵守 workspace-scoped 与 schema-validated 约束
4. 为后续单向低风险写回保留了清晰演进路径

## 11. MVP Acceptance Criteria

- 可以接收一种真实来源的入站数据并完成标准化落库
- 可以为来源对象建立独立外部记录，而不是只写进 Issue 描述
- 可以把外部记录与 Issue 建立绑定并展示给用户
- 可以记录并查看至少一类处理尝试结果与失败摘要
- 可以将绑定上下文交给现有 Agent 协作链路
- 不要求通用 connector 平台，不要求多来源，不要求完整审批

## 12. Risks

### Risk A: Ferry 特例误导通用建模

缓解：

- 所有字段设计优先抽象 identity / snapshot / binding / attempt，而非复刻 Ferry 流程字段

### Risk B: 过早进入写回

缓解：

- 首版以 read-only + visible audit 为默认闭环
- 真实写回仅保留边界与试点位

### Risk C: Issue 过载

缓解：

- 外部对象、同步、审批、副作用均独立建模
- Issue 只承担协作与展示中心

### Risk D: 上游冲突扩大

缓解：

- 优先新增模块、表、queries、views
- 避免修改核心共享 issue 语义

## 13. Open Questions

1. v1 是否需要真正的 cursor / polling 模型，还是只需手动/模拟入站即可验证产品
2. 第一来源是否必须就是 Ferry，还是应做一个 Ferry-like 脱敏样本接入
3. Issue 绑定是否从第一天就支持一对多，还是先按一对一落模
4. 管理页面优先做在 Issue 内嵌视图，还是独立 integrations/delivery 页面
5. 首个“低风险写回试点”是否只定义契约、不进入实现范围

## 14. Recommended Phase-1 Build Order

1. 定义最小领域模型与命名边界
2. 定义最小 API / schema / query surface
3. 做 read-only inbound → external record → issue binding 闭环
4. 做失败/尝试可见性
5. 接入现有 Agent 上下文
6. 再决定是否继续推进单向低风险写回试点

## 15. Non-Goals Reminder

这份 MVP spec 不授权：

- 开始实现代码
- 引入新平台依赖
- 扩张为通用产品平台
- 接受任意高风险自动副作用

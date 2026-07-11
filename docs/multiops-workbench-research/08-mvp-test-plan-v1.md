# MultiOps Workbench MVP Test Plan v1

> 状态：Draft / Planning only。本文档定义 MVP 方案阶段的测试思路、验收场景与证据要求，不代表当前仓库已经具备这些能力。
>
> 基准日期：2026-07-10

## 1. Test Strategy Goal

在不实施生产代码的前提下，先明确未来 MVP 应如何被验证，避免先做实现、再临时补测试口径。

本测试计划服务于两个目标：

1. 验证 MVP spec 是否自洽、可验收、边界清楚
2. 为后续实现阶段提前定义最小测试金字塔和关键验收用例

## 2. Testing Principles

- 优先验证真实产品命题，不优先验证抽象平台能力
- 优先验证 bounded context 边界是否清楚，而不是单个函数实现细节
- 优先验证审计、失败可见性和 workspace 隔离
- 对“未来可能有”的能力只定义占位测试，不制造伪范围
- 不把日志 grep 当主要验收手段；关键状态应通过 UI/API/存储对象可见

## 3. Coverage Map

### 3.1 Spec-level coverage

要验证文档本身是否回答了：

- MVP 到底做什么、不做什么
- 为什么先做 read-only 闭环
- 为什么不先做通用 connector 平台
- 用户成功的定义是什么
- 后续写回试点边界是否被清楚限制

### 3.2 Future implementation coverage

后续实现阶段至少应覆盖：

1. Domain model tests
2. API schema / contract tests
3. Backend service tests
4. Query and parsing tests in `packages/core`
5. Shared UI rendering tests in `packages/views`
6. End-to-end acceptance tests for the golden flow

## 4. Acceptance Test Themes

### Theme A: Inbound ingestion

系统能接收一个来源对象并形成标准化记录。

### Theme B: External record and binding

系统能独立存储来源对象，并与 Issue 绑定。

### Theme C: Visibility and audit

用户能看见来源摘要、最近同步、最近处理结果与失败信息。

### Theme D: Agent context routing

绑定后的来源上下文能进入现有 Agent 协作链路。

### Theme E: Isolation and safety

workspace 隔离、权限边界、只读默认策略和写回未授权边界可被验证。

## 5. Spec Acceptance Checklist

在进入实现前，这份 spec 应先通过文档级验收：

- [ ] Included / excluded scope 明确
- [ ] MVP 目标不是通用平台，而是一个真实闭环
- [ ] 外部对象独立建模的理由清楚
- [ ] 新增 bounded context 与现有 Multica 核心边界清楚
- [ ] 成功指标与失败可见性可观察
- [ ] 写回仅为受控试点边界，不构成默认能力
- [ ] 至少能映射到一组具体测试场景

## 6. Proposed Future Test Matrix

| Area | Purpose | Suggested level |
| --- | --- | --- |
| External record model | 验证 identity/snapshot/workspace constraints | Backend unit/integration |
| Issue binding model | 验证 relation and uniqueness rules | Backend integration |
| Inbound ingest handler/service | 验证 idempotency and normalization | Backend integration |
| Delivery attempt log | 验证 success/failure summary visibility | Backend integration |
| API schemas | 验证 malformed response tolerance | TS unit |
| Shared issue external summary UI | 验证 rendering and empty/error states | Views test |
| Operator list/detail UI | 验证 state presentation and filters | Views test |
| Golden flow | 验证 end-to-end user path | E2E |

## 7. Core Scenarios

### Scenario 1: New inbound object creates external record

**Given** 一个 workspace 收到新的来源对象

**When** 系统处理该对象

**Then** 应创建一个 external record，保存其稳定身份、摘要快照与 workspace 归属

**Evidence expected**

- external record 可查询
- issue 尚未错误依赖描述字段承载全部来源数据
- recent ingest result 可见

### Scenario 2: External record binds to issue

**Given** 一个 external record 已存在

**When** 系统为其创建或关联一个 Issue

**Then** 应形成显式 binding，而不是只依赖标题正则或 description 拼接

**Evidence expected**

- binding record 存在
- issue 页面能显示来源摘要
- source provenance 可追踪

### Scenario 3: Duplicate inbound is idempotent

**Given** 同一个来源对象被重复投递或重复拉取

**When** 系统再次处理

**Then** 不应产生重复 external record，也不应无意义重复建 Issue

**Evidence expected**

- identity uniqueness 生效
- 处理结果标记为 duplicate/no-op/update
- 审计记录保留本次尝试

### Scenario 4: Agent receives external context

**Given** 一个绑定了 external record 的 Issue

**When** 触发现有 Agent 协作链路

**Then** Agent 应获得受控的来源摘要上下文，而不是直接获得任意凭据

**Evidence expected**

- Agent 可生成分析/建议
- 协作结果写回 issue/comment
- 无外部凭据直接暴露给 Agent

### Scenario 5: Failure is visible

**Given** 入站标准化或后续处理失败

**When** 用户或管理员查看相关页面

**Then** 应能看到失败时间、失败摘要、关联对象，而不是只能读日志

**Evidence expected**

- delivery/attempt summary visible
- issue or operator view 有失败摘要
- 可以区分“未处理”和“处理失败”

### Scenario 6: Workspace isolation holds

**Given** 两个不同 workspace 存在同类来源对象

**When** 用户在 workspace A 查看数据

**Then** 不应看到 workspace B 的 external record、binding 或处理结果

**Evidence expected**

- query 层按 workspace 过滤
- API contract 明确需要 workspace context
- UI 不会跨 workspace 泄漏对象

### Scenario 7: Read-only default is enforced

**Given** MVP 仍处于默认只读策略

**When** 用户通过 MVP 路径触发处理

**Then** 不应发生未批准的外部写回

**Evidence expected**

- 没有 active writeback call path
- 或 writeback path 仅为 disabled / placeholder / policy-blocked
- 用户界面不暗示能力已开放

## 8. Edge Cases To Plan For

1. 相同 external id 但来源状态已更新
2. 来源对象缺少标题或关键摘要字段
3. 绑定 issue 已被关闭/删除/迁移
4. 入站处理成功但绑定失败
5. 处理重试后状态变化
6. 原始 payload 不可展示或需要脱敏
7. 来源链接不可访问或为空
8. 同一 issue 未来需要多绑定，但首版模型先一对一

## 9. Suggested Test Layers By Repo Area

### Backend

优先验证：

- workspace isolation
- idempotency
- external record lifecycle
- binding invariants
- attempt/failure summaries

建议位置：

- `server/internal/...` 对应服务测试
- 必要时数据库集成测试

### packages/core

优先验证：

- API schema parsing
- malformed response fallback
- workspace-scoped query keys
- shared types and adapters

### packages/views

优先验证：

- issue external summary block
- operator list/detail presentation
- loading / empty / failed states
- no platform-specific routing imports

### E2E

优先验证黄金路径，不先追求大而全：

1. 来源对象进入系统
2. 用户在 issue 中看到来源摘要
3. 用户看到处理结果或失败摘要
4. Agent 协作链路可读取该上下文

## 10. Evidence Required Before Declaring MVP Done

后续若进入实现，至少要收集以下证据：

- 一个真实或脱敏真实样本的完整黄金路径截图/录屏
- 一个重复入站幂等场景结果
- 一个失败可见场景结果
- 一个 workspace 隔离验证结果
- 一个 Agent 读取上下文但未触碰外部凭据的证据
- 若进入写回试点，再追加 effect/idempotency 证据

## 11. Non-Goals For Testing

本轮不需要：

- 测试任意第三方 connector 兼容性
- 测试 visual builder
- 测试复杂审批恢复
- 测试多组织 agent delegation
- 测试 Temporal/Cedar/Nango 之类平台集成

## 12. Exit Criteria For Planning Stage

在开始写代码前，应确认：

- [ ] MVP spec 已经稳定到可以拆解为实现任务
- [ ] 每个核心功能都有至少一个 Given/When/Then 验收场景
- [ ] 非目标范围被清楚写出，避免实施期扩张
- [ ] 测试重点与产品命题一致，而不是平台化幻想
- [ ] 用户/决策人认可该测试口径

## 13. Recommended Next Step

下一步不应直接实施，而应基于本测试计划继续补一版更具体的实现蓝图：

- 建议新增哪些表
- 建议新增哪些后端包与 API
- 建议新增哪些 `packages/core` types/queries
- 建议新增哪些 `packages/views` 页面或区块
- 每一项对应哪些测试用例

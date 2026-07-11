# MultiOps Workbench 研究档案

> 状态：方案研究 / 脑暴阶段。本文档集不是实施计划，也不代表其中能力已经存在。
>
> 基准日期：2026-07-10

## 研究目的

在修改 Multica 业务代码前，先回答三个问题：

1. 已跑通的 Ferry → Multica MVP 实际证明了什么，尚未证明什么？
2. Multica 哪些能力可以作为 Workbench 骨架复用，哪些能力必须建立独立边界？
3. 是否已有可靠协议、框架或脚手架可复用，哪些依赖会引入许可证、运维或产品锁定风险？

## 文档导航

- [01-method-and-evidence.md](01-method-and-evidence.md)：研究方法、证据分级、假设和决策流程
- [02-ferry-mvp-evidence.md](02-ferry-mvp-evidence.md)：Ferry MVP 实证链路、安全问题和不可泛化假设
- [03-multica-reuse-boundaries.md](03-multica-reuse-boundaries.md)：Multica 能力复用矩阵、缺失原语和上游合并边界
- [04-framework-landscape.md](04-framework-landscape.md)：外部框架、协议、许可证和复用建议
- [05-product-and-architecture-options.md](05-product-and-architecture-options.md)：候选产品原型、候选架构、反对意见和待验证问题
- [06-decision-record-template.md](06-decision-record-template.md)：后续方案决策模板
- [07-mvp-spec-v1.md](07-mvp-spec-v1.md)：第一版 MVP 规格草案，明确范围、目标、边界和验收口径
- [08-mvp-test-plan-v1.md](08-mvp-test-plan-v1.md)：第一版 MVP 测试计划，定义验收场景、覆盖层次和证据要求

## 当前结论摘要

### 已确认事实

- Ferry MVP 已实际跑通：定时拉取工单、分类、创建并指派 Multica Issue、Agent 排障或审批、企业微信通知。
- Multica 已具备 workspace-scoped Issue、评论、Agent、Runtime、Daemon、Autopilot、Skills、WebSocket 和共享 Web/Desktop 视图等骨架。
- 当前仓库没有完整的外部对象绑定、Connector 实例、入站事件账本、可靠回写 Outbox、审批快照、策略决策和外部副作用台账。
- Ferry MVP 是外部脚本编排，不是通用 Connector Framework；审批路径已有真实写入副作用。

### 推荐方向（仍需验证）

- 保持 Multica Issue 为人和 Agent 的主要协作对象，但不要把外部系统字段直接塞进 Issue 核心表。
- 在 Multica 核心域之外建立 Integration、Delivery 和 Approval 边界，通过稳定服务接口与 Issue/Agent 域交互。
- 首个研究原型应围绕一个真实 Ferry 闭环验证领域模型，不要先建设通用 REST DSL、Connector SDK、可视化 DAG 或 A2A 网络。
- 采用标准契约和可靠性模式优先于引入大型平台：OpenAPI、CloudEvents、AsyncAPI、MCP、Outbox/Inbox、幂等和副作用台账。
- Temporal、Cedar、Nango 等只能在明确达到复杂度阈值后评估；它们不是默认答案。

## 事实标签

后续文档统一使用：

- **FACT**：代码、配置或实际运行记录直接支持
- **EVIDENCE**：Ferry MVP 样本或可复现实验支持
- **REFERENCE**：官方规范、主仓库或外部产品提供的参考
- **INFERENCE**：基于事实作出的架构推断
- **HYPOTHESIS**：需要用户、数据或原型验证的假设
- **DECISION**：经明确评审批准的选择；当前文档集中暂无正式决策

## 本阶段明确不做

- 不修改 Multica 或 Ferry 业务代码
- 不启动、停止或修改 systemd 服务
- 不读取 Ferry `.env`、凭据、归档或原始生产 Payload
- 不选定最终产品原型
- 不把研究推荐直接转换成实施任务
- 不因某框架功能列表丰富就默认引入

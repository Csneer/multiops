# 研究方法与证据治理

## 1. 从 VibeOps-SOP 复用的阶段门

`/home/devops/projects/vibeops-sop` 提供了一套证据驱动、先研究后实施的流程：

1. 需求澄清
2. 资料取证
3. MVP 切分
4. 技术方案与测试计划
5. 用户批准后分阶段实施
6. 独立审查与验收
7. 上线检查、部署验证和运维交接

可复用材料：

- `templates/requirement.md`：背景、用户、输入、输出、成功标准、非目标、风险
- `templates/evidence-research.md`：官方资料、项目模式、不确定项、禁止假设
- `templates/mvp-scope.md`：本次做/不做、黄金路径、最小失败路径、验收条件
- `templates/implementation-plan.md`：文件、数据流、错误、日志、测试、部署和风险
- `templates/release-checklist.md`：版本、负责人、资源、回滚
- `templates/ops-runbook.md`：启动、停止、状态、日志、配置、排障和恢复

## 2. VibeOps-SOP 对本项目仍不够的部分

MultiOps Workbench 还需要补充：

- 至少 3 个候选产品原型，而不是从初步构想直接收敛到单一方案
- 被否决方案和否决理由
- 证据质量、适用版本、失效条件和复现实验
- 假设的置信度、影响、Owner、验证方法和关闭标准
- 决策台账及复审触发条件
- 用户价值、行为指标和运行反馈，而不仅是工程验收
- 方案被证伪后回退到前一阶段的机制

## 3. 建议研究流程

### Gate A：问题框定

产物：Problem Brief。

必须回答：

- 首个核心用户是谁：值班 SRE、应用运维、审批人、平台管理员还是研发？
- 当前链路中最昂贵的问题是什么：信息搬运、排障、审批、回写还是审计？
- 用户愿意把哪些动作交给 Agent？
- 哪些动作不可逆或必须人工批准？
- 现有 Ferry MVP 中哪一步产生了真实收益？

出口条件：问题和成功指标可被观测；明确非目标。

### Gate B：证据包

每条材料至少记录：

| 字段 | 含义 |
| --- | --- |
| `claim` | 要支持或反驳的陈述 |
| `kind` | FACT / EVIDENCE / REFERENCE / INFERENCE / HYPOTHESIS |
| `source` | 文件、官方文档或实验 |
| `observed_at` | 观察日期，格式 `YYYY-MM-DD` |
| `scope` | 适用系统、版本、环境和用户 |
| `confidence` | high / medium / low |
| `invalidated_by` | 什么情况会使结论失效 |
| `reproduction` | 可选复现步骤；禁止放凭据 |

出口条件：关键 API、权限、状态和副作用没有依赖记忆或猜测。

### Gate C：候选方案发散

至少提出三个方向，并为每个方向写最强支持意见和最强反对意见。

比较维度：

- 用户价值与验证速度
- 对 Multica 上游合并的影响
- 数据模型侵入程度
- 外部副作用安全性
- 连接器开发成本
- 可靠性和可观测性
- 部署与运维成本
- 许可证和供应商锁定
- 可回滚性

### Gate D：假设验证

每个高影响假设都要有验证卡：

```yaml
hypothesis: Issue 可以继续作为所有运维任务的主要协作对象
confidence: medium
impact_if_false: high
validation:
  - 复盘 Ferry 样本的所有状态和参与者
  - 对比事件、请求、审批、执行四类对象的生命周期
  - 用三个非 Ferry 场景做建模演练
close_when: 三类来源均无需扭曲 Issue 状态机即可表达
owner: TBD
```

### Gate E：决策

只有满足以下条件，材料才可升级为 DECISION：

- 事实和假设已分开
- 至少两个可行替代方案被认真评估
- 反对意见有书面回应
- 风险、复审条件和撤销成本明确
- 决策人明确批准

### Gate F：研究 MVP，而非产品 MVP

第一轮实现前，先用 Schema、Payload、状态转换表或可丢弃原型验证：

- 幂等和重放
- 外部对象与 Issue 的关系
- 审批快照与执行参数绑定
- 写回失败后的操作体验
- 多 workspace 凭据隔离
- Agent 工具权限和副作用审计

## 4. 独立评审问题

每轮方案评审至少问：

1. 是否把一个 Ferry 特例包装成了通用平台需求？
2. 是否因已有 Multica Issue 而强行把所有对象都建模成 Issue？
3. 是否在没有第二个连接器前就设计了 Connector SDK？
4. 是否把“Agent 可以调用”误当成“Agent 被授权执行”？
5. 是否把重试当成幂等？
6. 是否把 workflow 成功当成外部副作用 exactly-once？
7. 是否能从入站 Payload 一直追踪到 Issue、Run、Approval 和 Writeback？
8. 如果继续合并 Multica 上游，哪些文件会持续冲突？
9. 若框架停更或许可证变化，核心产品是否仍可运行？
10. 哪些复杂度可以延后，直到真实使用证明需要？

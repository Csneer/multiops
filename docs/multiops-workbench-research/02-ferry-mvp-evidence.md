# Ferry → Multica MVP 实证复盘

## 1. 已跑通链路

**FACT / EVIDENCE**

```text
Ferry 待办工单
  → systemd 常驻脚本每 5 分钟增量拉取
  → 获取 process-structure 和表单详情
  → 白名单审批 / 关键词排障分类
  → 创建并指派 Multica Issue
  → Agent 只读排障或调用 Ferry 审批脚本
  → Issue 进入 blocked / in_review / done
  → 每分钟扫描并发送企业微信通知
```

主要组件：

- `/home/devops/projects/ferry_ops/ferry_multica_ingest.py`
- `/home/devops/projects/ferry_ops/ferry_approve_work_order.py`
- `/home/devops/projects/ferry_ops/multica_issue_notify.py`
- `/home/devops/projects/ferry_ops/ferry-multica-ingest.service`
- `/home/devops/projects/ferry_ops/multica-issue-notify.service`
- `/home/devops/projects/ferry_ops/multica-issue-notify.timer`
- `/home/devops/projects/ferry_ops/README.md`
- `/home/devops/projects/ferry_ops/HANDOFF.md`

本次研究未读取 `.env`、归档、凭据、Token、私钥或原始生产 Payload。

## 2. 关键机制

### 增量与去重

- 使用 `(create_time, id)` 复合 watermark
- 下次查询向前 overlap 5 分钟
- SQLite `tickets.ferry_id` 作为本地去重键
- 每条工单处理后落库，批次结束后推进 watermark

可复用点：复合游标、overlap、逐条提交和本地幂等记录。

限制：

- Multica Issue 创建成功、SQLite 记录前崩溃会重复建单
- 只按创建时间拉取，无法感知旧工单后续更新
- `insert or replace` 不能保留完整事件历史
- ignored 工单不会因规则更新而重新评估

### 分类与路由

分类优先级是白名单审批优先于关键词排障。

当前审批白名单仅有两个流程：

- `OSS文件上传 + 运维`
- `校验文件上传 + 运维`

排障路径依赖 Jenkins、发布、Kubernetes、Pod、Ingress、失败等关键词。

可复用点：低风险 allowlist 应优先于模糊分类；信息不足时进入 `blocked` 而不是猜测。

不可泛化：流程名、节点名、中文审批边和关键词均为本地 Ferry 约定。

### 审批

审批脚本在写操作前重新拉取详情，并验证：

- 流程名
- 当前节点
- 当前节点唯一可执行的“同意/通过/确认”边

随后调用 Ferry `handle` 接口，并使用 `is_exec_task=true` 尝试触发节点任务。

重要事实：审批不是建议性流程，而是真实外部写操作。

重要缺口：代码没有强制校验 bucket、路径、附件内容、申请人或业务字段；“已确认工单信息”主要依赖 Agent 指令，不是代码级授权策略。

### 通知

通知器扫描 `blocked`、`in_review` 和 `done`，成功发送后记录 `(issue_id, status, updated_at)`。

限制：

- 不覆盖创建、执行中、失败和重试状态
- 评论摘要可能将敏感排障信息带出 Multica
- 同一状态下更新时间变化会重复通知

## 3. 实际样本证明了什么

文档记录样本：

- `#5631`：进入排障链路，因缺少 Jenkins Job/Build URL/日志而进入 `blocked`
- `#5642`：手工调用 Ferry handle 返回处理完成
- `#5643`：Approval Agent 审批成功；早期 `is_exec_task=false` 可能未触发后台任务
- `#5644`：人工审批对照样本

这些样本证明：

- Ferry 数据可被转换成 Agent 可消费的 Multica Issue
- Agent 能在 Issue 生命周期中报告阻塞和完成
- 明确白名单下可以执行真实审批
- 企业微信可以承接结果通知

它们没有证明：

- 任意工单系统都能使用相同字段和状态模型
- Ferry 后台任务一定执行完成
- 外部写操作具备 exactly-once
- Issue 状态足以表达来源、执行、审批和同步的全部状态
- 当前安全边界足以支持更多生产写操作
- 用户真正需要通用 Connector SDK 或可视化自动化平台

## 4. 安全与运维发现

### 高优先级

1. `.env` 权限元数据显示为 `0664`，预期含敏感凭据，应收紧到 `0600`；本次未读取内容。
2. 附件下载接受表单任意 HTTP(S) URL，缺少 host allowlist、私网地址阻断和重定向控制，存在 SSRF 风险。
3. Agent 约定承担了部分授权职责；审批允许性必须由服务端策略和不可变快照决定。
4. 表单、URL、附件路径、流程任务和评论可能扩大敏感数据传播面。

### 中优先级

- 生产 Ferry 地址硬编码在源码
- README 仍称默认 dry-run，但 systemd 实际使用 `--no-dry-run --approval-mode agent`
- README 所述只读接口与实际审批写接口不一致
- 未能独立确认 2026-07-10 systemd 服务实时运行状态

## 5. 应保留与应淘汰

### 保留思想

- dry-run 与 active 模式分离
- 写前重读并校验当前状态
- allowlist 优先
- blocked 是正常结果
- 成功后再写通知幂等记录
- 把流程节点、边和 task 上下文交给 Agent
- 复合 watermark + overlap

### 不应直接复制

- 标题正则作为 Issue 身份协议
- 本地 SQLite 作为跨系统最终账本
- 关键词分类作为通用路由系统
- Agent Prompt 作为审批授权边界
- 任意 URL 附件下载
- 把所有外部字段拼进 Issue 描述
- 将 Ferry 状态、Agent 状态和 Workbench 状态混为一个 Issue status

## 6. 对产品方向的启示

**INFERENCE**

首个产品验证不应是“能否接入 Ferry”，因为该链路已经证明。下一步应验证：

1. 外部对象绑定和生命周期是否需要独立于 Issue
2. 写回与审批失败时，用户能否理解、重试和审计
3. 权限和策略能否在 Agent 之外阻止未授权副作用
4. 同一 Issue 绑定多个外部对象是否有真实需求
5. 第二个非 Ferry 来源是否能在不改核心域的前提下接入

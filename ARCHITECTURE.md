# Architecture

## Mission

Proactive Interaction Engine 只负责：观察事实、编译语义事件、维护当前状态、判断是否值得主动、生成抽象行为计划、评估用户反馈。

摄像头、麦克风、ROS、厂商 SDK、硬件控制、模型运行时、数据库驱动和操作系统 API 都属于外部适配器。

## Hard Boundaries

1. `internal/domain` 不依赖平台、传输、存储、模型框架或生成代码。
2. 原始音视频、帧、PCM、Tensor 不进入核心；输入适配器只提交规范化 Observation。
3. Event、Command、Query 使用独立强类型，禁止万能 JSON、`Any` 或全局消息总线。
4. 大模型不能决定实体行为、修改安全规则或直接写敏感记忆；所有输出经过结构化校验和确定性策略。
5. `SemanticEvent -> WorldState -> Decision -> BehaviorPlan -> first ActionCommand` 在本地内存中完成，不依赖云端或数据库。
6. 引擎只下发高层语义动作。碰撞、限位、急停、刹车和实时避障由底层控制器负责。
7. `WorldState` 只允许单写者修改，策略只读取不可变快照。
8. 跨进程使用 Protobuf/gRPC；进程内使用 Go 领域类型；边界显式映射。
9. 每个动作包含 ID、交互 ID、截止时间、抢占策略、所需能力和幂等语义。
10. 不支持的能力永远不能出现在 BehaviorPlan 或 ActionCommand 中。

## Dependency Direction

```text
internal/domain
      ^
internal/application
      ^
adapters + internal/runtime
      ^
cmd
```

禁止：

```text
domain -> adapters/contracts/gen/runtime
application -> concrete adapters/ROS/SQLite/vendor SDK
model worker -> embodiment adapter
```

依赖边界由 `tests/architecture` 检查。

## Core Flow

```text
Observation
  -> ingress validation / TTL / deduplication
  -> temporal Event Compiler
  -> single-writer World State Projector
  -> Opportunity Detector
  -> deterministic Hard Guard
  -> pure Policy
  -> Decision Validator
  -> capability-aware Behavior Planner
  -> ActionDriver port
  -> ActionStatus / Episode / Outcome
```

`SILENT` 是一等 Decision，必须记录原因，不生成 BehaviorPlan。

## Domain Language

- Observation：感知源的即时、可能有噪声的信号。
- SemanticEvent：经时序规则确认且不可撤销的事实。
- Opportunity：可能值得互动的机会，不是最终决定。
- Decision：确定性守卫与策略产生的意图，包含 `SILENT`。
- BehaviorPlan：载体无关的行为树，只含 Sequence、Parallel、Race、Action、WaitEvent、Condition。
- ActionCommand：输出适配器可接受、拒绝、取消或超时的命令。
- Outcome：一次 InteractionEpisode 的用户反馈结果。

## Runtime and Degradation

控制/拒绝为 P0，取消和动作状态为 P1，语义事件与决策为 P2，日志、记忆和遥测为 P3。所有队列有界；连续状态只保留最新值；控制命令和动作终态不可丢弃。

当前 `internal/runtime/lifecycle.Runner` 使用容量独立的 Observation 与 Control 队列，同时最多执行一个 Observation，以保持 `WorldState` 单写者。显式 `USER_REJECTED` 分两阶段执行：P0 阶段先取消当前处理上下文并立即通过 `ActionDriver.StopAll` 请求载体停止可中断动作；Runner 等活动处理 Goroutine 退出后，再串行提交拒绝事实、终止匹配的 InteractionEpisode、记录 `REJECTED` Outcome 并进入当前用户的冷却期。`StopAll` 失败不丢弃拒绝事实，`SHUTDOWN` 不生成拒绝。该软件取消链路不替代物理急停。

显式拒绝冷却默认 30 分钟、仅作用于当前用户，并保持配置化。硬守卫按事件时间在半开区间 `[rejected_at, cooldown_until)` 内返回 `SILENT/RECENT_REJECTION`，截止时刻恢复。`WorldState` 与 Episode Tracker 只允许在上述串行提交阶段修改。

模型调用必须有 deadline、cancel、max concurrency、budget、circuit breaker 和本地 fallback。数据库失败进入无持久化模式；模型失败使用本地模板；载体断开取消当前计划；用户拒绝立即取消可中断行为。

## Reproducibility

每个 Decision 记录 `policy_version`、`behavior_version`、`config_hash`、`snapshot_hash`、`model_version`、`random_seed` 和 `trace_id`。相同语义事件日志、配置、版本和随机种子必须得到相同 Decision。

第一阶段使用“内存状态 + 语义审计日志 + 周期快照”，不采用完整 Event Sourcing，也不默认保存原始音视频。

## Initial Scope

首个垂直闭环：

```text
用户离开 -> 用户稳定返回 -> 检查 busy / quiet
-> GREET_SHORT 或 SILENT
-> 生成能力兼容的 BehaviorPlan
-> Fake ActionDriver 执行并记录状态
```

已完成的反馈子闭环为“显式拒绝 -> P0 停止 -> `REJECTED` Outcome -> 冷却硬守卫”。`WaitEvent` 执行、用户正常回应和 `NO_RESPONSE` 超时 Outcome 尚未完成，后续必须与确定性回放测试一起交付，不能把行为树中已有的 `WaitEvent` 节点视为运行时已支持。

暂不引入 Kafka、Kubernetes、微服务拆分、动态插件、万能事件总线、完整 Event Sourcing、工作流平台、向量数据库实时依赖、LLM 总控制器或 ROS 领域类型。

## Change Policy

新增平台只能增加 adapter、composition root 和跨进程 mapper，不应修改 `domain`、`decision` 或抽象 `behavior`。长期边界变更必须新增 ADR，并同步更新本文件、架构测试和仓库 Skill。

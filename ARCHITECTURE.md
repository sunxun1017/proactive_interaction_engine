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
11. 场景只组合已注册、健康、获授权且协议兼容的强类型能力；能力缺失和冲突按显式 fallback 降级。
12. 生物识别 Worker 只输出候选，身份必须经 Identity Resolver；原始媒体、embedding 和模板不得进入核心或语义审计。

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

`UserReply` 是可信 PC 输入边界在应用层响应窗口开放时构造的规范化 Observation。首版完全免按键：`[opened_at, deadline)` 内检测到任意人声即视为用户回应；窗口外人声不能生成 `UserReply`。响应窗口是唯一对话指向信号，Ingress 忽略已弃用且不可信的 `addressing_agent` 并把接受的人声置信度规范化为 1；VAD worker 不读取行为树、Episode、Outcome 或 Wakeup token。核心不接收原始 PCM、转写文本、回复内容、VAD 分数或识别启发式，只把该 Observation 编译为 `USER_REPLIED` 事实；用户回复不是 ControlCommand。

跨进程 Worker 发布 Observation 时必须携带 Registry 服务端签发的 lease。Ingress 使用服务端注入时钟验证 lease 存在、Provider 健康且 `now < lease_expires_at`，并核对 `source_id` 对应注册 instance、Provider 已声明该强类型能力，且当前场景为该能力显式选择了同一 Provider。首版 gRPC Ingress 只接收 `PersonPresence`、`UserBusy` 和 `SpeechActivity`；直接发布 `UserReply`、`UserControl`、`QuietMode` 或 `DeviceCondition` 均 fail closed。Handler 只能经 `Runner.SubmitObservation` 进入单写者链路，禁止直接调用 `Engine.Process`。

## Runtime and Degradation

控制/拒绝为 P0，取消和动作状态为 P1，语义事件与决策为 P2，日志、记忆和遥测为 P3。所有队列有界；连续状态只保留最新值；控制命令和动作终态不可丢弃。

当前 `internal/runtime/lifecycle.Runner` 使用容量独立的 Observation 与 Control 队列，同时最多执行一个 Observation 或 Wakeup work，以保持 `WorldState` 单写者。显式 `USER_REJECTED` 分两阶段执行：P0 阶段先取消当前处理上下文并立即通过 `ActionDriver.StopAll` 请求载体停止可中断动作；Runner 等活动处理 Goroutine 退出后，再串行提交拒绝事实、终止匹配的 InteractionEpisode、记录 `REJECTED` Outcome 并进入当前用户的冷却期。`StopAll` 失败不丢弃拒绝事实，`SHUTDOWN` 不生成拒绝。该软件取消链路不替代物理急停。

显式拒绝冷却默认 30 分钟、仅作用于当前用户，并保持配置化。硬守卫按事件时间在半开区间 `[rejected_at, cooldown_until)` 内返回 `SILENT/RECENT_REJECTION`，截止时刻恢复。`WorldState` 与 Episode Tracker 只允许在上述串行提交阶段修改。

Application Engine 持有当前专用 continuation，并只向 Runner 暴露不透明的 `Wakeup{Token, Deadline}`。Runner 同时最多维护一个 timer 和一个 active work；active work 可以是 Observation 或到期推进，但 Runner 不解释 `WaitEvent`、用户回复、Episode 或 Outcome。空闲调度严格按 P0 Control、已排队 Observation、到期 Wakeup 的顺序检查；P0 对任意 active work 都执行 cancel、立即 `StopAll`、join，之后才提交拒绝。每次 work 或控制提交后 Runner 重新读取 Wakeup 并停止旧 timer，active work 期间不并发执行 timer work。

当前欢迎行为计划中的 `WaitEvent(user.reply, 8s)` 决定响应窗口时长；8 秒来自行为计划，不是 Engine 全局配置。窗口采用 `[opened_at, deadline)`：截止时刻及之后的精确 Wakeup 生成 occurred_at 等于 deadline 的 `RESPONSE_WINDOW_EXPIRED`，结束 Episode 为 `NO_RESPONSE/RESPONSE_WINDOW_ELAPSED`，进入当前用户默认 5 分钟且可配置的独立冷却，再按实际下发时钟执行 `RETURN_IDLE`。显式拒绝优先于 Observation 和到期推进。到期 Event、Outcome、冷却与 pending 清理先于审计和后续动作；审计失败仅降级为 warning，`RETURN_IDLE` 失败也不回滚已提交事实。

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

已完成三个反馈子闭环：

- 显式拒绝 -> P0 停止 -> `REJECTED` Outcome -> 冷却硬守卫；
- 适配器确认的 `UserReply` -> `USER_REPLIED` -> `ACCEPTED` Outcome -> 执行 `user.reply` 等待点后的动作；
- 响应窗口到期 -> `RESPONSE_WINDOW_EXPIRED` -> `NO_RESPONSE` Outcome -> 独立冷却 -> `RETURN_IDLE`。

当前只支持根 Sequence 中唯一的 `WaitEvent(user.reply)` 专用 continuation，由 Application Engine 持有；Runner 仅调度不透明 Wakeup，不解释行为树或 Episode 语义。回复后动作的 deadline 在实际下发时生成。通用 `WaitEvent`、行为树 cursor 和工作流执行器仍未实现。

Stage B 无硬件产品雏形已可通过 Fake Embodiment、Fake Clock、内存审计和五条模拟场景执行。Stage C1 的强类型能力契约、Provider Registry、场景 manifest 与校验已完成；Stage C2 的基础 PC 体验也已完成：loopback Web Avatar/TTS/控制面板，私有 UDS 上的 Camera/VAD Provider，以及 Registry、Ingress、Runner 和 Engine 的纵向链路。Stage C3 生物身份仍未实现。

Stage C 已扩展为能力平台。Provider 声明稳定 ID、协议/实现版本、强类型能力、健康、隐私等级、延迟和取消语义；版本化场景声明 required、optional、选定 Provider、最低身份保证和确定性 fallback。首版采用显式部署配置，不实现任意动态插件或运行中热卸载。

摄像头、麦克风及每项生物识别能力必须分别授权并持续显示状态。生物识别默认关闭；face detection 和 liveness 只需功能授权，face identification、speaker identification 与 speaker verification 还需逐用户注册。本地提取并加密保存模板，原始注册媒体提取后立即丢弃。VAD 和 ASR 仍是独立能力。Worker 只输出身份候选，Identity Resolver 负责阈值、融合和冲突；不确定、多人歧义或人脸/声纹冲突时回退匿名，不能加载私有记忆，也不能将生物识别用于安全认证。

原始帧、PCM、裁剪、embedding、声纹向量、Tensor 和生物模板只存在于受控 Worker 或专用加密存储，不进入 Engine、语义审计或通用契约。控制界面只监听 loopback，首个运行平台为 Ubuntu Linux，本地语音输出使用 Speech Dispatcher。摄像头、麦克风、身份、UI 或 TTS 失效时必须独立降级，不能阻塞 P0 拒绝和核心静默路径。详细产品定义见 `docs/PRODUCT_REQUIREMENTS.md`，边界决策见 ADR 0002。

Stage C2 的 Python media worker 仅连接 composition 创建的随机私有 UDS：运行目录 `0700`、socket `0600`，不开放 TCP。CameraCapture 与 MicrophoneCapture 默认关闭并分别驱动子进程；许可表示期望状态，只有健康且未过期的 Registry lease 才表示 Provider 正在运行。Camera 使用显式设备、640×480/约 5 FPS、HOG/upper-body 匿名人体检测和进入/退出滞回，不得在 CameraCapture 授权下加载人脸模型。VAD 使用 16 kHz mono s16le、20 ms 帧、300 ms 稳定语音判定和 500 ms 静音重武装。worker 所有者负责 SIGTERM、超时 SIGKILL 与 join，不自动无限重启。

暂不引入 Kafka、Kubernetes、微服务拆分、任意动态插件、万能事件总线、完整 Event Sourcing、工作流平台、向量数据库实时依赖、LLM 总控制器或 ROS 领域类型。

## Change Policy

新增平台只能增加 adapter、composition root 和跨进程 mapper，不应修改 `domain`、`decision` 或抽象 `behavior`。长期边界变更必须新增 ADR，并同步更新本文件、架构测试和仓库 Skill。

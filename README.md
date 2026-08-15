# Proactive Interaction Engine

一个可执行的主动式陪伴智能体平台。项目采用“模块化单体核心 + 端口适配器 + 契约优先 + 重模型进程隔离”，既可用确定性模拟验证“用户离开后返回”的主动交互，也已提供 Ubuntu PC 上的本地 Web Avatar、免按键 VAD 和匿名在场检测雏形。

## 当前能力

- 强类型 `Observation -> SemanticEvent -> WorldSnapshot` 链路
- 返回欢迎机会发现、忙碌/安静模式硬守卫、确定性规则策略
- 与载体无关的 `BehaviorPlan` 和能力过滤
- Fake Embodiment、Fake Clock、内存审计记录器
- 单写者 Runner、有界 Observation/Control 队列和 P0 `StopAll` 抢占
- 显式用户拒绝的 `REJECTED` Outcome、30 分钟单用户冷却与确定性硬守卫
- 可信 Ingress 在响应窗口内构造的 `UserReply`、`ACCEPTED` Outcome 与回复后行为续接
- 响应窗口到期后的 `NO_RESPONSE` Outcome、5 分钟单用户冷却与自动 `RETURN_IDLE`
- 欢迎、保持静默、用户回复、显式拒绝和无响应五条模拟场景
- 强类型能力契约、Provider Registry、严格场景 manifest 与显式 Provider 选择
- 严格的 scenario schema v2 与 Provider operational profile 兼容校验；跨进程 Protobuf 和 Provider protocol 仍为 v1
- 带 Provider lease 校验的 gRPC Ingress fake-media 纵向闭环
- 私有 UDS 上的真实 Camera/VAD Provider、15 秒租约与独立降级
- 默认关闭且可分别撤销的 Camera/Microphone 权限、持续 Provider 状态
- loopback typed Web 控制面板、Web Avatar 与本地 Speech Dispatcher TTS
- Stage C3 model-independent checkpoint：加密生物 catalog/vault 与删除、Identity Resolver/coordinator、五个独立 evidence RPC，以及 desktop 同步 runtime/live readiness seam
- Registry、identity ingress、resolver/runtime 和加密删除的 fake/in-memory 纵向验证
- Protobuf 外部契约、行为与配置样例、架构依赖测试
- 仓库级 Agent Skill 与 Git/CI 约束

## 快速开始

需要 Go 1.23 或使用 Docker：

```bash
make test
go run ./cmd/simulator
go run ./cmd/simulator -busy
go run ./cmd/simulator -reply
go run ./cmd/simulator -reject
go run ./cmd/simulator -timeout
```

正常场景会生成 `GREET_SHORT` 与抽象动作；`-busy` 场景会生成带 `USER_ON_CALL` 原因的 `SILENT`，且不下发动作。

`-reply` 使用 Fake Clock 演示用户在响应窗口内回复后记录 `ACCEPTED`；`-reject` 演示 `StopAll`、`REJECTED` 与 30 分钟冷却（P0 抢占顺序由 Runner 集成测试验证）；`-timeout` 精确推进到响应截止时刻，记录 `NO_RESPONSE`、进入 5 分钟冷却并执行 `RETURN_IDLE`。

Stage B 的无硬件产品雏形和 Stage C2 的基础 PC 体验已完成。Camera worker 使用 HOG/upper-body 的匿名人体检测，不运行人脸识别；Microphone worker 使用本地 WebRTC VAD，响应窗口内任何稳定人声都可由可信 Ingress 转为回复。原始帧/PCM 不出 worker，不录制、不转写。

Stage C3 当前只达到 model-independent checkpoint：模型无关的身份契约、授权、加密存储、删除、解析、窗口协调、ingress 和 desktop composition seam 已由 fake/in-memory 测试验证。这不表示真实生物识别或 Stage C3 已完成：仓库尚无真实人脸/声纹/活体模型与注册采集、生产 master-key provider、Camera/Microphone 设备共享、UI enrollment/status，也未在生产 `cmd/desktop.Build` 中启用 identity。canonical subject identity、私有记忆读取和个性化欢迎属于后续 C4；核心仍只支持欢迎计划中专用的 `WaitEvent(user.reply)` continuation，不是通用工作流执行器。

相关增量验证：

```bash
go test ./adapters/capability/registry ./adapters/config/scenario ./adapters/input/ingress
go test -race ./adapters/input/ingress
go test ./internal/application/biometric ./internal/application/identity
go test ./adapters/storage/biometricvault ./adapters/identity/ingress ./cmd/desktop
go test ./tests/architecture
make media-env
make media-test
make web-build
go run ./cmd/desktop
```

`cmd/desktop` 只监听 loopback，worker 只连接随机私有 UDS。摄像头和麦克风首次均为关闭状态；在 Web 面板分别授权后才启动对应进程，撤权或退出会停止并 join 子进程。默认显式设备是 `/dev/video0`，音频使用 PulseAudio `pacat`；首版不会在多个摄像头间静默选择。`make media-env` 在仓库内创建被 Git 忽略的 Python 3.10 venv，不使用全局 pip。

## 目录

```text
cmd/                  进程入口与装配
internal/domain/      领域类型与纯规则
internal/application/ 用例编排和端口
internal/runtime/     时钟与运行期基础设施
adapters/             外部输入、载体、模型、存储适配器
workers/              隔离的本地 Camera/VAD Provider
contracts/            跨进程 Protobuf 契约
behaviors/            可审阅的行为定义
configs/              已验证配置样例
tests/                架构、场景、契约与回放测试
.agents/skills/       随仓库版本化的 Agent 约束
```

完整产品需求见 [docs/PRODUCT_REQUIREMENTS.md](docs/PRODUCT_REQUIREMENTS.md)，详细边界见 [ARCHITECTURE.md](ARCHITECTURE.md) 和 [ADR 0002](docs/adr/0002-compose-scenarios-from-capabilities.md)。

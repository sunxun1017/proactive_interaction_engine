# Proactive Interaction Engine

一个可执行的主动式陪伴智能体核心骨架。项目采用“模块化单体核心 + 端口适配器 + 契约优先 + 重模型进程隔离”，首版实现可在无摄像头、无机器人、无云端模型的环境中验证“用户离开后返回”的主动交互闭环。

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
- 带 Provider lease 校验的 gRPC Ingress fake-media 纵向闭环
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

Stage B 的无硬件产品雏形已完成：上述五条路径均可通过 Fake Embodiment、Fake Clock 和内存审计在本机执行。Stage C1 已完成强类型能力契约、Provider Registry 和首个版本化场景 manifest；Stage C2 已用 fake-media gRPC 测试连通 Registry、Ingress、Runner 与 Engine。真实摄像头/VAD worker、Web avatar、Speech Dispatcher TTS、控制界面和生物识别仍待实现，当前模拟器不伪装这些外部能力。核心目前仍只支持欢迎计划中专用的 `WaitEvent(user.reply)` continuation，不是通用工作流执行器。

相关增量验证：

```bash
go test ./adapters/capability/registry ./adapters/config/scenario ./adapters/input/ingress
go test -race ./adapters/input/ingress
go test ./tests/architecture
```

## 目录

```text
cmd/                  进程入口与装配
internal/domain/      领域类型与纯规则
internal/application/ 用例编排和端口
internal/runtime/     时钟与运行期基础设施
adapters/             外部输入、载体、模型、存储适配器
contracts/            跨进程 Protobuf 契约
behaviors/            可审阅的行为定义
configs/              已验证配置样例
tests/                架构、场景、契约与回放测试
.agents/skills/       随仓库版本化的 Agent 约束
```

完整产品需求见 [docs/PRODUCT_REQUIREMENTS.md](docs/PRODUCT_REQUIREMENTS.md)，详细边界见 [ARCHITECTURE.md](ARCHITECTURE.md) 和 [ADR 0002](docs/adr/0002-compose-scenarios-from-capabilities.md)。

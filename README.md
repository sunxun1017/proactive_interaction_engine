# Proactive Interaction Engine

一个可执行的主动式陪伴智能体核心骨架。项目采用“模块化单体核心 + 端口适配器 + 契约优先 + 重模型进程隔离”，首版实现可在无摄像头、无机器人、无云端模型的环境中验证“用户离开后返回”的主动交互闭环。

## 当前能力

- 强类型 `Observation -> SemanticEvent -> WorldSnapshot` 链路
- 返回欢迎机会发现、忙碌/安静模式硬守卫、确定性规则策略
- 与载体无关的 `BehaviorPlan` 和能力过滤
- Fake Embodiment、Fake Clock、内存审计记录器
- 单写者 Runner、有界 Observation/Control 队列和 P0 `StopAll` 抢占
- 显式用户拒绝的 `REJECTED` Outcome、30 分钟单用户冷却与确定性硬守卫
- 由输入适配器确认的 `UserReply`、`ACCEPTED` Outcome 与回复后行为续接
- 欢迎、用户回复与保持静默模拟场景
- Protobuf 外部契约、行为与配置样例、架构依赖测试
- 仓库级 Agent Skill 与 Git/CI 约束

## 快速开始

需要 Go 1.23 或使用 Docker：

```bash
make test
go run ./cmd/simulator
go run ./cmd/simulator -busy
go run ./cmd/simulator -reply
```

正常场景会生成 `GREET_SHORT` 与抽象动作；`-busy` 场景会生成带 `USER_ON_CALL` 原因的 `SILENT`，且不下发动作。

`-reply` 使用 Fake Clock 演示用户在响应窗口内回复后记录 `ACCEPTED` 并执行 `RETURN_IDLE`。当前仅支持专用 `WaitEvent(user.reply)` continuation；通用等待执行器、自动超时和 `NO_RESPONSE` 仍是下一阶段能力。

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

详细边界见 [ARCHITECTURE.md](ARCHITECTURE.md)。

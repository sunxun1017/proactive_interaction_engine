# Product Requirements

## Product

Proactive Interaction Platform 是一个可主动感知时机、发起轻量互动、评估用户反馈并逐步学习偏好的陪伴智能体平台。它不是等待按键、唤醒词或来电接通的被动助手。

平台的核心闭环是：

```text
感知用户与场景
-> 判断是否适合主动
-> 选择载体无关的行为
-> 自然等待用户回应
-> 形成 ACCEPTED / NO_RESPONSE / REJECTED
-> 在安全边界内调整后续主动策略
```

`SILENT` 是正确且可解释的产品结果。主动能力不能绕过用户许可、安静模式、忙碌状态、拒绝冷却或设备安全边界。

## Product Principles

- 免按键主动：智能体可以自己发起互动，用户无需先唤醒或接听。
- 场景组合能力：场景声明必需和可选能力，不把摄像头、模型或载体写死进核心。
- 本地优先：实时决策、停止、静默、模板欢迎和身份解析默认不依赖云端。
- 隐私可控：媒体采集、生物识别和长期学习分别授权、可见、可撤销、可删除。
- 确定性降级：能力缺失、超时或冲突时按场景定义回退，不猜测身份或伪造成功。
- 平台无关：相同语义事件在 PC 与机器人上产生相同决策，仅动作映射不同。

## Initial Users and Platforms

首个可运行产品面向 Ubuntu Linux x86_64 的本地 PC，使用摄像头、麦克风、扬声器和 loopback Web UI。后续通过 Adapter 接入其他操作系统、ROS2、厂商 SDK 和机器人载体。

系统支持三类主体：

- `ANONYMOUS`：未启用识别或无法可靠确认身份。
- `RECOGNIZED`：已注册用户被一个或多个生物特征能力可靠识别。
- `VERIFIED`：指定用户通过场景要求的验证方式确认。

首版生物识别只用于陪伴个性化，不用于门锁、支付、危险动作或其他安全认证。

## Capability Platform

### Perception Capabilities

- `PERSON_PRESENCE`
- `FACE_DETECTION`
- `FACE_IDENTIFICATION`
- `FACE_LIVENESS`
- `VOICE_ACTIVITY`
- `SPEAKER_IDENTIFICATION`
- `SPEAKER_VERIFICATION`
- `SPEECH_TRANSCRIPTION`
- `ATTENTION_ESTIMATION`
- `BUSY_STATE`
- `GESTURE_DETECTION`
- `DEVICE_STATE`

### Embodiment Capabilities

- `DISPLAY_TEXT`
- `AVATAR_ATTEND`
- `AVATAR_EXPRESSION`
- `SPEECH_SYNTHESIS`
- `GESTURE`
- `LIGHT`
- `LOCOMOTION`

每个能力 Provider 声明稳定 ID、协议版本、能力类型、实现版本、健康状态、隐私等级、延迟上限、取消语义及设备要求。场景只能选择已注册、协议兼容且健康的 Provider；未知 Provider 或重复主 Provider 必须在激活前失败。

首版使用显式配置选择已经部署的 Provider，不实现任意代码动态加载或运行中热卸载。

## Scenario Model

场景是版本化、可校验的声明，至少包含：

- 场景 ID 与版本。
- 必需能力。
- 可选能力。
- 每项能力的选定 Provider。
- 身份保证等级。
- 能力缺失、超时、冲突和设备故障的确定性降级。
- 行为、策略和隐私配置引用。

首批场景：

| Scene | Required | Optional | Fallback |
| --- | --- | --- | --- |
| 匿名返回欢迎 | presence、VAD、feedback output | face、speaker、TTS | 通用视觉欢迎 |
| 个性化返回欢迎 | presence、identity、feedback output | face、speaker、TTS | 匿名欢迎 |
| 隐私模式 | presence | visual output | 禁用生物识别 |
| 共享家庭 | presence、identity resolver | face、speaker | 冲突时匿名且不读私有记忆 |
| 自然对话 | VAD、ASR、feedback output | speaker | ASR 失败时只记录回应 |

场景降级不能放宽安全、权限或身份阈值。缺失必需能力且没有声明 fallback 时，场景不得激活。

## Proactive Welcome Flow

基础返回欢迎流程：

```text
用户离开达到阈值
-> 稳定返回
-> quiet / busy / cooldown guards
-> GREET_SHORT 或 SILENT
-> ATTEND / ACKNOWLEDGE / SPEAK
-> 8 秒响应窗口
-> ACCEPTED / NO_RESPONSE / REJECTED
-> RETURN_IDLE
```

- 程序启动时画面中已经有人只产生 presence，不立即伪造 return。
- 欢迎语播放完成后才打开响应窗口；首版不处理 TTS 期间插话。
- 响应窗口为 `[opened_at, deadline)`。
- 窗口内累计约 300 毫秒稳定人声即可成为回复候选，无需按钮、唤醒词或转写。
- 窗口外语音不能成为 `UserReply`。
- `NO_RESPONSE` 后不追加追问，默认进入 5 分钟冷却。
- 显式拒绝为 P0，立即停止可中断动作，默认进入 30 分钟冷却。

## Identity Platform

生物识别链路：

```text
camera / microphone
-> face / speaker worker
-> identity candidates
-> identity resolver
-> canonical subject identity observation
-> Runner
-> single-writer Engine
```

Worker 可以输出档案引用、置信度、活体结果、模型版本和事件时间，但原始帧、PCM、人脸裁剪、声纹向量、embedding 与 tensor 不得进入 Engine、语义审计或通用消息契约。

Identity Resolver 负责阈值、候选融合、多人场景和冲突处理。身份不确定、多人无法选定目标或人脸与声纹冲突时必须回退 `ANONYMOUS`；不得选择分数较高的一方强行确认。匿名状态不能加载任何人的私有记忆。

人脸识别、声纹识别和声纹验证是独立能力。声纹识别从多个档案中选择候选，声纹验证只验证指定主体，不能用一个布尔结果混淆二者。

## Enrollment and Privacy

生物识别默认关闭。每位用户必须分别同意创建人脸或声纹档案，并能独立停用和删除。

注册流程：

1. 展示用途、处理位置、保留内容和删除方式。
2. 用户显式授权采集。
3. 本地提取生物特征模板。
4. 立即丢弃原始注册音视频。
5. 加密保存模板、模型版本和授权记录。
6. 支持重新注册、撤销和彻底删除。

运行时必须持续显示摄像头、麦克风和生物识别状态。原始媒体只在 Worker 的有界内存中短暂处理，不录制、不转写、不进入审计或持久化。Web UI 首版只绑定 loopback。

## Learning and Memory

平台按设备、匿名会话和已确认主体隔离学习数据。只有达到场景要求的身份等级，才能读取该主体的个性化偏好。

首批学习信号：

- `ACCEPTED`：正向参与反馈。
- `NO_RESPONSE`：弱负反馈，不能等同拒绝。
- `REJECTED`：强负反馈。
- 场景、时间段、输出方式和非敏感设备上下文。

学习可以调整有界的主动频率、冷却、语音/视觉表达和模板选择，但不能绕过 hard guards、启用传感器、降低身份阈值、授权实体动作或直接保存敏感记忆。策略变化必须版本化、可解释、可回放、可关闭和可重置。

默认只持久化语义事件、决策、动作状态、Outcome、配置与模型版本。互动明细保留 90 天；聚合偏好保留到用户重置。生物模板与语义审计隔离存储。

## Degradation

- Camera 故障：停止视觉主动触发，不伪造 presence。
- VAD/Microphone 故障：当前互动自然超时并显示 degraded。
- Face/Speaker 故障：回退匿名，不加载私有记忆。
- 身份冲突：回退匿名并记录非敏感原因。
- TTS 故障：保留文字和视觉反馈。
- UI 断开：不阻塞 Engine、TTS 或 P0。
- Audit/Storage 故障：实时闭环继续，暂停学习。
- Network 故障：本地欢迎、静默、拒绝和停止继续工作。

创建 goroutine、设备句柄或子进程的组件必须负责 cancel、stop 和 join。

## Delivery Stages

### Stage C1: Capability Contracts

强类型能力声明、Provider 注册与健康、场景 required/optional/fallback 校验、Fake Provider 与 conformance tests。

### Stage C2: Base PC Experience

Person Presence、免按键 VAD、Web Avatar、本地 Speech Dispatcher TTS、隐私与控制面板。

### Stage C3: Biometric Identity

用户注册与删除、人脸识别、声纹识别/验证、Identity Resolver、加密模板存储和匿名降级。

### Stage C4: Scenario Composition

匿名欢迎、个性化欢迎、隐私模式、共享家庭和能力故障降级。

### Stage C5: Personalized Learning

按主体隔离的 Outcome 统计、最小样本门槛、策略版本化、查看/关闭/重置。

### Stage D: Adapter SDK

Go/Python/C++ SDK、Protobuf contracts、conformance kit 与示例 Adapter。

## Stage C Acceptance

1. 场景能声明必需、可选能力和确定性 fallback。
2. 未注册、协议不兼容、不健康或重复的 Provider 无法被静默选中。
3. 匿名模式不要求生物识别即可完成主动欢迎闭环。
4. 个性化模式可组合人脸、声纹或二者，冲突时回退匿名。
5. 用户可分别授权、撤销和删除摄像头、麦克风、人脸及声纹数据。
6. 原始媒体和 embedding 不进入核心、审计或持久化。
7. 免按键语音回复、超时与 P0 拒绝保持确定顺序。
8. 单个能力故障独立降级，不阻塞静默、停止和本地反馈。
9. 相同语义输入、配置、版本和种子得到相同决策。
10. 契约、场景、并发、隐私、故障和端到端测试通过。

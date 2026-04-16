# s10: Team Protocols (团队协议)

`s01 > s02 > s03 > s04 > s05 > s06 | s07 > s08 > s09 > [ s10 ] s11 > s12`

> _"队友之间要有统一的沟通规矩"_ -- 一个 request-response 模式驱动所有协商。
>
> **Harness 层**: 协议 -- 模型之间的结构化握手。

## 问题

s09 中队友能干活能通信, 但缺少结构化协调:

**关机**: 直接杀线程会留下写了一半的文件和过期的 config.json。需要握手 -- 领导请求, 队友批准 (收尾退出) 或拒绝 (继续干)。

**计划审批**: 领导说 "重构认证模块", 队友立刻开干。高风险变更应该先过审。

两者结构一样: 一方发带唯一 ID 的请求, 另一方引用同一 ID 响应。

## 解决方案

```
Shutdown Protocol            Plan Approval Protocol
==================           ======================

Lead             Teammate    Teammate           Lead
  |                 |           |                 |
  |--shutdown_req-->|           |--plan_req------>|
  | {req_id:"abc"}  |           | {req_id:"xyz"}  |
  |                 |           |                 |
  |<--shutdown_resp-|           |<--plan_resp-----|
  | {req_id:"abc",  |           | {req_id:"xyz",  |
  |  approve:true}  |           |  approve:true}  |

Shared FSM:
  [pending] --approve--> [approved]
  [pending] --reject---> [rejected]

Trackers:
  shutdown_requests = {req_id: {target, status}}
  plan_requests     = {req_id: {from, plan, status}}
```

## 工作原理

### System Prompt

```
You are a team lead at %s. Manage teammates with shutdown and plan approval protocols.
```

1. 领导生成 request_id, 通过收件箱发起关机请求。

```go
// 请求跟踪器
// 用于跟踪关闭请求和计划审批请求的状态
var (
	shutdownRequests = make(map[string]map[string]string) // 关闭请求映射表
	planRequests     = make(map[string]map[string]string) // 计划请求映射表
	trackerLock      sync.Mutex                           // 互斥锁，确保并发安全
)

func handleShutdownRequest(teammate string) string {
	trackerLock.Lock()
	defer trackerLock.Unlock()

	// 生成8位请求ID
	reqID := fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)
	shutdownRequests[reqID] = map[string]string{
		"target": teammate,
		"status": "pending",
	}

	bus.Send("lead", teammate, "Please shut down gracefully.",
		"shutdown_request", map[string]interface{}{"request_id": reqID})

	return fmt.Sprintf("Shutdown request %s sent (status: pending)", reqID)
}
```

2. 队友收到请求后, 用 approve/reject 响应。

```go
if toolName == "shutdown_response" {
	reqID := args["request_id"].(string)
	approve := args["approve"].(bool)

	trackerLock.Lock()
	if req, exists := shutdownRequests[reqID]; exists {
		if approve {
			req["status"] = "approved"
		} else {
			req["status"] = "rejected"
		}
		shutdownRequests[reqID] = req
	}
	trackerLock.Unlock()

	reason := ""
	if r, ok := args["reason"]; ok {
		reason = r.(string)
	}

	bus.Send(sender, "lead", reason,
		"shutdown_response",
		map[string]interface{}{"request_id": reqID, "approve": approve})
}
```

3. 计划审批遵循完全相同的模式。队友提交计划 (生成 request_id), 领导审查 (引用同一个 request_id)。

```go
func handlePlanReview(requestID string, approve bool, feedback string) {
	trackerLock.Lock()
	defer trackerLock.Unlock()

	if req, exists := planRequests[requestID]; exists {
		if approve {
			req["status"] = "approved"
		} else {
			req["status"] = "rejected"
		}
		planRequests[requestID] = req

		bus.Send("lead", req["from"], feedback,
			"plan_approval_response",
			map[string]interface{}{"request_id": requestID, "approve": approve})
	}
}
```

一个 FSM, 两种用途。同样的 `pending -> approved | rejected` 状态机可以套用到任何请求-响应协议上。

## 相对 s09 的变更

| 组件     | 之前 (s09) | 之后 (s10)                    |
| -------- | ---------- | ----------------------------- |
| Tools    | 9          | 12 (+shutdown_req/resp +plan) |
| 关机     | 仅自然退出 | 请求-响应握手                 |
| 计划门控 | 无         | 提交/审查与审批               |
| 关联     | 无         | 每个请求一个 request_id       |
| FSM      | 无         | pending -> approved/rejected  |

## 试一试

```sh
cd ai-agent-study/s10
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Spawn alice as a coder. Then request her shutdown.`
2. `List teammates to see alice's status after shutdown approval`
3. `Spawn bob with a risky refactoring task. Review and reject his plan.`
4. `Spawn charlie, have him submit a plan, then approve it.`
5. 输入 `/team` 监控状态

## 业务流程图

### 系统架构总览

```mermaid
graph TB
    subgraph "团队领导循环 (Team Lead Loop)"
        User[用户输入] --> LeadLoop[领导代理循环]
        LeadLoop --> LLMCall[LLM API 调用]
        LLMCall --> ToolExec[工具执行]
        ToolExec --> LeadLoop
    end

    subgraph "协议管理 (Protocol Management)"
        ProtocolMgr[协议管理器] --> ShutdownTracker[关机请求跟踪器]
        ProtocolMgr --> PlanTracker[计划请求跟踪器]
        ProtocolMgr --> TrackerLock[线程安全锁]
    end

    subgraph "消息系统 (Message System)"
        MsgBus[消息总线] --> Inboxes[JSONL 收件箱]
        MsgBus --> Files[文件系统]
        MsgBus --> MsgMutex[消息互斥锁]
    end

    subgraph "状态机引擎 (FSM Engine)"
        ShutdownFSM[关机状态机]
        PlanFSM[计划状态机]
        SharedFSM[共享状态机逻辑]
    end

    subgraph "队友循环 (Teammate Loops)"
        Alice[Alice 循环] --> MsgBus
        Bob[Bob 循环] --> MsgBus
        TeamLead[团队领导循环] --> MsgBus
    end

    ToolExec --> ProtocolMgr
    ToolExec --> MsgBus
    ProtocolMgr --> ShutdownFSM
    ProtocolMgr --> PlanFSM
    ShutdownFSM --> SharedFSM
    PlanFSM --> SharedFSM
```

### 详细流程序列

#### 1. 关机请求协议流程

```mermaid
sequenceDiagram
    participant User as 用户
    participant LeadLoop as 领导循环
    participant ProtocolMgr as 协议管理器
    participant ShutdownTracker as 关机跟踪器
    participant Alice as Alice
    participant MsgBus as 消息总线

    User->>LeadLoop: 请求关闭队友
    LeadLoop->>ProtocolMgr: handleShutdownRequest(队友)
    ProtocolMgr->>ShutdownTracker: 生成请求ID
    ShutdownTracker->>ShutdownTracker: 创建待处理请求
    ProtocolMgr->>MsgBus: 发送关机请求
    MsgBus-->>Alice: 带请求ID的请求
    ProtocolMgr-->>LeadLoop: 请求发送确认
    LeadLoop-->>User: 显示请求状态

    Note over Alice: Alice 处理请求
    Alice->>ProtocolMgr: shutdown_response(批准, 原因)
    ProtocolMgr->>ShutdownTracker: 更新请求状态
    ShutdownTracker-->>ProtocolMgr: 状态已更新
    ProtocolMgr->>MsgBus: 发送响应给领导
    MsgBus-->>LeadLoop: 收到响应
```

#### 2. 计划审批协议流程

```mermaid
sequenceDiagram
    participant Alice as Alice
    participant ProtocolMgr as 协议管理器
    participant PlanTracker as 计划跟踪器
    participant LeadLoop as 领导循环
    participant MsgBus as 消息总线

    Alice->>ProtocolMgr: submit_plan(计划详情)
    ProtocolMgr->>PlanTracker: 生成请求ID
    PlanTracker->>PlanTracker: 创建待处理请求
    ProtocolMgr->>MsgBus: 发送计划请求给领导
    MsgBus-->>LeadLoop: 带请求ID的计划

    Note over LeadLoop: 领导审查计划
    LeadLoop->>ProtocolMgr: handlePlanReview(请求ID, 批准, 反馈)
    ProtocolMgr->>PlanTracker: 更新请求状态
    PlanTracker-->>ProtocolMgr: 状态已更新
    ProtocolMgr->>MsgBus: 发送计划审批响应
    MsgBus-->>Alice: 审批决定

    alt 计划已批准
        Alice->>Alice: 开始执行计划
    else 计划被拒绝
        Alice->>Alice: 根据反馈修改计划
    end
```

#### 3. 共享 FSM 状态管理流程

```mermaid
sequenceDiagram
    participant Request as 请求处理器
    participant FSM as 共享状态机
    participant Tracker as 请求跟踪器
    participant Response as 响应处理器

    Request->>FSM: 创建带请求ID的请求
    FSM->>Tracker: 存储为待处理状态
    Tracker-->>FSM: 请求已存储

    Note over FSM: 状态转换逻辑
    alt 批准请求
        Response->>FSM: approve(请求ID)
        FSM->>Tracker: 更新为已批准
        Tracker-->>FSM: 状态已更新
    else 拒绝请求
        Response->>FSM: reject(请求ID)
        FSM->>Tracker: 更新为已拒绝
        Tracker-->>FSM: 状态已更新
    end

    FSM-->>Response: 状态变更确认
```

#### 4. 协议跟踪监控流程

```mermaid
sequenceDiagram
    participant User as 用户
    participant LeadLoop as 领导循环
    participant ProtocolMgr as 协议管理器
    participant ShutdownTracker as 关机跟踪器
    participant PlanTracker as 计划跟踪器

    User->>LeadLoop: 请求协议状态
    LeadLoop->>ProtocolMgr: 获取所有请求状态
    ProtocolMgr->>ShutdownTracker: 获取关机请求
    ProtocolMgr->>PlanTracker: 获取计划请求
    ShutdownTracker-->>ProtocolMgr: 关机请求列表
    PlanTracker-->>ProtocolMgr: 计划请求列表
    ProtocolMgr->>ProtocolMgr: 格式化状态报告
    ProtocolMgr-->>LeadLoop: 完整状态报告
    LeadLoop-->>User: 显示协议状态
```

### 关键状态转换

```mermaid
stateDiagram-v2
    [*] --> pending: request_created()
    pending --> approved: approve_request()
    pending --> rejected: reject_request()
    approved --> [*]: request_completed()
    rejected --> [*]: request_closed()

    note right of pending
        请求等待响应
        请求ID已分配
        跟踪活动状态
    end note

    note right of approved
        请求已接受
        操作已授权
        继续执行
    end note

    note right of rejected
        请求被拒绝
        提供反馈
        可以重新提交
    end note
```

### 协议消息流架构

```mermaid
graph TB
    subgraph "请求类型 (Request Types)"
        ShutdownReq[关机请求]
        PlanReq[计划请求]
        BroadcastReq[广播请求]
    end

    subgraph "响应类型 (Response Types)"
        ShutdownResp[关机响应]
        PlanResp[计划响应]
        ApprovalResp[审批响应]
    end

    subgraph "消息处理 (Message Processing)"
        GenerateID[生成请求ID]
        TrackRequest[跟踪请求]
        SendRequest[发送请求]
        ProcessResponse[处理响应]
        UpdateStatus[更新状态]
    end

    subgraph "存储层 (Storage Layer)"
        ShutdownTracker[关机跟踪器]
        PlanTracker[计划跟踪器]
        Inboxes[JSONL 收件箱]
        FileSystem[文件系统]
    end

    ShutdownReq --> GenerateID
    PlanReq --> GenerateID
    BroadcastReq --> GenerateID

    GenerateID --> TrackRequest
    TrackRequest --> SendRequest
    SendRequest --> Inboxes

    ShutdownResp --> ProcessResponse
    PlanResp --> ProcessResponse
    ApprovalResp --> ProcessResponse

    ProcessResponse --> UpdateStatus
    UpdateStatus --> ShutdownTracker
    UpdateStatus --> PlanTracker
```

### 协议协调架构

```mermaid
graph TB
    subgraph "协议层 (Protocol Layer)"
        ShutdownProtocol[关机协议]
        PlanProtocol[计划协议]
        SharedLogic[共享状态机逻辑]
    end

    subgraph "跟踪层 (Tracking Layer)"
        RequestTracker[请求跟踪器]
        StateManager[状态管理器]
        IDGenerator[ID 生成器]
    end

    subgraph "通信层 (Communication Layer)"
        MessageRouter[消息路由器]
        RequestHandler[请求处理器]
        ResponseHandler[响应处理器]
    end

    subgraph "存储层 (Storage Layer)"
        ShutdownStore[关机存储]
        PlanStore[计划存储]
        MessageStore[消息存储]
    end

    ShutdownProtocol --> SharedLogic
    PlanProtocol --> SharedLogic
    SharedLogic --> RequestTracker

    RequestTracker --> StateManager
    RequestTracker --> IDGenerator
    StateManager --> MessageRouter

    MessageRouter --> RequestHandler
    MessageRouter --> ResponseHandler
    RequestHandler --> ShutdownStore
    ResponseHandler --> PlanStore
    MessageRouter --> MessageStore
```

### 核心组件交互

```mermaid
graph TB
    subgraph "协议管理器核心 (ProtocolManager Core)"
        PM[协议管理器] --> ST[关机跟踪器]
        PM --> PT[计划跟踪器]
        PM --> TL[跟踪器锁]
    end

    subgraph "状态机引擎核心 (FSM Engine Core)"
        FSM[共享状态机] --> States[状态机]
        FSM --> Transitions[状态转换]
        FSM --> Validator[请求验证器]
    end

    subgraph "协议操作 (Protocol Operations)"
        ShutdownReq[关机请求] --> ShutdownResp[关机响应]
        PlanReq[计划请求] --> PlanResp[计划响应]
        Track[跟踪请求] --> Update[更新状态]
        Generate[生成ID] --> Store[存储请求]
    end

    subgraph "团队集成 (Team Integration)"
        TL[团队领导] --> ProtocolTools[协议工具]
        Teammate[队友] --> ProtocolTools
        ProtocolTools --> ShutdownReq
        ProtocolTools --> PlanReq
        ProtocolTools --> Track
        ProtocolTools --> TL
        ProtocolTools --> Teammate
    end

    TL --> PM
    ST --> FSM
    PT --> FSM
    TL --> Track
    Track --> Generate
    Generate --> Store
    Store --> ST
    Store --> PT
```

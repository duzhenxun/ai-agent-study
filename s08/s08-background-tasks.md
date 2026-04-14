# s08: Background Tasks (后台任务)

`s01 > s02 > s03 > s04 > s05 > s06 | s07 > [ s08 ] s09 > s10 > s11 > s12`

> _"慢操作丢后台, agent 继续想下一步"_ -- 后台线程跑命令, 完成后注入通知。
>
> **Harness 层**: 后台执行 -- 模型继续思考, harness 负责等待。

## 问题

有些命令要跑好几分钟: `npm install`、`pytest`、`docker build`。阻塞式循环下模型只能干等。用户说 "装依赖, 顺便建个配置文件", 智能体却只能一个一个来。

## 解决方案

```
Main thread                Background thread
+-----------------+        +-----------------+
| agent loop      |        | subprocess runs |
| ...             |        | ...             |
| [LLM call] <---+------- | enqueue(result) |
|  ^drain queue   |        +-----------------+
+-----------------+

Timeline:
Agent --[spawn A]--[spawn B]--[other work]----
             |          |
             v          v
          [A runs]   [B runs]      (parallel)
             |          |
             +-- results injected before next LLM call --+
```

## 工作原理

#### System Prompt

```
You are a coding agent at %s. Use background_run for long-running commands.
```

1. BackgroundManager 用线程安全的通知队列追踪任务。

```go
// BackgroundManager 管理后台异步任务。
type BackgroundManager struct {
	tasks             map[string]*Task // 存储所有任务的映射表
	notificationQueue []Notification   // 待通知给大模型的消息队列
	mu                sync.Mutex       // 保证并发安全的互斥锁
}

// NewBackgroundManager 创建一个新的后台任务管理器。
func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{
		tasks: make(map[string]*Task),
	}
}
```

2. `run()` 启动守护线程, 立即返回。

```go
// Run 启动一个异步后台任务。
// 它会立即返回任务 ID，并在后台协程中执行命令。
func (bm *BackgroundManager) Run(command string) string {
	// 生成一个基于当前纳秒时间戳的简单 8 位任务 ID
	taskID := strconv.FormatInt(time.Now().UnixNano(), 10)[:8]

	bm.mu.Lock()
	bm.tasks[taskID] = &Task{Status: "running", Command: command}
	bm.mu.Unlock()

	// 启动协程异步执行任务
	go bm.execute(taskID, command)

	return fmt.Sprintf("Background task %s started: %s", taskID, command)
}
```

3. 子进程完成后, 结果进入通知队列。

```go
// execute 是内部执行函数，负责实际运行命令并更新任务状态。
func (bm *BackgroundManager) execute(taskID, command string) {
	// 为后台任务设置 300 秒超时限制
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workdir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	output := out.String()
	status := "completed"

	// 处理超时和执行错误
	if ctx.Err() == context.DeadlineExceeded {
		output = "Error: Timeout (300s)"
		status = "timeout"
	} else if err != nil {
		output = fmt.Sprintf("Error: %v\nOutput:\n%s", err, output)
		status = "error"
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	// 更新任务状态和结果
	task := bm.tasks[taskID]
	task.Status = status
	task.Result = output

	// 将完成的消息加入通知队列
	bm.notificationQueue = append(bm.notificationQueue, Notification{
		TaskID:  taskID,
		Status:  status,
		Command: command,
		Result:  output,
	})
}
```

4. 每次 LLM 调用前排空通知队列。

```go
func agentLoop(messages *[]Message) {
	*messages = append([]Message{{Role: "system", Content: system}}, *messages...)
	for {
		notifs := bg.DrainNotifications()
		if len(notifs) > 0 {
			fmt.Println(">>>Background notifications:", notifs)
			var notifTexts []string
			for _, n := range notifs {
				notifTexts = append(notifTexts, fmt.Sprintf("[bg:%s] %s: %s", n.TaskID, n.Status, n.Result))
			}
			*messages = append(*messages, Message{Role: "user", Content: fmt.Sprintf("<background-results>\n%s\n</background-results>", strings.Join(notifTexts, "\n"))})
			*messages = append(*messages, Message{Role: "assistant", Content: "Noted background results."})
		}

		msg, err := chatCompletionsCreate(*messages, openAITools())

		if err != nil {
			log.Printf("Error calling API: %v", err)
			return
		}

		*messages = append(*messages, msg)

		if len(msg.ToolCalls) == 0 {
			if msg.Content != "" {
				fmt.Println(msg.Content)
			}
			return
		}

		for _, tc := range msg.ToolCalls {
			name := tc.Function.Name
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)

			handler, ok := toolHandlers[name]
			var output string
			if ok {
				output = handler.(func(map[string]interface{}) string)(args)
			} else {
				output = fmt.Sprintf("Unknown tool: %s", name)
			}

			if len(output) > 200 {
				fmt.Printf(">✌️ 执行命令:%s \n ✌️ 参数:%+v \n ✌️ 结果:%s...✌️\n\n\n", name, args, output[:200])
			} else {
				fmt.Printf(">✌️ 执行命令:%s \n ✌️ 参数:%+v \n ✌️ 结果:%s ✌️\n\n\n", name, args, output)
			}

			*messages = append(*messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}
	}
}
```

循环保持单线程。只有子进程 I/O 被并行化。

## 相对 s07 的变更

| 组件     | 之前 (s07) | 之后 (s08)                        |
| -------- | ---------- | --------------------------------- |
| Tools    | 8          | 6 (基础 + background_run + check) |
| 执行方式 | 仅阻塞     | 阻塞 + 后台线程                   |
| 通知机制 | 无         | 每轮排空的队列                    |
| 并发     | 无         | 守护线程                          |

## 试一试

```sh
cd ai-agent-study/s08
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Run "sleep 5 && echo done" in the background, then create a file while it runs`
2. `Start 3 background tasks: "sleep 2", "sleep 4", "sleep 6". Check their status.`
3. `Run pytest in the background and keep working on other things`

## 业务流程图

### 系统架构总览

```mermaid
graph TB
    subgraph "主线程"
        User[用户输入] --> AgentLoop[Agent 循环]
        AgentLoop --> DrainQueue[清空通知队列]
        DrainQueue --> LLMCall[LLM API 调用]
        LLMCall --> ToolExec[工具执行]
        ToolExec --> AgentLoop
    end

    subgraph "后台线程"
        BgRun[后台运行] --> TaskExec[任务执行]
        TaskExec --> NotifQueue[通知队列]
        NotifQueue --> DrainQueue
    end

    subgraph "任务管理"
        BgMgr[后台管理器] --> TaskMap[任务映射]
        BgMgr --> NotifQueue
        Check[检查后台] --> TaskMap
    end

    ToolExec --> BgRun
    ToolExec --> Check
    BgRun --> BgMgr
```

### 详细流程序列

#### 1. 用户交互与 Agent 主循环流程

```mermaid
sequenceDiagram
    participant User as 用户
    participant AgentLoop as Agent循环
    participant BgManager as 后台管理器
    participant LLM as 大模型
    participant NotificationQueue as 通知队列

    User->>AgentLoop: 输入命令
    AgentLoop->>BgManager: 清空通知()
    BgManager->>NotificationQueue: 获取待处理通知
    NotificationQueue-->>BgManager: 返回通知
    BgManager-->>AgentLoop: 通知列表

    alt 有后台结果
        AgentLoop->>LLM: 添加后台结果到消息
        AgentLoop->>LLM: 聊天完成请求
    else 无后台结果
        AgentLoop->>LLM: 直接发送聊天完成请求
    end

    LLM-->>AgentLoop: LLM响应和工具调用
    AgentLoop->>AgentLoop: 执行工具
    AgentLoop-->>User: 显示结果
```

#### 2. 后台任务执行流程

```mermaid
sequenceDiagram
    participant AgentLoop as Agent循环
    participant BackgroundManager as 后台管理器
    participant TaskExecution as 任务执行
    participant NotificationQueue as 通知队列
    participant Subprocess as 子进程

    AgentLoop->>BackgroundManager: 后台运行(命令)
    BackgroundManager->>BackgroundManager: 生成任务ID
    BackgroundManager->>BackgroundManager: 创建"运行中"状态的任务
    BackgroundManager->>TaskExecution: 启动执行(任务ID, 命令)
    BackgroundManager-->>AgentLoop: 立即返回任务ID

    TaskExecution->>Subprocess: 执行命令上下文()
    Subprocess-->>TaskExecution: 等待完成

    alt 任务成功完成
        TaskExecution->>TaskExecution: 状态 = "已完成"
    else 任务超时
        TaskExecution->>TaskExecution: 状态 = "超时"
    else 任务失败
        TaskExecution->>TaskExecution: 状态 = "错误"
    end

    TaskExecution->>BackgroundManager: 更新任务状态和结果
    TaskExecution->>NotificationQueue: 添加通知
    NotificationQueue-->>AgentLoop: 可用于下次LLM调用
```

#### 3. 任务状态检查流程

```mermaid
sequenceDiagram
    participant AgentLoop as Agent循环
    participant BackgroundManager as 后台管理器
    participant TaskMap as 任务映射

    AgentLoop->>BackgroundManager: 检查后台(任务ID?)

    alt 提供任务ID
        BackgroundManager->>TaskMap: 查找特定任务
        TaskMap-->>BackgroundManager: 任务详情
        BackgroundManager-->>AgentLoop: 单个任务状态
    else 未提供任务ID
        BackgroundManager->>TaskMap: 获取所有任务
        TaskMap-->>BackgroundManager: 所有任务列表
        BackgroundManager-->>AgentLoop: 任务概览
    end
```

### 关键状态转换

```mermaid
stateDiagram-v2
    [*] --> 运行中: 调用后台运行()
    运行中 --> 已完成: 命令成功
    运行中 --> 超时: 超过300秒超时
    运行中 --> 错误: 命令失败
    已完成 --> [*]: 任务结果被消费
    超时 --> [*]: 错误结果被消费
    错误 --> [*]: 错误结果被消费

    note right of 运行中
        后台协程在子进程中
        执行命令，
        限制300秒超时
    end note

    note right of 已完成
        结果添加到通知队列
        等待下次LLM调用
    end note
```

### 数据流架构

```mermaid
graph LR
    subgraph "输入流程"
        A[用户命令] --> B{工具类型}
        B -->|后台运行| C[后台任务]
        B -->|检查后台| D[任务查询]
        B -->|其他工具| E[立即执行]
    end

    subgraph "后台处理"
        C --> F[任务队列]
        F --> G[协程池]
        G --> H[子进程执行]
        H --> I[结果处理]
        I --> J[通知队列]
    end

    subgraph "输出流程"
        J --> K[下次LLM调用]
        D --> L[任务状态响应]
        E --> M[直接结果]
        K --> N[增强上下文]
        L --> N
        M --> N
        N --> O[用户响应]
    end
```

### 核心组件交互

```mermaid
graph TB
    subgraph "BackgroundManager Core"
        BM[后台管理器] --> TM[任务映射]
        BM --> NQ[通知队列]
        BM --> MU[同步锁]
    end

    subgraph "Task Lifecycle"
        Run[运行任务] --> Create[创建任务]
        Create --> Execute[执行任务]
        Execute --> Update[更新状态]
        Update --> Notify[添加通知]
        Notify --> Drain[清空通知]
    end

    subgraph "Agent Integration"
        AL[Agent循环] --> Drain
        Drain --> MSG[添加消息]
        MSG --> LLM[LLM调用]
        LLM --> Tools[工具执行]
        Tools --> Run
    end

    MU --> TM
    MU --> NQ
    Execute --> TM
    Execute --> NQ
```

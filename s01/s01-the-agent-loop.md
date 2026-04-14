# s01: The Agent Loop (智能体循环)

`[ s01 ] s02 > s03 > s04 > s05 > s06 | s07 > s08 > s09 > s10 > s11 > s12`

> _"One loop & Bash is all you need"_ -- 一个工具 + 一个循环 = 一个智能体。
>
> **Harness 层**: 循环 -- 模型与真实世界的第一道连接。

## 问题

语言模型能推理代码, 但碰不到真实世界 -- 不能读文件、跑测试、看报错。没有循环, 每次工具调用你都得手动把结果粘回去。你自己就是那个循环。

## 解决方案

```
+--------+      +-------+      +---------+
|  User  | ---> |  LLM  | ---> |  Tool   |
| prompt |      |       |      | execute |
+--------+      +---+---+      +----+----+
                    ^                |
                    |   tool_result  |
                    +----------------+
                    (loop until stop_reason != "tool_use")
```

一个退出条件控制整个流程。循环持续运行, 直到模型不再调用工具。

## 工作原理

#### System Prompt (English)

```
You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.
```

#### System Prompt (Chinese)

```
您是 %s 的编码代理。使用 bash 来解决任务。执行，不要解释。
```

1. 用户 prompt 作为第一条消息。

```go
messages = []Message{{Role: "user", Content: query}}
```

2. 将消息和工具定义一起发给 LLM。

```go
msg, err := chatCompletionsCreate(messages, openAITools())
if err != nil {
    log.Printf("Error calling API: %v", err)
    return
}
```

3. 追加助手响应。检查 `stop_reason` -- 如果模型没有调用工具, 结束。

```go
messages = append(messages, msg)
if len(msg.ToolCalls) == 0 {
    if msg.Content != "" {
        fmt.Println(msg.Content)
    }
    return
}
```

4. 执行每个工具调用, 收集结果, 作为 user 消息追加。回到第 2 步。

```go
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

    messages = append(messages, Message{
        Role:       "tool",
        ToolCallID: tc.ID,
        Content:    output,
    })
}
```

组装为一个完整函数:

```go
func agentLoop(messages *[]Message) {
    *messages = append([]Message{{Role: "system", Content: system}}, *messages...)
    for {
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

            *messages = append(*messages, Message{
                Role:       "tool",
                ToolCallID: tc.ID,
                Content:    output,
            })
        }
    }
}
```

不到 30 行, 这就是整个智能体。后面 11 个章节都在这个循环上叠加机制 -- 循环本身始终不变。

## 变更内容

| 组件         | 之前 | 之后                        |
| ------------ | ---- | --------------------------- |
| Agent loop   | (无) | `while True` + stop_reason  |
| Tools        | (无) | `bash` (单一工具)           |
| Messages     | (无) | 累积式消息列表              |
| Control flow | (无) | `stop_reason != "tool_use"` |

## 试一试

```sh
cd ai-agent-study/s01
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Create a file called hello.py that prints "Hello, World!"`
2. `List all Python files in this directory`
3. `What is the current git branch?`
4. `Create a directory called test_output and write 3 files in it`

**Chinese prompts (also work well):**

1. `Create a file called hello.py that prints "Hello, World!"`
2. `List all Python files in this directory`
3. `What is the current git branch?`
4. `Create a directory called test_output and write 3 files in it`

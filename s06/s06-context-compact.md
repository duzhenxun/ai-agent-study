# s06: Context Compact (上下文压缩)

`s01 > s02 > s03 > s04 > s05 > [ s06 ] | s07 > s08 > s09 > s10 > s11 > s12`

> _"上下文总会满, 要有办法腾地方"_ -- 三层压缩策略, 换来无限会话。
>
> **Harness 层**: 压缩 -- 干净的记忆, 无限的会话。

## 问题

上下文窗口是有限的。读一个 1000 行的文件就吃掉 \~4000 token; 读 30 个文件、跑 20 条命令, 轻松突破 100k token。不压缩, 智能体根本没法在大项目里干活。

## 解决方案

三层压缩, 激进程度递增:

```
Every turn:
+------------------+
| Tool call result |
+------------------+
        |
        v
[Layer 1: micro_compact]        (silent, every turn)
  Replace tool_result > 3 turns old
  with "[Previous: used {tool_name}]"
        |
        v
[Check: tokens > 50000?]
   |               |
   no              yes
   |               |
   v               v
continue    [Layer 2: auto_compact]
              Save transcript to .transcripts/
              LLM summarizes conversation.
              Replace all messages with [summary].
                    |
                    v
            [Layer 3: compact tool]
              Model calls compact explicitly.
              Same summarization as auto_compact.
```

## 工作原理

#### System Prompt

```
You are a coding agent at %s. Use tools to solve tasks.
```

1. **第一层 -- micro_compact**: 每次 LLM 调用前, 将旧的 tool result 替换为占位符。

```go
func microCompact(messages *[]Message) {
	toolResults := []struct {
		msgIndex int
		partIndex int
		part      map[string]interface{}
	}{}

	for i, msg := range *messages {
		if msg.Role == "user" {
			if contentList, ok := msg.Content.([]interface{}); ok {
				for j, part := range contentList {
					if partMap, ok := part.(map[string]interface{}); ok {
						if partType, ok := partMap["type"]; ok && partType == "tool_result" {
						toolResults = append(toolResults, struct {
							msgIndex int
						partIndex int
						part      map[string]interface{}
						}{i, j, partMap})
					}
				}
				}
			}
		}
	}

	if len(toolResults) <= keepRecent {
		return
	}

	for _, result := range toolResults[:len(toolResults)-keepRecent] {
		if content, ok := result.part["content"].(string); ok && len(content) > 100 {
			if toolName, nameOk := result.part["tool_use_id"].(string); nameOk {
				result.part["content"] = fmt.Sprintf("[Previous: used %s]", toolName)
			}
	}
}
```

1. **第二层 -- auto_compact**: token 超过阈值时, 保存完整对话到磁盘, 让 LLM 做摘要。

```go
func autoCompact(messages *[]Message) []Message {
	// Save transcript for recovery
	transcriptPath := filepath.Join(transcriptDir, fmt.Sprintf("transcript_%d.jsonl", time.Now().Unix()))
	os.MkdirAll(transcriptDir, 0755)
	file, err := os.Create(transcriptPath)
	if err == nil {
		defer file.Close()
		encoder := json.NewEncoder(file)
		for _, msg := range *messages {
			encoder.Encode(msg)
		}
	}

	// LLM summarizes
	messagesJSON, _ := json.Marshal(*messages)
	truncatedMessages := string(messagesJSON)
	if len(truncatedMessages) > 80000 {
		truncatedMessages = truncatedMessages[:80000]
	}

	summaryPrompt := fmt.Sprintf("Summarize this conversation for continuity...%s", truncatedMessages)
	msg, err := chatCompletionsCreate([]Message{{Role: "user", Content: summaryPrompt}}, nil)
	if err != nil {
		log.Printf("Error in auto compact: %v", err)
		return *messages
	}

	return []Message{
		{Role: "user", Content: fmt.Sprintf("[Compressed]\n\n%s", msg.Content)},
		{Role: "assistant", Content: "Understood. Continuing."},
	}
}
```

1. **第三层 -- manual compact**: `compact` 工具按需触发同样的摘要机制。
2. 循环整合三层:

```go
func agentLoop(messages *[]Message) {
	*messages = append([]Message{{Role: "system", Content: system}}, *messages...)
	for {
		// Layer 1: micro compact
		microCompact(messages)

		// Layer 2: auto compact if threshold exceeded
		if estimateTokens(*messages) > threshold {
			*messages = autoCompact(messages)
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

		// ... tool execution ...
		for _, tc := range msg.ToolCalls {
			name := tc.Function.Name
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)

			// Layer 3: manual compact
			if name == "compact" {
				*messages = autoCompact(messages)
				continue
			}

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

完整历史通过 _transcript_ 保存在磁盘上。信息没有真正丢失, 只是移出了活跃上下文。

## 相对 s05 的变更

| 组件          | 之前 (s05) | 之后 (s06)           |
| ------------- | ---------- | -------------------- |
| Tools         | 5          | 5 (基础 + compact)   |
| 上下文管理    | 无         | 三层压缩             |
| Micro-compact | 无         | 旧结果 -> 占位符     |
| Auto-compact  | 无         | token 阈值触发       |
| Transcripts   | 无         | 保存到 .transcripts/ |

## 试一试

```sh
cd ai-agent-study/s06
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Read every Python file in the agents/ directory one by one` (观察 micro-compact 替换旧结果)
2. `Keep reading files until compression triggers automatically`
3. `Use the compact tool to manually compress the conversation`

# s03: TodoWrite (待办写入)

`s01 > s02 > [ s03 ] s04 > s05 > s06 | s07 > s08 > s09 > s10 > s11 > s12`

> _"没有计划的 agent 走哪算哪"_ -- 先列步骤再动手, 完成率翻倍。
>
> **Harness 层**: 规划 -- 让模型不偏航, 但不替它画航线。

## 问题

多步任务中, 模型会丢失进度 -- 重复做过的事、跳步、跑偏。对话越长越严重: 工具结果不断填满上下文, 系统提示的影响力逐渐被稀释。一个 10 步重构可能做完 1-3 步就开始即兴发挥, 因为 4-10 步已经被挤出注意力了。

## 解决方案

```
+--------+      +-------+      +---------+
|  User  | ---> |  LLM  | ---> | Tools   |
| prompt |      |       |      | + todo  |
+--------+      +---+---+      +----+----+
                    ^                |
                    |   tool_result  |
                    +----------------+
                          |
              +-----------+-----------+
              | TodoManager state     |
              | [ ] task A            |
              | [>] task B  <- doing  |
              | [x] task C            |
              +-----------------------+
                          |
              if rounds_since_todo >= 3:
                inject <reminder> into tool_result
```

## 工作原理

#### 系统提示

```
You are a coding agent at %s.
Use the todo tool to plan multi-step tasks. Mark in_progress before starting, completed when done.
Prefer tools over prose.
```

1. TodoManager 存储带状态的项目。同一时间只允许一个 `in_progress`。

```go
// TodoManager 管理待办事项
type TodoManager struct {
	items []TodoItem
	mu    sync.Mutex
}

type TodoItem struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

func NewTodoManager() *TodoManager {
	return &TodoManager{
		items: []TodoItem{},
	}
}

func (tm *TodoManager) Update(items []TodoItem) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	validated := []TodoItem{}
	inProgressCount := 0
	for _, item := range items {
		status := item.Status
		if status == "" {
			status = "pending"
		}
		if status == "in_progress" {
			inProgressCount++
		}
		validated = append(validated, TodoItem{
			ID:     item.ID,
			Text:   item.Text,
			Status: status,
		})
	}

	if inProgressCount > 1 {
		return "", fmt.Errorf("Only one task can be in_progress")
	}

	tm.items = validated
	return tm.render(), nil
}
```

2. `todo` 工具和其他工具一样加入 dispatch map。

```go
// Tool Handlers Map
var toolHandlers = map[string]interface{}{
	// ...base tools...
	"todo": func(args map[string]interface{}) string {
		itemsInterface := args["items"].([]interface{})
		var items []TodoItem
		for _, itemInterface := range itemsInterface {
			if itemMap, ok := itemInterface.(map[string]interface{}); ok {
				id := int(itemMap["id"].(float64))
				text := itemMap["text"].(string)
				status := ""
				if s, ok := itemMap["status"].(string); ok {
					status = s
				}
				items = append(items, TodoItem{ID: id, Text: text, Status: status})
			}
		}
		result, err := todoManager.Update(items)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
}
```

3. nag reminder: 模型连续 3 轮以上不调用 `todo` 时注入提醒。

```go
if roundsSinceTodo >= 3 && len(messages) > 0 {
	last := messages[len(messages)-1]
	if last.Role == "user" {
		if contentList, ok := last.Content.([]interface{}); ok {
			reminder := map[string]interface{}{
				"type": "text",
				"text": "<reminder>Update your todos.</reminder>",
			}
			contentList = append([]interface{}{reminder}, contentList...)
			last.Content = contentList
		}
	}
}
```

"同时只能有一个 in_progress" 强制顺序聚焦。nag reminder 制造问责压力 -- 你不更新计划, 系统就追着你问。

## 相对 s02 的变更

| 组件       | 之前 (s02) | 之后 (s03)                 |
| ---------- | ---------- | -------------------------- |
| Tools      | 4          | 5 (+todo)                  |
| 规划       | 无         | 带状态的 TodoManager       |
| Nag 注入   | 无         | 3 轮后注入 `<reminder>`    |
| Agent loop | 简单分发   | + rounds_since_todo 计数器 |

## 试一试

```sh
cd ai-agent-study/s03
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Refactor the file hello.py: add type hints, docstrings, and a main guard`
2. `Create a Python package with __init__.py, utils.py, and tests/test_utils.py`
3. `Review all Python files and fix any style issues`

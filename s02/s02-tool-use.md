# s02: Tool Use (工具使用)

`s01 > [ s02 ] s03 > s04 > s05 > s06 | s07 > s08 > s09 > s10 > s11 > s12`

> _"加一个工具, 只加一个 handler"_ -- 循环不用动, 新工具注册进 dispatch map 就行。
>
> **Harness 层**: 工具分发 -- 扩展模型能触达的边界。

## 问题

只有 `bash` 时, 所有操作都走 shell。`cat` 截断不可预测, `sed` 遇到特殊字符就崩, 每次 bash 调用都是不受约束的安全面。专用工具 (`read_file`, `write_file`) 可以在工具层面做路径沙箱。

关键洞察: 加工具不需要改循环。

## 解决方案

```
+--------+      +-------+      +------------------+
|  User  | ---> |  LLM  | ---> | Tool Dispatch    |
| prompt |      |       |      | {                |
+--------+      +---+---+      |   bash: run_bash |
                    ^           |   read: run_read |
                    |           |   write: run_wr  |
                    +-----------+   edit: run_edit |
                    tool_result | }                |
                                +------------------+

The dispatch map is a dict: {tool_name: handler_function}.
One lookup replaces any if/elif chain.
```

## 工作原理

#### System Prompt

```
You are a coding agent at %s. Use tools to solve tasks. Act, don't explain.
```

1. 每个工具有一个处理函数。路径沙箱防止逃逸工作区。

```go
func safePath(p string) (string, error) {
	workdirAbs, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}

	absPath, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	if !strings.HasPrefix(absPath, workdirAbs) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}

	return absPath, nil
}

func runRead(path string, limit *int) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	content, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	lines := strings.Split(string(content), "\n")
	if limit != nil && *limit < len(lines) {
		limitedLines := lines[:*limit]
		limitedLines = append(limitedLines, fmt.Sprintf("... (%d more lines)", len(lines)-*limit))
		result := strings.Join(limitedLines, "\n")
		if len(result) > 50000 {
			return result[:50000]
		}
		return result
	}
	result := strings.Join(lines, "\n")
	if len(result) > 50000 {
		return result[:50000]
	}
	return result
}
```

2. dispatch map 将工具名映射到处理函数。

```go
// Tool Handlers Map
var toolHandlers = map[string]interface{}{
	"bash": func(args map[string]interface{}) string {
		return runBash(args["command"].(string))
	},
	"read_file": func(args map[string]interface{}) string {
		var limit *int
		if l, ok := args["limit"]; ok {
			val := int(l.(float64))
			limit = &val
		}
		return runRead(args["path"].(string), limit)
	},
	"write_file": func(args map[string]interface{}) string {
		return runWrite(args["path"].(string), args["content"].(string))
	},
	"edit_file": func(args map[string]interface{}) string {
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	},
}
```

3. 循环中按名称查找处理函数。循环体本身与 s01 完全一致。

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

加工具 = 加 handler + 加 schema。循环永远不变。

## 相对 s01 的变更

| 组件       | 之前 (s01)       | 之后 (s02)                  |
| ---------- | ---------------- | --------------------------- |
| Tools      | 1 (仅 bash)      | 4 (bash, read, write, edit) |
| Dispatch   | 硬编码 bash 调用 | `TOOL_HANDLERS` 字典        |
| 路径安全   | 无               | `safe_path()` 沙箱          |
| Agent loop | 不变             | 不变                        |

## 试一试

```sh
cd ai-agent-study/s02
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Read the file requirements.txt`
2. `Create a file called greet.py with a greet(name) function`
3. `Edit greet.py to add a docstring to the function`
4. `Read greet.py to verify the edit worked`

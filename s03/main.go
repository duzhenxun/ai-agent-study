package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Global variables for configuration
var (
	model          string
	baseURL        string
	apiKey         string
	authHeaderName string
	workdir, _     = os.Getwd()
	system         = ""
)

// loadEnv 加载 .env 文件中的环境变量
func loadEnv() error {
	workdir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// 向上查找项目根目录（包含 .env 文件的目录）
	projectRoot := workdir
	for {
		envPath := filepath.Join(projectRoot, ".env")
		if _, err := os.Stat(envPath); err == nil {
			// 找到 .env 文件，使用当前目录
			break
		}

		// 向上一级目录查找
		parent := filepath.Dir(projectRoot)
		if parent == projectRoot {
			// 已经到达根目录，停止查找
			break
		}
		projectRoot = parent
	}

	envPath := filepath.Join(projectRoot, ".env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		fmt.Printf("Warning: .env file not found at %s, using system environment variables only\n", envPath)
		return nil
	}

	file, err := os.Open(envPath)
	if err != nil {
		return fmt.Errorf("failed to open .env file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释行
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析 KEY=VALUE 格式
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// 移除引号
		if (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) ||
			(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
			value = value[1 : len(value)-1]
		}

		// 只有当环境变量不存在时才设置
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}

	fmt.Printf("Loaded environment variables from .env file\n")
	return nil
}

// initConfig 初始化配置变量
func initConfig() {
	model = os.Getenv("MODEL_ID")
	baseURL = os.Getenv("OPENAI_BASE_URL")
	apiKey = os.Getenv("OPENAI_API_KEY")
	authHeaderName = os.Getenv("OPENAI_AUTH_HEADER")
	system = fmt.Sprintf("You are a coding agent at %s.\nUse the todo tool to plan multi-step tasks. Mark in_progress before starting, completed when done.\nPrefer tools over prose.", workdir)
}

// TodoManager 管理TODO任务列表
// 这是S03版本的核心特性，提供了完整的任务管理功能
// 支持任务的创建、更新、状态管理和持久化存储

// TodoItem 表示一个TODO任务项
type TodoItem struct {
	ID     string `json:"id"`     // 任务唯一标识符
	Text   string `json:"text"`   // 任务描述文本
	Status string `json:"status"` // 任务状态（pending/in_progress/completed）
}

// TodoManager 任务管理器，负责管理所有TODO任务
type TodoManager struct {
	mu    sync.Mutex // 互斥锁，确保并发安全
	items []TodoItem // 任务列表
}

// Update 更新任务列表
// 这个函数接收新的任务列表，进行验证后更新管理器中的任务
// 返回更新结果消息和可能的错误
func (tm *TodoManager) Update(items []interface{}) (string, error) {
	// 加锁确保并发安全
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// 限制最大任务数量，防止内存溢出
	if len(items) > 20 {
		return "", fmt.Errorf("max 20 todos allowed")
	}

	// 验证和转换任务数据
	var validated []TodoItem
	inProgressCount := 0 // 统计进行中的任务数量
	for i, itemRaw := range items {
		// 检查每个任务项是否为有效的对象
		itemMap, ok := itemRaw.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("item %d is not a valid object", i+1)
		}

		// 提取任务的基本信息
		text, _ := itemMap["text"].(string)
		status, _ := itemMap["status"].(string)
		id, _ := itemMap["id"].(string)

		// 验证任务文本不为空
		if text == "" {
			return "", fmt.Errorf("item %s: text required", id)
		}
		// 验证任务状态是否有效
		if status != "pending" && status != "in_progress" && status != "completed" {
			return "", fmt.Errorf("item %s: invalid status '%s'", id, status)
		}
		// 统计进行中的任务数量
		if status == "in_progress" {
			inProgressCount++
		}
		// 将验证后的任务添加到列表
		validated = append(validated, TodoItem{ID: id, Text: text, Status: status})
	}

	// 确保同时只有一个任务处于进行中状态
	if inProgressCount > 1 {
		return "", fmt.Errorf("only one task can be in_progress at a time")
	}

	// 更新任务列表
	tm.items = validated
	// 返回渲染后的任务列表
	return tm.render(), nil
}

// render 渲染任务列表为可读的字符串格式
// 这个函数将任务列表转换为用户友好的文本显示
// 包含任务状态、优先级和统计信息
func (tm *TodoManager) render() string {
	// 如果没有任务，返回提示信息
	if len(tm.items) == 0 {
		return "No todos."
	}

	// 构建显示行列表
	var lines []string
	done := 0 // 统计已完成的任务数量
	for _, item := range tm.items {
		// 根据任务状态设置不同的标记符号
		marker := "[ ]" // 默认为待完成状态
		switch item.Status {
		case "in_progress":
			marker = "[>]" // 进行中状态
		case "completed":
			marker = "[x]" // 已完成状态
			done++
		}
		// 格式化任务行：标记 + ID + 任务描述
		lines = append(lines, fmt.Sprintf("%s #%s: %s", marker, item.ID, item.Text))
	}

	// 添加完成统计信息
	lines = append(lines, fmt.Sprintf("\n(%d/%d completed)", done, len(tm.items)))
	// 将所有行连接成最终字符串
	return strings.Join(lines, "\n")
}

// 全局任务管理器实例
// 所有TODO操作都通过这个实例进行
var todoManager = &TodoManager{}

// safePath 解析路径并确保它在工作目录内
// 这是S02版本引入的安全特性，S03继续使用
// 防止路径遍历攻击，确保文件操作安全
func safePath(p string) (string, error) {
	// 获取工作目录的绝对路径
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

// runBash executes a shell command.
func runBash(command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workdir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	output := out.String()

	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}
	if err != nil {
		return fmt.Sprintf("Error: %v\nOutput:\n%s", err, output)
	}

	if len(output) > 50000 {
		output = output[:50000]
	}
	if output == "" {
		return "(no output)"
	}
	return output
}

// runRead reads a file.
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

// runWrite writes content to a file.
func runWrite(path, content string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	err = os.MkdirAll(filepath.Dir(fp), 0755)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	err = os.WriteFile(fp, []byte(content), 0644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// runEdit replaces text in a file.
func runEdit(path, oldText, newText string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	content, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if !strings.Contains(string(content), oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}
	newContent := strings.Replace(string(content), oldText, newText, 1)
	err = os.WriteFile(fp, []byte(newContent), 0644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Edited %s", path)
}

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
	"todo": func(args map[string]interface{}) string {
		items, _ := args["items"].([]interface{})
		result, err := todoManager.Update(items)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
}

// Structs for OpenAI API
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools"`
	ToolChoice  string    `json:"tool_choice"`
	Temperature float64   `json:"temperature"`
}

type ChatCompletionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func openAITools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "todo", Description: "Update task list. Track progress on multi-step tasks.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"id": map[string]string{"type": "string"}, "text": map[string]string{"type": "string"}, "status": map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}}}, "required": []string{"id", "text", "status"}}}}, "required": []string{"items"}}}},
	}
}

func chatCompletionsCreate(messages []Message) (Message, error) {
	header := http.Header{}
	authHeader := authHeaderName
	if authHeader == "" {
		authHeader = "Authorization"
	}
	header.Set(authHeader, "Bearer "+apiKey)
	header.Set("Content-Type", "application/json")

	payload := ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		Tools:       openAITools(),
		ToolChoice:  "auto",
		Temperature: 0,
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return Message{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header = header

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Message{}, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var response ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Message{}, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(response.Choices) == 0 {
		return Message{}, fmt.Errorf("no choices in response")
	}

	return response.Choices[0].Message, nil
}

func agentLoop(messages *[]Message) {
	roundsSinceTodo := 0
	for {
		if roundsSinceTodo >= 3 && len(*messages) > 0 {
			last := &(*messages)[len(*messages)-1]
			if last.Role == "user" {
				last.Content = "<reminder>Update your todos.</reminder>\n" + last.Content
			}
		}

		msg, err := chatCompletionsCreate(*messages)
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

		usedTodo := false
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
			if name == "todo" {
				usedTodo = true
			}
		}
		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}
	}
}

func main() {
	// 加载 .env 文件
	if err := loadEnv(); err != nil {
		fmt.Printf("Warning: Failed to load .env file: %v\n", err)
	}

	// 初始化配置变量
	initConfig()

	// 检查必需的环境变量
	if model == "" || baseURL == "" || apiKey == "" {
		fmt.Printf("Error: Ensure MODEL_ID, OPENAI_BASE_URL, and OPENAI_API_KEY environment variables are set.\n")
		return
	}

	history := []Message{
		{Role: "system", Content: system},
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("s03 >> ")
		if !scanner.Scan() {
			break
		}
		query := scanner.Text()
		if query == "q" || query == "exit" || query == "" {
			break
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

// =============================================================================
// # s08: Background Tasks (后台任务)

// `s01 > s02 > s03 > s04 > s05 > s06 | s07 > [ s08 ] s09 > s10 > s11 > s12`

// > *"慢操作丢后台, agent 继续想下一步"* -- 后台线程跑命令, 完成后注入通知。
// >
// > **Harness 层**: 后台执行 -- 模型继续思考, harness 负责等待。

// ## 问题

// 有些命令要跑好几分钟: `npm install`、`pytest`、`docker build`。阻塞式循环下模型只能干等。用户说 "装依赖, 顺便建个配置文件", 智能体却只能一个一个来。

// ## 解决方案

// ```
// Main thread                Background thread
// +-----------------+        +-----------------+
// | agent loop      |        | subprocess runs |
// | ...             |        | ...             |
// | [LLM call] <---+------- | enqueue(result) |
// |  ^drain queue   |        +-----------------+
// +-----------------+

// Timeline:
// Agent --[spawn A]--[spawn B]--[other work]----
//              |          |
//              v          v
//           [A runs]   [B runs]      (parallel)
//              |          |
//              +-- results injected before next LLM call --+
// ```

// ## 工作原理

// 1. BackgroundManager 用线程安全的通知队列追踪任务。
// 2. `run()` 启动守护线程, 立即返回。
// 3. 子进程完成后, 结果进入通知队列。
// 4. 每次 LLM 调用前排空通知队列。
// 循环保持单线程。只有子进程 I/O 被并行化。
// Run "sleep 5 && echo done" in the background, then create a file while it runs
// 1.`在后台运行“sleep 5 && echo done”，然后在运行时创建一个文件`
// 2. `启动 3 个后台任务：“sleep 2”、“sleep 4”、“sleep 6”。检查他们的状态。`
// 3. `在后台运行 pytest 并继续处理其他事情`
// =============================================================================

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
	"strconv"
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
	system = fmt.Sprintf("You are a coding agent at %s. Use background_run for long-running commands.", workdir)
}

// 后台任务相关结构体
type Task struct {
	Status  string // 任务状态 (running, completed, timeout, error)
	Result  string // 任务执行结果
	Command string // 执行的命令
}

type Notification struct {
	TaskID  string `json:"task_id"` // 任务唯一标识
	Status  string `json:"status"`  // 任务状态
	Command string `json:"command"` // 执行的命令
	Result  string `json:"result"`  // 执行结果
}

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

// Check 检查后台任务的状态。
// 如果提供了 taskID，则返回该任务的详细结果。
// 如果 taskID 为空，则返回所有任务的概览列表。
func (bm *BackgroundManager) Check(taskID string) string {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if taskID != "" {
		task, ok := bm.tasks[taskID]
		if !ok {
			return fmt.Sprintf("Error: Unknown task %s", taskID)
		}
		result := task.Result
		if result == "" {
			result = "(running)"
		}
		return fmt.Sprintf("[%s] %s\n%s", task.Status, task.Command, result)
	}

	var lines []string
	for id, task := range bm.tasks {
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", id, task.Status, task.Command))
	}
	if len(lines) == 0 {
		return "No background tasks."
	}
	return strings.Join(lines, "\n")
}

func (bm *BackgroundManager) DrainNotifications() []Notification {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	notifs := bm.notificationQueue
	bm.notificationQueue = []Notification{}
	return notifs
}

var bg = NewBackgroundManager()

// Other tool implementations (runBash, runRead, etc.) and OpenAI structs are the same as in s07.
// They are omitted here for brevity but should be included in the full file.

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
	"background_run": func(args map[string]interface{}) string {
		return bg.Run(args["command"].(string))
	},
	"check_background": func(args map[string]interface{}) string {
		taskID, _ := args["task_id"].(string)
		return bg.Check(taskID)
	},
}

// Placeholder for the full agent loop and other functions.
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
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}
type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
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

func openAITools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command (blocking).", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "background_run", Description: "Run command in background thread. Returns task_id immediately.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "check_background", Description: "Check background task status. Omit task_id to list all.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]string{"type": "string"}}}}},
	}
}

func chatCompletionsCreate(messages []Message, tools []Tool) (Message, error) {
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
		Tools:       tools,
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

	history := []Message{}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("s08 >> ")
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

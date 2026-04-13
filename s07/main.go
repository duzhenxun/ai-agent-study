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
	"sort"
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
	tasksDir       = filepath.Join(workdir, ".tasks")
	system         = ""
)
var tasks = NewTaskManager(tasksDir)

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
	"task_create": func(args map[string]interface{}) string {
		subject, _ := args["subject"].(string)
		description, _ := args["description"].(string)
		result, err := tasks.Create(subject, description)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
	"task_update": func(args map[string]interface{}) string {
		taskID := int(args["task_id"].(float64))
		var status *string
		if s, ok := args["status"].(string); ok {
			status = &s
		}
		var addBlockedBy, addBlocks []int
		if b, ok := args["addBlockedBy"].([]interface{}); ok {
			for _, v := range b {
				addBlockedBy = append(addBlockedBy, int(v.(float64)))
			}
		}
		if b, ok := args["addBlocks"].([]interface{}); ok {
			for _, v := range b {
				addBlocks = append(addBlocks, int(v.(float64)))
			}
		}
		result, err := tasks.Update(taskID, status, addBlockedBy, addBlocks)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
	"task_list": func(args map[string]interface{}) string {
		result, err := tasks.ListAll()
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
	"task_get": func(args map[string]interface{}) string {
		taskID := int(args["task_id"].(float64))
		result, err := tasks.Get(taskID)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return result
	},
}

// Task 表示一个任务及其属性
type Task struct {
	ID          int    `json:"id"`          // 任务ID，唯一标识符
	Subject     string `json:"subject"`     // 任务主题/标题
	Description string `json:"description"` // 任务详细描述
	Status      string `json:"status"`      // 任务状态：pending/in_progress/completed
	BlockedBy   []int  `json:"blockedBy"`   // 被哪些任务ID阻塞
	Blocks      []int  `json:"blocks"`      // 阻塞哪些任务ID
	Owner       string `json:"owner"`       // 任务负责人/所有者
}

// TaskManager manages tasks persisted as JSON files.
type TaskManager struct {
	dir    string
	mu     sync.Mutex
	nextID int
}

// NewTaskManager creates a new TaskManager
func NewTaskManager(tasksDir string) *TaskManager {
	_ = os.MkdirAll(tasksDir, 0755)
	tm := &TaskManager{dir: tasksDir}
	tm.nextID = tm.maxID() + 1
	return tm
}

func (tm *TaskManager) extractID(path string) int {
	idStr := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "task_"), ".json")
	id, _ := strconv.Atoi(idStr)
	return id
}

func (tm *TaskManager) maxID() int {
	files, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	maxID := 0
	for _, file := range files {
		idStr := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "task_"), ".json")
		id, err := strconv.Atoi(idStr)
		if err == nil && id > maxID {
			maxID = id
		}
	}
	return maxID
}

func (tm *TaskManager) load(taskID int) (*Task, error) {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", taskID))
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("task %d not found", taskID)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var task Task
	err = json.Unmarshal(content, &task)
	return &task, err
}

func (tm *TaskManager) save(task *Task) error {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", task.ID))
	content, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0644)
}

func (tm *TaskManager) Create(subject, description string) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Blocks:      []int{},
	}
	if err := tm.save(task); err != nil {
		return "", err
	}
	tm.nextID++
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result), nil
}

func (tm *TaskManager) Get(taskID int) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result), nil
}

func (tm *TaskManager) Update(taskID int, status *string, addBlockedBy, addBlocks []int) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return "", err
	}

	if status != nil {
		if *status != "pending" && *status != "in_progress" && *status != "completed" {
			return "", fmt.Errorf("invalid status: %s", *status)
		}
		task.Status = *status
		if *status == "completed" {
			tm.clearDependency(taskID)
		}
	}

	if addBlockedBy != nil {
		task.BlockedBy = append(task.BlockedBy, addBlockedBy...)
	}
	if addBlocks != nil {
		task.Blocks = append(task.Blocks, addBlocks...)
		for _, blockedID := range addBlocks {
			if blockedTask, err := tm.load(blockedID); err == nil {
				blockedTask.BlockedBy = append(blockedTask.BlockedBy, taskID)
				tm.save(blockedTask)
			}
		}
	}

	if err := tm.save(task); err != nil {
		return "", err
	}
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result), nil
}

func (tm *TaskManager) clearDependency(completedID int) {
	files, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	for _, file := range files {
		if task, err := tm.load(tm.extractID(file)); err == nil {
			var newBlockedBy []int
			for _, id := range task.BlockedBy {
				if id != completedID {
					newBlockedBy = append(newBlockedBy, id)
				}
			}
			task.BlockedBy = newBlockedBy
			tm.save(task)
		}
	}
}

func (tm *TaskManager) ListAll() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	files, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	var tasks []*Task
	for _, file := range files {
		if task, err := tm.load(tm.extractID(file)); err == nil {
			tasks = append(tasks, task)
		}
	}

	if len(tasks) == 0 {
		return "No tasks.", nil
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })

	var lines []string
	for _, t := range tasks {
		marker := "[?]"
		switch t.Status {
		case "pending":
			marker = "[ ]"
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
		}
		blocked := ""
		if len(t.BlockedBy) > 0 {
			blocked = fmt.Sprintf(" (blocked by: %v)", t.BlockedBy)
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s", marker, t.ID, t.Subject, blocked))
	}
	return strings.Join(lines, "\n"), nil
}

// safePath and other base tool implementations remain the same...

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
	system = fmt.Sprintf("You are a coding agent at %s. Use task tools to plan and track work.", workdir)
}

// Structs for OpenAI API and other functions (chatCompletionsCreate, agentLoop, main) are similar to s06 and are omitted for brevity
// but with the new task_* tools included in openAITools.
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

func openAITools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_create", Description: "Create a new task.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"subject": map[string]string{"type": "string"}, "description": map[string]string{"type": "string"}}, "required": []string{"subject"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_update", Description: "Update a task's status or dependencies.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}, "status": map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}}, "addBlockedBy": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer"}}, "addBlocks": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "integer"}}}, "required": []string{"task_id"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_list", Description: "List all tasks with status summary.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "task_get", Description: "Get full details of a task by ID.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}}, "required": []string{"task_id"}}}},
	}
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
		fmt.Print("s07 >> ")
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

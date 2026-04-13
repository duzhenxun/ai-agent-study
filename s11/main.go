// =============================================================================
// Idle cycle with task board polling, auto-claiming unclaimed tasks, and
// identity re-injection after context compression. Builds on s10's protocols.

//     Teammate lifecycle:
//     +-------+
//     | spawn |
//     +---+---+
//         |
//         v
//     +-------+  tool_use    +-------+
//     | WORK  | <----------- |  LLM  |
//     +---+---+              +-------+
//         |
//         | stop_reason != tool_use
//         v
//     +--------+
//     | IDLE   | poll every 5s for up to 60s
//     +---+----+
//         |
//         +---> check inbox -> message? -> resume WORK
//         |
//         +---> scan .tasks/ -> unclaimed? -> claim -> resume WORK
//         |
//         +---> timeout (60s) -> shutdown

//     Identity re-injection after compression:
//     messages = [identity_block, ...remaining...]
//     "You are 'coder', role: backend, team: my-team"

// Key insight: "The agent finds work itself."
//
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
	"sort"
	"strings"
	"sync"
	"time"
)

// 全局配置变量
var (
	model          string                             // 模型ID
	baseURL        string                             // API基础URL
	apiKey         string                             // API密钥
	authHeaderName string                             // 认证头名称
	workdir, _     = os.Getwd()                       // 当前工作目录
	teamDir        = filepath.Join(workdir, ".team")  // 团队数据目录
	inboxDir       = filepath.Join(teamDir, "inbox")  // 收件箱目录
	tasksDir       = filepath.Join(workdir, ".tasks") // 任务目录
	pollInterval   = 5 * time.Second                  // 轮询间隔
	idleTimeout    = 60 * time.Second                 // 空闲超时
	system         string                             // 系统提示词
	validMsgTypes  = map[string]bool{                 // 有效的消息类型映射表
		"message":                true, // 普通消息
		"broadcast":              true, // 广播消息
		"shutdown_request":       true, // 关闭请求
		"shutdown_response":      true,
		"plan_approval_response": true,
	}
)

// loadEnv 加载 .env 文件中的环境变量
// 这个函数从项目根目录的 .env 文件中读取环境变量并设置到系统中
// 只有当环境变量不存在时才会设置，避免覆盖系统已有的环境变量
func loadEnv() error {
	// 获取当前工作目录
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

	// 构建 .env 文件路径
	envPath := filepath.Join(projectRoot, ".env")

	// 检查 .env 文件是否存在
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		fmt.Printf("Warning: .env file not found at %s, using system environment variables only\n", envPath)
		return nil
	}

	// 打开并读取 .env 文件
	file, err := os.Open(envPath)
	if err != nil {
		return fmt.Errorf("failed to open .env file: %w", err)
	}
	defer file.Close()

	// 逐行读取 .env 文件
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释行（以 # 开头）
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析键值对（格式：KEY=VALUE）
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			// 移除可能存在的引号和反引号
			value = strings.Trim(value, "\"'`")
			value = strings.TrimSpace(value)

			// 只有当环境变量不存在时才设置
			if os.Getenv(key) == "" {
				os.Setenv(key, value)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading .env file: %w", err)
	}

	fmt.Printf("Loaded environment variables from .env file\n")
	return nil
}

// initConfig 初始化配置变量
// 这个函数从环境变量中读取配置参数并初始化全局变量
// 必须在 loadEnv() 函数之后调用
func initConfig() {
	model = os.Getenv("MODEL_ID")                    // 模型ID
	baseURL = os.Getenv("OPENAI_BASE_URL")           // API基础URL
	apiKey = os.Getenv("OPENAI_API_KEY")             // API密钥
	authHeaderName = os.Getenv("OPENAI_AUTH_HEADER") // 认证头名称
	// 初始化系统提示词，包含团队协作说明
	system = fmt.Sprintf("You are a team lead at %s. Teammates are autonomous.", workdir)
}

// Request trackers
var (
	shutdownRequests = make(map[string]map[string]string)
	planRequests     = make(map[string]map[string]string)
	trackerLock      sync.Mutex
	claimLock        sync.Mutex
)

type InboxMessage struct {
	Type      string      `json:"type"`
	From      string      `json:"from"`
	Content   string      `json:"content"`
	Timestamp float64     `json:"timestamp"`
	Extra     interface{} `json:"extra,omitempty"`
}

// Task board scanning
type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	BlockedBy   []int  `json:"blockedBy"`
	Blocks      []int  `json:"blocks"`
	Owner       string `json:"owner"`
}

func scanUnclaimedTasks() []Task {
	_ = os.MkdirAll(tasksDir, 0755)
	var unclaimed []Task
	files, _ := filepath.Glob(filepath.Join(tasksDir, "task_*.json"))
	sort.Strings(files)
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var task Task
		if err := json.Unmarshal(content, &task); err == nil {
			if task.Status == "pending" && task.Owner == "" && len(task.BlockedBy) == 0 {
				unclaimed = append(unclaimed, task)
			}
		}
	}
	return unclaimed
}

func claimTask(taskID int, owner string) string {
	claimLock.Lock()
	defer claimLock.Unlock()

	path := filepath.Join(tasksDir, fmt.Sprintf("task_%d.json", taskID))
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Sprintf("Error: Task %d not found", taskID)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading task: %v", err)
	}

	var task Task
	if err := json.Unmarshal(content, &task); err != nil {
		return fmt.Sprintf("Error parsing task: %v", err)
	}

	task.Owner = owner
	task.Status = "in_progress"

	newContent, _ := json.MarshalIndent(task, "", "  ")
	os.WriteFile(path, newContent, 0644)

	return fmt.Sprintf("Claimed task #%d for %s", taskID, owner)
}

// Identity re-injection
func makeIdentityBlock(name, role, teamName string) Message {
	return Message{
		Role:    "user",
		Content: fmt.Sprintf("<identity>You are '%s', role: %s, team: %s. Continue your work.</identity>", name, role, teamName),
	}
}

type MessageBus struct {
	dir string
	mu  sync.Mutex
}

func NewMessageBus(inboxDir string) *MessageBus {
	_ = os.MkdirAll(inboxDir, 0755)
	return &MessageBus{dir: inboxDir}
}

func (bus *MessageBus) Send(sender, to, content, msgType string, extra interface{}) (string, error) {
	if !validMsgTypes[msgType] {
		return "", fmt.Errorf("invalid message type: %s", msgType)
	}
	msg := InboxMessage{
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
		Extra:     extra,
	}
	inboxPath := filepath.Join(bus.dir, fmt.Sprintf("%s.jsonl", to))
	bus.mu.Lock()
	defer bus.mu.Unlock()

	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	json.NewEncoder(f).Encode(msg)
	return fmt.Sprintf("Sent %s to %s", msgType, to), nil
}

func (bus *MessageBus) ReadInbox(name string) ([]InboxMessage, error) {
	inboxPath := filepath.Join(bus.dir, fmt.Sprintf("%s.jsonl", name))
	bus.mu.Lock()
	defer bus.mu.Unlock()

	if _, err := os.Stat(inboxPath); os.IsNotExist(err) {
		return nil, nil
	}

	content, err := os.ReadFile(inboxPath)
	if err != nil {
		return nil, err
	}

	var messages []InboxMessage
	for _, line := range strings.Split(string(content), "\n") {
		if line != "" {
			var msg InboxMessage
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				messages = append(messages, msg)
			}
		}
	}

	// Drain the inbox
	_ = os.WriteFile(inboxPath, []byte(""), 0644)

	return messages, nil
}

func (bus *MessageBus) Broadcast(sender, content string, teammates []string) (string, error) {
	count := 0
	for _, name := range teammates {
		if name != sender {
			bus.Send(sender, name, content, "broadcast", nil)
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count), nil
}

var bus = NewMessageBus(inboxDir)

// Autonomous TeammateManager

// Autonomous TeammateManager
type Teammate struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type TeamConfig struct {
	TeamName string     `json:"team_name"`
	Members  []Teammate `json:"members"`
}

type TeammateManager struct {
	dir        string
	configPath string
	config     *TeamConfig
	threads    map[string]chan bool
	mu         sync.Mutex
}

func NewTeammateManager(teamDir string) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0755)
	tm := &TeammateManager{
		dir:        teamDir,
		configPath: filepath.Join(teamDir, "config.json"),
		threads:    make(map[string]chan bool),
	}
	tm.loadConfig()
	return tm
}

func (tm *TeammateManager) loadConfig() {
	if _, err := os.Stat(tm.configPath); os.IsNotExist(err) {
		tm.config = &TeamConfig{TeamName: "default", Members: []Teammate{}}
		tm.saveConfig()
		return
	}
	content, _ := os.ReadFile(tm.configPath)
	json.Unmarshal(content, &tm.config)
}

func (tm *TeammateManager) saveConfig() {
	content, _ := json.MarshalIndent(tm.config, "", "  ")
	os.WriteFile(tm.configPath, content, 0644)
}

func (tm *TeammateManager) findMember(name string) *Teammate {
	for i, m := range tm.config.Members {
		if m.Name == name {
			return &tm.config.Members[i]
		}
	}
	return nil
}

func (tm *TeammateManager) setStatus(name, status string) {
	member := tm.findMember(name)
	if member != nil {
		member.Status = status
		tm.saveConfig()
	}
}

func (tm *TeammateManager) Spawn(name, role, prompt string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	member := tm.findMember(name)
	if member != nil {
		if member.Status != "idle" && member.Status != "shutdown" {
			return fmt.Sprintf("Error: '%s' is currently %s", name, member.Status)
		}
		member.Status = "working"
		member.Role = role
	} else {
		tm.config.Members = append(tm.config.Members, Teammate{Name: name, Role: role, Status: "working"})
	}
	tm.saveConfig()

	go tm.loop(name, role, prompt)

	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

func (tm *TeammateManager) loop(name, role, prompt string) {
	teamName := tm.config.TeamName
	sysPrompt := fmt.Sprintf("You are '%s', role: %s, team: %s, at %s. Use idle tool when you have no more work.", name, role, teamName, workdir)
	messages := []Message{{Role: "system", Content: sysPrompt}, {Role: "user", Content: prompt}}

	for {
		// WORK PHASE
		for i := 0; i < 50; i++ {
			inbox, _ := bus.ReadInbox(name)
			for _, msg := range inbox {
				if msg.Type == "shutdown_request" {
					tm.setStatus(name, "shutdown")
					return
				}
				content, _ := json.Marshal(msg)
				messages = append(messages, Message{Role: "user", Content: string(content)})
			}

			msg, err := chatCompletionsCreate(messages, teammateTools())
			if err != nil {
				tm.setStatus(name, "idle")
				return
			}

			messages = append(messages, msg)

			if len(msg.ToolCalls) == 0 {
				break
			}

			idleRequested := false
			for _, tc := range msg.ToolCalls {
				toolName := tc.Function.Name
				var args map[string]interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &args)

				var output string
				if toolName == "idle" {
					idleRequested = true
					output = "Entering idle phase. Will poll for new tasks."
				} else {
					output = tm.exec(name, toolName, args)
				}
				fmt.Printf("  [%s] %s: %s\n", name, toolName, output)
				messages = append(messages, Message{Role: "tool", ToolCallID: tc.ID, Content: output})
			}
			if idleRequested {
				break
			}
		}

		// IDLE PHASE
		tm.setStatus(name, "idle")
		resume := false
		timeout := time.After(idleTimeout)
		for {
			select {
			case <-timeout:
				tm.setStatus(name, "shutdown")
				return
			default:
			}

			inbox, _ := bus.ReadInbox(name)
			if len(inbox) > 0 {
				for _, msg := range inbox {
					if msg.Type == "shutdown_request" {
						tm.setStatus(name, "shutdown")
						return
					}
					content, _ := json.Marshal(msg)
					messages = append(messages, Message{Role: "user", Content: string(content)})
				}
				resume = true
				break
			}

			unclaimed := scanUnclaimedTasks()
			if len(unclaimed) > 0 {
				task := unclaimed[0]
				claimTask(task.ID, name)
				taskPrompt := fmt.Sprintf("<auto-claimed>Task #%d: %s\n%s</auto-claimed>", task.ID, task.Subject, task.Description)
				if len(messages) <= 3 {
					messages = append([]Message{makeIdentityBlock(name, role, teamName)}, messages...)
					messages = append(messages, Message{Role: "assistant", Content: fmt.Sprintf("I am %s. Continuing.", name)})
				}
				messages = append(messages, Message{Role: "user", Content: taskPrompt})
				messages = append(messages, Message{Role: "assistant", Content: fmt.Sprintf("Claimed task #%d. Working on it.", task.ID)})
				resume = true
				break
			}

			time.Sleep(pollInterval)
		}

		if !resume {
			tm.setStatus(name, "shutdown")
			return
		}
		tm.setStatus(name, "working")
	}
}

func (tm *TeammateManager) exec(sender, toolName string, args map[string]interface{}) string {
	switch toolName {
	case "bash":
		return runBash(args["command"].(string))
	case "read_file":
		var limit *int
		if l, ok := args["limit"].(float64); ok {
			val := int(l)
			limit = &val
		}
		return runRead(args["path"].(string), limit)
	case "write_file":
		return runWrite(args["path"].(string), args["content"].(string))
	case "edit_file":
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	case "send_message":
		to, _ := args["to"].(string)
		content, _ := args["content"].(string)
		msgType, _ := args["msg_type"].(string)
		if msgType == "" {
			msgType = "message"
		}
		result, _ := bus.Send(sender, to, content, msgType, nil)
		return result
	case "read_inbox":
		inbox, _ := bus.ReadInbox(sender)
		result, _ := json.MarshalIndent(inbox, "", "  ")
		return string(result)
	case "shutdown_response":
		reqID := args["request_id"].(string)
		approve := args["approve"].(bool)
		trackerLock.Lock()
		if req, ok := shutdownRequests[reqID]; ok {
			if approve {
				req["status"] = "approved"
			} else {
				req["status"] = "rejected"
			}
		}
		trackerLock.Unlock()
		bus.Send(sender, "lead", args["reason"].(string), "shutdown_response", map[string]interface{}{"request_id": reqID, "approve": approve})
		if approve {
			return "Shutdown approved"
		}
		return "Shutdown rejected"
	case "plan_approval":
		planText := args["plan"].(string)
		// 使用时间戳生成唯一ID，避免外部依赖
		reqID := fmt.Sprintf("%d", time.Now().UnixNano())[:8]
		trackerLock.Lock()
		planRequests[reqID] = map[string]string{"from": sender, "plan": planText, "status": "pending"}
		trackerLock.Unlock()
		bus.Send(sender, "lead", planText, "plan_approval_response", map[string]interface{}{"request_id": reqID, "plan": planText})
		return fmt.Sprintf("Plan submitted (request_id=%s). Waiting for lead approval.", reqID)
	case "claim_task":
		return claimTask(int(args["task_id"].(float64)), sender)
	default:
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}
}

func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if len(tm.config.Members) == 0 {
		return "No teammates."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Team: %s", tm.config.TeamName))
	for _, m := range tm.config.Members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}
	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var names []string
	for _, m := range tm.config.Members {
		names = append(names, m.Name)
	}
	return names
}

var team = NewTeammateManager(teamDir)

// ... (MessageBus and other structs from s10 are the same)

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

func teammateTools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "send_message", Description: "Send message to a teammate.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"to": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}, "msg_type": map[string]interface{}{"type": "string", "enum": []string{"message", "broadcast", "shutdown_request", "shutdown_response", "plan_approval_response"}}}, "required": []string{"to", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_inbox", Description: "Read and drain your inbox.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "shutdown_response", Description: "Respond to a shutdown request.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"request_id": map[string]string{"type": "string"}, "approve": map[string]interface{}{"type": "boolean"}, "reason": map[string]string{"type": "string"}}, "required": []string{"request_id", "approve"}}}},
		{Type: "function", Function: FunctionDef{Name: "plan_approval", Description: "Submit a plan for lead approval.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"plan": map[string]string{"type": "string"}}, "required": []string{"plan"}}}},
		{Type: "function", Function: FunctionDef{Name: "idle", Description: "Signal that you have no more work.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "claim_task", Description: "Claim a task from the task board by ID.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}}, "required": []string{"task_id"}}}},
	}
}

func leadTools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "spawn_teammate", Description: "Spawn a new teammate.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}, "role": map[string]string{"type": "string"}, "prompt": map[string]string{"type": "string"}}, "required": []string{"name", "role", "prompt"}}}},
		{Type: "function", Function: FunctionDef{Name: "list_teammates", Description: "List all teammates.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "send_message", Description: "Send message to a teammate.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"to": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}, "msg_type": map[string]interface{}{"type": "string", "enum": []string{"message", "broadcast", "shutdown_request", "shutdown_response", "plan_approval_response"}}}, "required": []string{"to", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_inbox", Description: "Read and drain your inbox.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "broadcast", Description: "Broadcast message to all teammates.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content": map[string]string{"type": "string"}}, "required": []string{"content"}}}},
		{Type: "function", Function: FunctionDef{Name: "create_task", Description: "Create a new task.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"subject": map[string]string{"type": "string"}, "description": map[string]string{"type": "string"}}, "required": []string{"subject", "description"}}}},
	}
}

func agentLoop(messages *[]Message) {
	// Add system message if not present
	if len(*messages) == 0 {
		*messages = append(*messages, Message{Role: "system", Content: system})
	}

	for {
		msg, err := chatCompletionsCreate(*messages, leadTools())
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

			var output string
			switch name {
			case "bash":
				output = runBash(args["command"].(string))
			case "read_file":
				var limit *int
				if l, ok := args["limit"].(float64); ok {
					val := int(l)
					limit = &val
				}
				output = runRead(args["path"].(string), limit)
			case "write_file":
				output = runWrite(args["path"].(string), args["content"].(string))
			case "edit_file":
				output = runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
			case "spawn_teammate":
				output = team.Spawn(args["name"].(string), args["role"].(string), args["prompt"].(string))
			case "list_teammates":
				output = team.ListAll()
			case "send_message":
				to, _ := args["to"].(string)
				content, _ := args["content"].(string)
				msgType, _ := args["msg_type"].(string)
				if msgType == "" {
					msgType = "message"
				}
				result, _ := bus.Send("lead", to, content, msgType, nil)
				output = result
			case "read_inbox":
				inbox, _ := bus.ReadInbox("lead")
				result, _ := json.MarshalIndent(inbox, "", "  ")
				output = string(result)
			case "broadcast":
				content := args["content"].(string)
				teammates := team.MemberNames()
				result, _ := bus.Broadcast("lead", content, teammates)
				output = result
			case "create_task":
				subject := args["subject"].(string)
				description := args["description"].(string)
				taskID := createTask(subject, description)
				output = fmt.Sprintf("Created task #%d: %s", taskID, subject)
			default:
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

func createTask(subject, description string) int {
	_ = os.MkdirAll(tasksDir, 0755)

	// Find next task ID
	files, _ := filepath.Glob(filepath.Join(tasksDir, "task_*.json"))
	maxID := 0
	for _, f := range files {
		var num int
		fmt.Sscanf(filepath.Base(f), "task_%d.json", &num)
		if num > maxID {
			maxID = num
		}
	}

	taskID := maxID + 1
	task := Task{
		ID:          taskID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		BlockedBy:   []int{},
		Blocks:      []int{},
		Owner:       "",
	}

	content, _ := json.MarshalIndent(task, "", "  ")
	os.WriteFile(filepath.Join(tasksDir, fmt.Sprintf("task_%d.json", taskID)), content, 0644)

	return taskID
}

func getValidMsgTypes() []string {
	keys := make([]string, 0, len(validMsgTypes))
	for k := range validMsgTypes {
		keys = append(keys, k)
	}
	return keys
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
		fmt.Print("\033[36ms11 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := scanner.Text()
		if strings.TrimSpace(strings.ToLower(query)) == "q" || strings.TrimSpace(strings.ToLower(query)) == "exit" || query == "" {
			break
		}

		if strings.TrimSpace(query) == "/team" {
			fmt.Println(team.ListAll())
			continue
		}
		if strings.TrimSpace(query) == "/inbox" {
			inbox, _ := bus.ReadInbox("lead")
			result, _ := json.MarshalIndent(inbox, "", "  ")
			fmt.Println(string(result))
			continue
		}
		if strings.TrimSpace(query) == "/tasks" {
			_ = os.MkdirAll(tasksDir, 0755)
			files, _ := filepath.Glob(filepath.Join(tasksDir, "task_*.json"))
			for _, f := range files {
				content, _ := os.ReadFile(f)
				var task Task
				json.Unmarshal(content, &task)
				marker := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}[task.Status]
				if marker == "" {
					marker = "[?]"
				}
				owner := ""
				if task.Owner != "" {
					owner = fmt.Sprintf(" @%s", task.Owner)
				}
				fmt.Printf("  %s #%d: %s%s\n", marker, task.ID, task.Subject, owner)
			}
			continue
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

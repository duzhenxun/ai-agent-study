// =============================================================================
// # s09: Agent Teams (智能体团队)

// `s01 > s02 > s03 > s04 > s05 > s06 | s07 > s08 > [ s09 ] s10 > s11 > s12`

// > *"任务太大一个人干不完, 要能分给队友"* -- 持久化队友 + JSONL 邮箱。
// >
// > **Harness 层**: 团队邮箱 -- 多个模型, 通过文件协调。

// ## 问题

// 子智能体 (s04) 是一次性的: 生成、干活、返回摘要、消亡。没有身份, 没有跨调用的记忆。后台任务 (s08) 能跑 shell 命令, 但做不了 LLM 引导的决策。

// 真正的团队协作需要三样东西: (1) 能跨多轮对话存活的持久智能体, (2) 身份和生命周期管理, (3) 智能体之间的通信通道。

// ## 解决方案

// ```
// Teammate lifecycle:
//   spawn -> WORKING -> IDLE -> WORKING -> ... -> SHUTDOWN

// Communication:
//   .team/
//     config.json           <- team roster + statuses
//     inbox/
//       alice.jsonl         <- append-only, drain-on-read
//       bob.jsonl
//       lead.jsonl

//               +--------+    send("alice","bob","...")    +--------+
//               | alice  | -----------------------------> |  bob   |
//               | loop   |    bob.jsonl << {json_line}    |  loop  |
//               +--------+                                +--------+
//                    ^                                         |
//                    |        BUS.read_inbox("alice")          |
//                    +---- alice.jsonl -> read + drain ---------+
// ```
// Key insight: "Teammates that can talk to each other."
// 1. `生成 alice（编码员）和 bob（测试员）。让爱丽丝给鲍勃发一条消息。
// 2.`向所有队友广播“状态更新：第一阶段完成”`
// 3.“检查潜在客户收件箱中是否有任何消息”
// """
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
	"strings"
	"sync"
	"time"
)

// 全局配置变量
var (
	model          string                            // 模型ID
	baseURL        string                            // API基础URL
	apiKey         string                            // API密钥
	authHeaderName string                            // 认证头名称
	workdir, _     = os.Getwd()                      // 当前工作目录
	teamDir        = filepath.Join(workdir, ".team") // 团队数据目录
	inboxDir       = filepath.Join(teamDir, "inbox") // 收件箱目录
	system         string                            // 系统提示词
	validMsgTypes  = map[string]bool{                // 有效的消息类型映射表
		"message":                true, // 普通消息
		"broadcast":              true, // 广播消息
		"shutdown_request":       true, // 关闭请求
		"shutdown_response":      true, // 关闭响应
		"plan_approval_response": true, // 计划审批响应
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
	fmt.Printf("model: %s, baseURL: %s, apiKey: %s, authHeaderName: %s\n", model, baseURL, apiKey, authHeaderName)
	// 初始化系统提示词，包含团队协作说明
	system = fmt.Sprintf("You are a team lead at %s. Spawn teammates and communicate via inboxes.", workdir)
}

// Message struct for inbox communication
type InboxMessage struct {
	Type      string      `json:"type"`
	From      string      `json:"from"`
	Content   string      `json:"content"`
	Timestamp float64     `json:"timestamp"`
	Extra     interface{} `json:"extra,omitempty"`
}

// MessageBus 消息总线
// 负责处理团队成员之间的收件箱通信
type MessageBus struct {
	dir string     // 收件箱目录路径
	mu  sync.Mutex // 互斥锁，确保并发安全
}

// NewMessageBus 创建新的消息总线
// 初始化消息总线并创建必要的目录结构
func NewMessageBus(inboxDir string) *MessageBus {
	// 创建收件箱目录
	_ = os.MkdirAll(inboxDir, 0755)
	return &MessageBus{dir: inboxDir}
}

// Send 发送消息到指定收件箱
// 将消息写入目标收件箱的JSONL文件中
func (bus *MessageBus) Send(sender, to, content, msgType string, extra interface{}) (string, error) {
	// 验证消息类型是否有效
	if !validMsgTypes[msgType] {
		return "", fmt.Errorf("invalid message type: %s", msgType)
	}
	// 创建消息对象
	msg := InboxMessage{
		Type:      msgType,
		From:      sender,
		Content:   content,
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
		Extra:     extra,
	}
	// 构建收件箱文件路径
	inboxPath := filepath.Join(bus.dir, fmt.Sprintf("%s.jsonl", to))
	bus.mu.Lock()
	defer bus.mu.Unlock()

	// 以追加模式打开收件箱文件
	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// 将消息编码为JSON并写入文件
	json.NewEncoder(f).Encode(msg)
	return fmt.Sprintf("Sent %s to %s", msgType, to), nil
}

// ReadInbox 读取指定用户的收件箱
// 返回所有未读消息并清空收件箱
func (bus *MessageBus) ReadInbox(name string) ([]InboxMessage, error) {
	// 构建收件箱文件路径
	inboxPath := filepath.Join(bus.dir, fmt.Sprintf("%s.jsonl", name))
	bus.mu.Lock()
	defer bus.mu.Unlock()

	// 检查收件箱文件是否存在
	if _, err := os.Stat(inboxPath); os.IsNotExist(err) {
		return nil, nil // 没有收件箱则返回空消息列表
	}

	// 读取收件箱文件内容
	content, err := os.ReadFile(inboxPath)
	if err != nil {
		return nil, err
	}

	// 解析JSONL格式的消息文件
	var messages []InboxMessage
	for _, line := range strings.Split(string(content), "\n") {
		if line != "" {
			var msg InboxMessage
			// 解析每一行的JSON消息
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				messages = append(messages, msg)
			}
		}
	}

	// 清空收件箱文件，避免重复读取
	_ = os.WriteFile(inboxPath, []byte(""), 0644)

	return messages, nil
}

// Broadcast 广播消息给所有团队成员
// 除了发送者之外的所有成员都会收到广播消息
func (bus *MessageBus) Broadcast(sender, content string, teammates []string) (string, error) {
	count := 0 // 记录广播成功的成员数量
	for _, name := range teammates {
		// 跳过发送者自己
		if name != sender {
			bus.Send(sender, name, content, "broadcast", nil)
			count++
		}
	}
	return fmt.Sprintf("Broadcast to %d teammates", count), nil
}

// 全局消息总线实例
var bus = NewMessageBus(inboxDir)

// 团队成员和团队管理器结构体

// Teammate 团队成员结构体
// 定义团队成员的基本信息和状态
type Teammate struct {
	Name   string `json:"name"`   // 成员名称
	Role   string `json:"role"`   // 成员角色
	Status string `json:"status"` // 成员状态
}

// TeamConfig 团队配置结构体
type TeamConfig struct {
	TeamName string     `json:"team_name"` // 团队名称
	Members  []Teammate `json:"members"`   // 团队成员列表
}

// TeammateManager 团队成员管理器
// 负责管理团队成员的创建、通信和生命周期
type TeammateManager struct {
	dir        string               // 团队目录路径
	configPath string               // 配置文件路径
	config     *TeamConfig          // 团队配置
	threads    map[string]chan bool // 成员线程通信通道
	mu         sync.Mutex           // 互斥锁，确保并发安全
}

// NewTeammateManager 创建新的团队成员管理器
// 初始化管理器并加载团队配置
func NewTeammateManager(teamDir string) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0755)
	tm := &TeammateManager{
		dir:        teamDir,
		configPath: filepath.Join(teamDir, "config.json"),
		threads:    make(map[string]chan bool),
	}
	// 加载团队配置
	tm.loadConfig()
	return tm
}

// loadConfig 加载团队配置文件
// 从JSON文件中读取团队配置信息
func (tm *TeammateManager) loadConfig() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, err := os.Stat(tm.configPath); os.IsNotExist(err) {
		// 如果配置文件不存在，创建默认配置
		tm.config = &TeamConfig{TeamName: "default", Members: []Teammate{}}
		tm.saveConfig()
		return
	}
	content, _ := os.ReadFile(tm.configPath)
	json.Unmarshal(content, &tm.config)
}

// saveConfig 保存团队配置文件
// 将团队配置信息写入JSON文件
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

	stopChan := make(chan bool)
	tm.threads[name] = stopChan

	go tm.teammateLoop(name, role, prompt, stopChan)

	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

func (tm *TeammateManager) teammateLoop(name, role, prompt string, stopChan chan bool) {
	sysPrompt := fmt.Sprintf("You are '%s', role: %s, at %s. Use send_message to communicate. Complete your task.", name, role, workdir)
	messages := []Message{{Role: "user", Content: prompt}}
	messages = append([]Message{{Role: "system", Content: sysPrompt}}, messages...)

	for i := 0; i < 50; i++ {
		select {
		case <-stopChan:
			return
		default:
		}

		inbox, _ := bus.ReadInbox(name)
		for _, msg := range inbox {
			content, _ := json.Marshal(msg)
			messages = append(messages, Message{Role: "user", Content: string(content)})
		}

		msg, err := chatCompletionsCreate(messages, teammateTools())
		if err != nil {
			log.Printf("[%s] Error calling API: %v", name, err)
			break
		}

		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			break
		}

		for _, tc := range msg.ToolCalls {
			toolName := tc.Function.Name
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)

			output := tm.exec(name, toolName, args)
			fmt.Printf("  [%s] %s: %s\n", name, toolName, output)
			messages = append(messages, Message{Role: "tool", ToolCallID: tc.ID, Content: output})
		}
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()
	member := tm.findMember(name)
	if member != nil && member.Status != "shutdown" {
		member.Status = "idle"
		tm.saveConfig()
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

// Base tool implementations and OpenAI structs are omitted for brevity.

// Tool handlers for the lead agent
var toolHandlers = map[string]interface{}{
	"bash": func(args map[string]interface{}) string { return runBash(args["command"].(string)) },
	"read_file": func(args map[string]interface{}) string {
		var limit *int
		if l, ok := args["limit"].(float64); ok {
			val := int(l)
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
	"spawn_teammate": func(args map[string]interface{}) string {
		return team.Spawn(args["name"].(string), args["role"].(string), args["prompt"].(string))
	},
	"list_teammates": func(args map[string]interface{}) string { return team.ListAll() },
	"send_message": func(args map[string]interface{}) string {
		to, _ := args["to"].(string)
		content, _ := args["content"].(string)
		msgType, _ := args["msg_type"].(string)
		if msgType == "" {
			msgType = "message"
		}
		result, _ := bus.Send("lead", to, content, msgType, nil)
		return result
	},
	"read_inbox": func(args map[string]interface{}) string {
		inbox, _ := bus.ReadInbox("lead")
		result, _ := json.MarshalIndent(inbox, "", "  ")
		return string(result)
	},
	"broadcast": func(args map[string]interface{}) string {
		content, _ := args["content"].(string)
		result, _ := bus.Broadcast("lead", content, team.MemberNames())
		return result
	},
}

func agentLoop(messages *[]Message) {
	for {
		inbox, _ := bus.ReadInbox("lead")
		if len(inbox) > 0 {
			content, _ := json.MarshalIndent(inbox, "", "  ")
			*messages = append(*messages, Message{Role: "user", Content: fmt.Sprintf("<inbox>%s</inbox>", string(content))})
			*messages = append(*messages, Message{Role: "assistant", Content: "Noted inbox messages."})
		}

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
			// 没有工具调用时，等待用户输入
			break
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
		// 处理完工具调用后继续循环，让 AI 处理工具执行结果
	}
}

// NOTE: The following are placeholder functions and structs.
// You should replace them with the full implementations from s08.

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

func leadTools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "spawn_teammate", Description: "Spawn a persistent teammate that runs in its own thread.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}, "role": map[string]string{"type": "string"}, "prompt": map[string]string{"type": "string"}}, "required": []string{"name", "role", "prompt"}}}},
		{Type: "function", Function: FunctionDef{Name: "list_teammates", Description: "List all teammates with name, role, status.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "send_message", Description: "Send a message to a teammate's inbox.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"to": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}, "msg_type": map[string]interface{}{"type": "string", "enum": []string{"message", "broadcast", "shutdown_request", "shutdown_response", "plan_approval_response"}}}, "required": []string{"to", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_inbox", Description: "Read and drain the lead's inbox.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "broadcast", Description: "Send a message to all teammates.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content": map[string]string{"type": "string"}}, "required": []string{"content"}}}},
	}
}

func getValidMsgTypes() []string {
	keys := make([]string, 0, len(validMsgTypes))
	for k := range validMsgTypes {
		keys = append(keys, k)
	}
	return keys
}
func runBash(command string) string {
	// Check for dangerous commands
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}

	// Create and execute command with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workdir

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run command
	err := cmd.Run()

	// Get combined output
	out := stdout.String() + stderr.String()
	out = strings.TrimSpace(out)

	// Handle timeout
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}

	// Handle other errors
	if err != nil {
		return fmt.Sprintf("Error: %v\n%s", err, out)
	}

	// Return output or "(no output)" if empty
	if out == "" {
		return "(no output)"
	}

	// Limit output to 50000 characters
	if len(out) > 50000 {
		return out[:50000]
	}

	return out
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
		fmt.Print("\033[36ms09 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := scanner.Text()
		if query == "q" || query == "exit" || query == "" {
			break
		}

		if query == "/team" {
			fmt.Println(team.ListAll())
			continue
		}
		if query == "/inbox" {
			inbox, _ := bus.ReadInbox("lead")
			result, _ := json.MarshalIndent(inbox, "", "  ")
			fmt.Println(string(result))
			continue
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

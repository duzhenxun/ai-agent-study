// AI Agent 代码助手 - S02版本
// 在S01基础上增加了安全路径检查和工具系统
package main

import (
	"bufio"         // 缓冲I/O操作
	"bytes"         // 字节切片操作
	"context"       // 上下文管理，用于控制请求超时
	"encoding/json" // JSON编码解码
	"fmt"           // 格式化输入输出
	"io"            // 基础I/O接口
	"log"           // 日志记录
	"net/http"      // HTTP客户端
	"os"            // 操作系统接口
	"os/exec"       // 执行外部命令
	"path/filepath" // 路径处理工具
	"strings"       // 字符串操作
	"time"          // 时间处理
)

// 全局配置变量
// 这些变量存储从环境变量中读取的API配置信息
var (
	model          string       // OpenAI模型ID
	baseURL        string       // OpenAI API基础URL
	apiKey         string       // OpenAI API密钥
	authHeaderName string       // 认证头名称
	workdir, _     = os.Getwd() // 当前工作目录
	system         = ""         // 系统提示词
)

// loadEnv 从.env文件中加载环境变量
// 这个函数会读取项目根目录下的.env文件，解析其中的配置项
// 相比S01，增加了更完善的错误处理和路径检查
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

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading .env file: %w", err)
	}

	fmt.Printf("Loaded environment variables from .env file\n")
	return nil
}

// initConfig 初始化配置变量
// 这个函数在加载.env文件后被调用，用于将环境变量赋值给全局变量
// 同时设置系统提示词，告诉AI它的角色和工作环境
func initConfig() {
	model = os.Getenv("MODEL_ID")                    // 获取OpenAI模型ID
	baseURL = os.Getenv("OPENAI_BASE_URL")           // 获取API基础URL
	apiKey = os.Getenv("OPENAI_API_KEY")             // 获取API密钥
	authHeaderName = os.Getenv("OPENAI_AUTH_HEADER") // 获取认证头名称
	system = fmt.Sprintf("You are a coding agent at %s. Use tools to solve tasks. Act, don't explain.", workdir)
}

// safePath 解析路径并确保它在工作目录内
// 这是S02版本的重要安全特性，防止路径遍历攻击
// 确保所有文件操作都在允许的工作目录范围内进行
func safePath(p string) (string, error) {
	// 获取工作目录的绝对路径
	workdirAbs, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}

	// 获取目标路径的绝对路径
	absPath, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	// 检查目标路径是否在工作目录内
	// 这是防止路径遍历攻击的关键检查
	if !strings.HasPrefix(absPath, workdirAbs) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}

	return absPath, nil
}

// runBash 执行shell命令
// 相比S01，增加了危险命令检查和超时控制
func runBash(command string) string {
	// 定义危险命令列表，防止执行破坏性操作
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}
	// 设置120秒超时，防止命令无限期运行
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
		return fmt.Sprintf("Error: %v，Output:%s", err, output)
	}

	if len(output) > 50000 {
		output = output[:50000]
	}
	if output == "" {
		return "(no output)"
	}
	return output
}

// runRead 读取文件内容
// 这个函数使用safePath确保文件路径安全，然后读取文件内容
// 支持可选的行数限制参数，防止读取过大的文件
func runRead(path string, limit *int) string {
	// 使用safePath确保文件路径在允许的工作目录内
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 读取文件内容
	content, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 将内容转换为字符串
	contentStr := string(content)
	// 如果指定了行数限制，截取内容
	if limit != nil && *limit > 0 {
		lines := strings.Split(contentStr, "\n")
		if len(lines) > *limit {
			contentStr = strings.Join(lines[:*limit], "\n")
			contentStr += fmt.Sprintf("\n\n... (truncated, showing first %d lines)", *limit)
		}
	}
	return contentStr
}

// runWrite 写入内容到文件
// 这个函数使用safePath确保文件路径安全，然后将内容写入文件
func runWrite(path, content string) string {
	// 使用safePath确保文件路径在允许的工作目录内
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 创建所有必要的父目录（权限0755）
	err = os.MkdirAll(filepath.Dir(fp), 0755)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 将内容写入文件（权限0644）
	err = os.WriteFile(fp, []byte(content), 0644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 返回写入成功的消息
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// runEdit 替换文件中的文本
// 这个函数在文件中查找指定的旧文本，并替换为新文本
// 只替换第一个匹配项，确保精确的文本替换
func runEdit(path, oldText, newText string) string {
	// 使用safePath确保文件路径安全
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 读取文件内容
	content, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 检查旧文本是否存在于文件中
	if !strings.Contains(string(content), oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}
	// 执行文本替换（只替换第一个匹配项）
	newContent := strings.Replace(string(content), oldText, newText, 1)
	err = os.WriteFile(fp, []byte(newContent), 0644)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	// 返回编辑成功的消息
	return fmt.Sprintf("Edited %s", path)
}

// Tool Handlers Map 工具处理器映射
// 这个映射表定义了所有可用工具及其对应的处理函数
// 每个工具都有一个名称和一个处理函数，接收参数并返回结果
var toolHandlers = map[string]interface{}{
	"bash": func(args map[string]interface{}) string {
		// bash工具：执行shell命令
		return runBash(args["command"].(string))
	},
	"read_file": func(args map[string]interface{}) string {
		// read_file工具：读取文件内容
		var limit *int
		// 检查是否指定了行数限制参数
		if l, ok := args["limit"]; ok {
			// JSON数字被解码为float64类型，需要转换为int
			val := int(l.(float64))
			limit = &val
		}
		return runRead(args["path"].(string), limit)
	},
	"write_file": func(args map[string]interface{}) string {
		// write_file工具：写入内容到文件
		return runWrite(args["path"].(string), args["content"].(string))
	},
	"edit_file": func(args map[string]interface{}) string {
		// edit_file工具：替换文件中的文本
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	},
}

// OpenAI API 相关结构体定义
// 这些结构体用于序列化和反序列化OpenAI API的JSON数据

// Tool 表示一个可调用的工具
type Tool struct {
	Type     string      `json:"type"`     // 工具类型，通常为"function"
	Function FunctionDef `json:"function"` // 工具函数定义
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
	for {
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

	history := []Message{
		{Role: "system", Content: system},
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("s02 >> ")
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

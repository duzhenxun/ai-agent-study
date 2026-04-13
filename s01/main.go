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
	system = fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", workdir)
}

// runBash 执行shell命令
func runBash(command string) string {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return "Error: Dangerous command blocked"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = workdir

	outBytes, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "Error: Timeout (120s)"
	}

	out := strings.TrimSpace(string(outBytes))
	if err != nil && out == "" {
		out = err.Error()
	}
	if out == "" {
		out = "(no output)"
	}

	const maxLen = 50000
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
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
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "bash",
				Description: "Run a shell command.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]string{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		},
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
			var args struct {
				Command string `json:"command"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)

			command := strings.TrimSpace(args.Command)
			fmt.Printf("\n\033[33m$ %s\033[0m\n", command)
			output := runBash(command)

			// 打印部分输出以供调试
			if len(output) > 200 {
				fmt.Printf("%s...\n", output[:200])
			} else {
				fmt.Println(output)
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

	fmt.Printf("Using API Key: %s\n", apiKey)

	history := []Message{
		{Role: "system", Content: system},
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms01 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())

		if query == "" {
			continue
		}

		lower := strings.ToLower(query)
		if lower == "q" || lower == "exit" {
			break
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

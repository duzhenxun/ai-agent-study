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
	"regexp"
	"strings"
	"time"
)

// Global variables for configuration
// 全局配置变量
var (
	workdir, _ = os.Getwd()                       // 当前工作目录
	skillsDir  = filepath.Join(workdir, "skills") // 技能目录路径
	system     = ""                               // 系统提示词
)

// getConfig 获取配置信息
// 这个函数从环境变量中读取API配置参数
// 确保在.env文件加载后调用，以获取最新的配置值
func getConfig() (model, baseURL, apiKey, authHeaderName string) {
	model = os.Getenv("MODEL_ID")                    // 模型ID
	baseURL = os.Getenv("OPENAI_BASE_URL")           // API基础URL
	apiKey = os.Getenv("OPENAI_API_KEY")             // API密钥
	authHeaderName = os.Getenv("OPENAI_AUTH_HEADER") // 认证头名称
	return
}

// Skill 表示一个技能，包含元数据、主体内容和路径信息
// 这是S05版本的核心数据结构，用于定义和管理可加载的技能
type Skill struct {
	Meta map[string]string // 技能元数据（名称、描述、参数等）
	Body string            // 技能主体内容（通常是Markdown格式的说明）
	Path string            // 技能文件路径
}

// SkillLoader 技能加载器
// 负责从指定目录扫描、加载和管理所有技能文件
type SkillLoader struct {
	skillsDir string           // 技能目录路径
	skills    map[string]Skill // 已加载的技能映射表
}

// NewSkillLoader 创建新的技能加载器
// 初始化技能加载器并自动加载所有可用技能
func NewSkillLoader(skillsDir string) *SkillLoader {
	// 创建技能加载器实例
	sl := &SkillLoader{
		skillsDir: skillsDir,              // 设置技能目录
		skills:    make(map[string]Skill), // 初始化技能映射表
	}
	// 自动加载所有技能
	sl.loadAll()
	return sl
}

// loadAll 加载技能目录中的所有技能
// 扫描技能目录，查找所有符合格式的技能文件并加载到内存中
func (sl *SkillLoader) loadAll() {
	fmt.Printf("Scanning skills directory: %s\n", sl.skillsDir)
	// 检查技能目录是否存在
	if _, err := os.Stat(sl.skillsDir); os.IsNotExist(err) {
		fmt.Printf("Skills directory does not exist: %s\n", sl.skillsDir)
		return
	}

	// 递归扫描所有子目录中的 SKILL.md 文件
	// 使用filepath.Walk遍历整个技能目录树
	err := filepath.Walk(sl.skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err // 如果遍历出错，返回错误
		}

		// 只处理名为 SKILL.md 的文件
		// 这是技能定义文件的标准命名
		if filepath.Base(path) != "SKILL.md" {
			return nil // 跳过其他文件
		}

		// 读取技能文件内容
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Error reading skill file %s: %v", path, err)
			return nil // 读取失败时记录错误但继续处理其他文件
		}

		// 从文件路径推导技能名称
		// 例如: skills/git/SKILL.md -> 技能名为 "git"
		relPath, err := filepath.Rel(sl.skillsDir, path)
		if err != nil {
			relPath = filepath.Base(path) // 出错时使用文件名
		}
		name := filepath.Dir(relPath) // 获取父目录名作为技能名
		if name == "." {
			name = "default" // 如果在根目录，命名为default
		}

		fmt.Printf("Loading skill: %s from %s\n", name, path)
		// 解析技能文件的前置元数据和主体内容
		meta, body := sl.parseFrontmatter(string(content))
		// 将解析后的技能存储到映射表中
		sl.skills[name] = Skill{Meta: meta, Body: body, Path: path}

		return nil
	})

	if err != nil {
		log.Printf("Error walking skills directory: %v", err)
	}

	fmt.Printf("Loaded %d skills\n", len(sl.skills))
}

// parseFrontmatter 解析技能文件的前置元数据
// SKILL.md 文件使用 YAML front matter 格式，以 --- 分隔元数据和主体内容
// 返回解析后的元数据映射和主体内容
func (sl *SkillLoader) parseFrontmatter(text string) (map[string]string, string) {
	// 使用正则表达式匹配 YAML front matter 格式
	// 格式: ---\n<元数据>\n---\n<主体内容>
	re := regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)`)
	matches := re.FindStringSubmatch(text)
	if len(matches) != 3 {
		// 如果没有匹配到 front matter 格式，返回空元数据和原文本
		return make(map[string]string), text
	}

	// 解析元数据部分（YAML 格式的键值对）
	meta := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(matches[1]), "\n") {
		// 按冒号分割键值对，最多分割为两部分
		if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
			// 去除首尾空白后存储到元数据映射中
			meta[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	// 返回解析后的元数据和清理过的主体内容
	return meta, strings.TrimSpace(matches[2])
}

// GetDescriptions 获取所有技能的描述信息
// 返回格式化的技能列表，包含技能名称、描述和标签
// 用于向用户展示可用的技能选项
func (sl *SkillLoader) GetDescriptions() string {
	// 如果没有加载任何技能，返回提示信息
	if len(sl.skills) == 0 {
		return "(no skills available)"
	}

	// 构建技能描述列表
	var lines []string
	for name, skill := range sl.skills {
		// 获取技能描述，如果没有则使用默认值
		desc, ok := skill.Meta["description"]
		if !ok {
			desc = "No description"
		}
		// 获取技能标签（可选）
		tags, ok := skill.Meta["tags"]
		// 格式化技能信息行
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if ok {
			// 如果有标签，添加到行尾
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	// 将所有行连接成最终字符串
	return strings.Join(lines, "\n")
}

// GetContent 获取指定技能的内容
// 返回格式化的技能内容，包含技能名称和主体内容
// 如果技能不存在，返回错误信息和可用技能列表
func (sl *SkillLoader) GetContent(name string) string {
	// 查找指定的技能
	skill, ok := sl.skills[name]
	if !ok {
		// 如果技能不存在，收集所有可用技能名称
		var available []string
		for k := range sl.skills {
			available = append(available, k)
		}
		// 返回错误信息和可用技能列表
		return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, strings.Join(available, ", "))
	}
	// 返回格式化的技能内容，使用XML标签包裹
	return fmt.Sprintf("<skill name=\"%s\">\n%s\n</skill>", name, skill.Body)
}

// 全局技能加载器实例
// 在程序启动时自动初始化，加载所有可用技能
var skillLoader = NewSkillLoader(skillsDir)

// safePath 解析路径并确保它在工作目录内
// 这是S02版本引入的安全特性，S05继续使用
// 防止路径遍历攻击，确保文件操作安全
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
	if !strings.HasPrefix(absPath, workdirAbs) {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}

	return absPath, nil
}

// runBash 执行shell命令
// 继承自S02版本的安全shell命令执行功能
// 包含危险命令检查和超时控制
func runBash(command string) string {
	// 定义危险命令列表，防止执行破坏性操作
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		// 检查命令是否包含危险关键词
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
		// write_file工具：写入内容到文件
		return runWrite(args["path"].(string), args["content"].(string))
	},
	"edit_file": func(args map[string]interface{}) string {
		// edit_file工具：替换文件中的文本
		return runEdit(args["path"].(string), args["old_text"].(string), args["new_text"].(string))
	},
	"load_skill": func(args map[string]interface{}) string {
		// load_skill工具：加载指定名称的技能内容
		// 这是S05版本的核心新增功能，允许AI动态加载专业知识
		return skillLoader.GetContent(args["name"].(string))
	},
}

// OpenAI API 相关结构体定义
// 这些结构体用于序列化和反序列化OpenAI API的JSON数据

// Tool 表示一个可调用的工具
type Tool struct {
	Type     string      `json:"type"`     // 工具类型，通常为"function"
	Function FunctionDef `json:"function"` // 工具函数定义
}

// FunctionDef 表示工具函数的定义
type FunctionDef struct {
	Name        string      `json:"name"`        // 函数名称
	Description string      `json:"description"` // 函数描述
	Parameters  interface{} `json:"parameters"`  // 参数定义（JSON Schema格式）
}

// Message 表示对话消息
type Message struct {
	Role       string     `json:"role"`                   // 消息角色（system/user/assistant/tool）
	Content    string     `json:"content"`                // 消息内容
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // 工具调用列表（可选）
	ToolCallID string     `json:"tool_call_id,omitempty"` // 工具调用ID（可选）
}

// ToolCall 表示一个工具调用请求
type ToolCall struct {
	ID       string           `json:"id"`       // 工具调用唯一标识符
	Type     string           `json:"type"`     // 调用类型，通常为"function"
	Function ToolCallFunction `json:"function"` // 工具函数信息
}

// ToolCallFunction 表示工具调用的函数信息
type ToolCallFunction struct {
	Name      string `json:"name"`      // 函数名称
	Arguments string `json:"arguments"` // JSON格式的参数字符串
}

// ChatCompletionRequest 表示OpenAI聊天完成请求
type ChatCompletionRequest struct {
	Model       string    `json:"model"`       // 使用的模型ID
	Messages    []Message `json:"messages"`    // 对话消息历史
	Tools       []Tool    `json:"tools"`       // 可用工具列表
	ToolChoice  string    `json:"tool_choice"` // 工具选择策略，通常为"auto"
	Temperature float64   `json:"temperature"` // 温度参数，控制输出的随机性
}

// ChatCompletionResponse 表示OpenAI聊天完成响应
type ChatCompletionResponse struct {
	Choices []struct {
		Message Message `json:"message"` // AI生成的回复消息
	} `json:"choices"` // 所有可能的回复选择
}

// openAITools 生成OpenAI API工具列表
// 返回所有可用的工具定义，包括基础工具和技能加载工具
func openAITools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "load_skill", Description: "Load specialized knowledge by name.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}}, "required": []string{"name"}}}},
	}
}

func chatCompletionsCreate(messages []Message) (Message, error) {
	model, baseURL, apiKey, authHeaderName := getConfig()

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
	*messages = append([]Message{{Role: "system", Content: system}}, *messages...)
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
			fmt.Printf("\n 💁💁💁💁 大模型回复 开始 >>>>>>>> %s||||| %+v 大模型回复 结束 💁💁💁💁\n\n", name, args)
			handler, ok := toolHandlers[name]
			var output string
			if ok {
				output = handler.(func(map[string]interface{}) string)(args)
			} else {
				output = fmt.Sprintf("Unknown tool: %s", name)
			}

			if len(output) > 200 {
				fmt.Printf(">✌️ 执行命令:%s \n ✌️ 参数:%+v \n ✌️ 结果:%s...\n", name, args, output[:200])
			} else {
				fmt.Printf(">✌️ 执行命令:%s \n ✌️ 参数:%+v \n ✌️ 结果:%s\n", name, args, output)
			}

			*messages = append(*messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
		}
	}
}

// loadEnv 加载 .env 文件中的环境变量
// 这个函数会从项目根目录的 .env 文件中读取环境变量并设置到系统中
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
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释行
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析 KEY=VALUE 格式
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Printf("Warning: Invalid line %d in .env file: %s\n", lineNum, line)
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

// init 程序初始化函数
// 在main函数之前自动调用，负责加载环境变量和初始化系统配置
func init() {
	// 加载 .env 文件中的环境变量
	if err := loadEnv(); err != nil {
		log.Printf("Warning: Failed to load .env file: %v", err)
	}

	// 初始化系统提示词，包含技能系统的使用说明
	// 提示AI在处理不熟悉的话题时先使用load_skill工具获取专业知识
	system = fmt.Sprintf("You are a coding agent at %s.When executing scripts, If you need to use some scripts in the skill, Use load_skill to access specialized knowledge before tackling unfamiliar topics.\n\nSkills available:\n%s\n\nWhen executing scripts, you must include the skill name in the path:skill_name/scripts/xxx, you cannot omit the skill name.\n", workdir, skillLoader.GetDescriptions())
}

// main 程序主函数
// AI Agent代码助手的入口点，负责初始化和主循环
func main() {
	// 获取API配置信息
	model, baseURL, apiKey, _ := getConfig()
	// 检查必需的环境变量是否设置
	if model == "" || baseURL == "" || apiKey == "" {
		log.Fatalln("Error: Ensure MODEL_ID, OPENAI_BASE_URL, and OPENAI_API_KEY environment variables are set.")
	}

	history := []Message{}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Println("\033[36ms05 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := scanner.Text()
		if query == "q" || query == "exit" || query == "" {
			continue
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

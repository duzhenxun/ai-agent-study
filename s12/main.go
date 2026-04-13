// =============================================================================
// Directory-level isolation for parallel task execution.
// Tasks are the control plane and worktrees are the execution plane.

//     .tasks/task_12.json
//       {
//         "id": 12,
//         "subject": "Implement auth refactor",
//         "status": "in_progress",
//         "worktree": "auth-refactor"
//       }

//     .worktrees/index.json
//       {
//         "worktrees": [
//           {
//             "name": "auth-refactor",
//             "path": ".../.worktrees/auth-refactor",
//             "branch": "wt/auth-refactor",
//             "task_id": 12,
//             "status": "active"
//           }
//         ]
//       }

// Key insight: "Isolate by directory, coordinate by task ID."
//
// =============================================================================

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 全局配置变量
var (
	model          string       // 模型ID
	baseURL        string       // API基础URL
	apiKey         string       // API密钥
	authHeaderName string       // 认证头名称
	workdir, _     = os.Getwd() // 当前工作目录
	repoRoot       string       // 代码仓库根目录
	system         string       // 系统提示词
)

func detectRepoRoot(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	return strings.TrimSpace(string(out))
}

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
	// 检测代码仓库根目录
	repoRoot = detectRepoRoot(workdir)
	// 初始化系统提示词，包含任务和工作树管理说明
	system = fmt.Sprintf("You are a coding agent at %s. Use task + worktree tools for multi-task work. For parallel or risky changes: create tasks, allocate worktree lanes, run commands in those lanes, then choose keep/remove for closeout. Use worktree_events when you need lifecycle visibility.", workdir)
}

// checkDangerousCommands checks if a command contains dangerous patterns
func checkDangerousCommands(command string) bool {
	dangerous := []string{"rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"}
	for _, d := range dangerous {
		if strings.Contains(command, d) {
			return true
		}
	}
	return false
}

// Base tool implementations
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
	if checkDangerousCommands(command) {
		return "Error: Dangerous command blocked"
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = workdir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := stdout.String() + stderr.String()
	out = strings.TrimSpace(out)

	if err != nil {
		return fmt.Sprintf("Error: %v\n%s", err, out)
	}

	if out == "" {
		return "(no output)"
	}

	if len(out) > 50000 {
		return out[:50000]
	}

	return out
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

// EventBus for lifecycle events
type EventBus struct {
	path string
	mu   sync.Mutex
}

func NewEventBus(eventLogPath string) *EventBus {
	_ = os.MkdirAll(filepath.Dir(eventLogPath), 0755)
	if _, err := os.Stat(eventLogPath); os.IsNotExist(err) {
		os.WriteFile(eventLogPath, []byte(""), 0644)
	}
	return &EventBus{path: eventLogPath}
}

func (eb *EventBus) Emit(event string, task, worktree map[string]interface{}, errorMsg string) {
	payload := map[string]interface{}{
		"event":    event,
		"ts":       time.Now().Unix(),
		"task":     task,
		"worktree": worktree,
	}
	if errorMsg != "" {
		payload["error"] = errorMsg
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	f, _ := os.OpenFile(eb.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	json.NewEncoder(f).Encode(payload)
}

func (eb *EventBus) ListRecent(limit int) string {
	if limit == 0 {
		limit = 20
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	content, _ := os.ReadFile(eb.path)
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	var items []interface{}
	for _, line := range lines {
		var item interface{}
		if json.Unmarshal([]byte(line), &item) == nil {
			items = append(items, item)
		}
	}
	result, _ := json.MarshalIndent(items, "", "  ")
	return string(result)
}

// TaskManager for persistent task board
type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Owner       string `json:"owner"`
	Worktree    string `json:"worktree"`
	BlockedBy   []int  `json:"blockedBy"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type TaskManager struct {
	dir    string
	nextID int
	mu     sync.Mutex
}

func NewTaskManager(tasksDir string) *TaskManager {
	_ = os.MkdirAll(tasksDir, 0755)
	tm := &TaskManager{dir: tasksDir}
	tm.nextID = tm.maxID() + 1
	return tm
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

func (tm *TaskManager) path(taskID int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", taskID))
}

func (tm *TaskManager) load(taskID int) (*Task, error) {
	path := tm.path(taskID)
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
	task.UpdatedAt = time.Now().Unix()
	content, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tm.path(task.ID), content, 0644)
}

func (tm *TaskManager) Create(subject, description string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	task := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		Status:      "pending",
		CreatedAt:   time.Now().Unix(),
	}
	tm.save(task)
	tm.nextID++
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result)
}

func (tm *TaskManager) Get(taskID int) string {
	task, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result)
}

func (tm *TaskManager) Exists(taskID int) bool {
	_, err := os.Stat(tm.path(taskID))
	return !os.IsNotExist(err)
}

func (tm *TaskManager) Update(taskID int, status, owner *string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	task, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if status != nil {
		if *status != "pending" && *status != "in_progress" && *status != "completed" {
			return fmt.Sprintf("Error: Invalid status: %s", *status)
		}
		task.Status = *status
	}
	if owner != nil {
		task.Owner = *owner
	}
	tm.save(task)
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result)
}

func (tm *TaskManager) BindWorktree(taskID int, worktree, owner string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	task, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	task.Worktree = worktree
	if owner != "" {
		task.Owner = owner
	}
	if task.Status == "pending" {
		task.Status = "in_progress"
	}
	tm.save(task)
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result)
}

func (tm *TaskManager) UnbindWorktree(taskID int) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	task, err := tm.load(taskID)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	task.Worktree = ""
	tm.save(task)
	result, _ := json.MarshalIndent(task, "", "  ")
	return string(result)
}

func (tm *TaskManager) ListAll() string {
	var tasks []*Task
	files, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	sort.Strings(files)
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var task Task
		if json.Unmarshal(content, &task) == nil {
			tasks = append(tasks, &task)
		}
	}
	if len(tasks) == 0 {
		return "No tasks."
	}
	var lines []string
	for _, t := range tasks {
		marker := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}[t.Status]
		if marker == "" {
			marker = "[?]"
		}
		owner := ""
		if t.Owner != "" {
			owner = fmt.Sprintf(" owner=%s", t.Owner)
		}
		wt := ""
		if t.Worktree != "" {
			wt = fmt.Sprintf(" wt=%s", t.Worktree)
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", marker, t.ID, t.Subject, owner, wt))
	}
	return strings.Join(lines, "\n")
}

var tasks = NewTaskManager(filepath.Join(repoRoot, ".tasks"))
var events = NewEventBus(filepath.Join(repoRoot, ".worktrees", "events.jsonl"))

// WorktreeManager for git worktrees

type Worktree struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	TaskID    int    `json:"task_id"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	KeptAt    int64  `json:"kept_at,omitempty"`
	RemovedAt int64  `json:"removed_at,omitempty"`
}

type WorktreeIndex struct {
	Worktrees []Worktree `json:"worktrees"`
}

type WorktreeManager struct {
	repoRoot     string
	tasks        *TaskManager
	events       *EventBus
	dir          string
	indexPath    string
	gitAvailable bool
	mu           sync.Mutex
}

func NewWorktreeManager(repoRoot string, tasks *TaskManager, events *EventBus) *WorktreeManager {
	wm := &WorktreeManager{
		repoRoot:  repoRoot,
		tasks:     tasks,
		events:    events,
		dir:       filepath.Join(repoRoot, ".worktrees"),
		indexPath: filepath.Join(repoRoot, ".worktrees", "index.json"),
	}
	_ = os.MkdirAll(wm.dir, 0755)
	if _, err := os.Stat(wm.indexPath); os.IsNotExist(err) {
		wm.saveIndex(&WorktreeIndex{Worktrees: []Worktree{}})
	}
	wm.gitAvailable = wm.isGitRepo()
	return wm
}

func (wm *WorktreeManager) isGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = wm.repoRoot
	return cmd.Run() == nil
}

func (wm *WorktreeManager) runGit(args ...string) (string, error) {
	if !wm.gitAvailable {
		return "", fmt.Errorf("not in a git repository")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = wm.repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %s", strings.Join(args, " "), string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (wm *WorktreeManager) loadIndex() (*WorktreeIndex, error) {
	content, err := os.ReadFile(wm.indexPath)
	if err != nil {
		return nil, err
	}
	var index WorktreeIndex
	err = json.Unmarshal(content, &index)
	return &index, err
}

func (wm *WorktreeManager) saveIndex(index *WorktreeIndex) error {
	content, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(wm.indexPath, content, 0644)
}

func (wm *WorktreeManager) find(name string) *Worktree {
	index, err := wm.loadIndex()
	if err != nil {
		return nil
	}
	for i, wt := range index.Worktrees {
		if wt.Name == name {
			return &index.Worktrees[i]
		}
	}
	return nil
}

func (wm *WorktreeManager) validateName(name string) error {
	if matched, _ := regexp.MatchString(`^[A-Za-z0-9._-]{1,40}$`, name); !matched {
		return fmt.Errorf("invalid worktree name")
	}
	return nil
}

func (wm *WorktreeManager) Create(name string, taskID *int, baseRef string) string {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if err := wm.validateName(name); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if wm.find(name) != nil {
		return fmt.Sprintf("Error: Worktree '%s' already exists", name)
	}
	if taskID != nil && !wm.tasks.Exists(*taskID) {
		return fmt.Sprintf("Error: Task %d not found", *taskID)
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}

	path := filepath.Join(wm.dir, name)
	branch := "wt/" + name
	wm.events.Emit("worktree.create.before", map[string]interface{}{"id": taskID}, map[string]interface{}{"name": name, "base_ref": baseRef}, "")

	_, err := wm.runGit("worktree", "add", "-b", branch, path, baseRef)
	if err != nil {
		wm.events.Emit("worktree.create.failed", map[string]interface{}{"id": taskID}, map[string]interface{}{"name": name}, err.Error())
		return fmt.Sprintf("Error: %v", err)
	}

	entry := Worktree{
		Name:      name,
		Path:      path,
		Branch:    branch,
		TaskID:    -1,
		Status:    "active",
		CreatedAt: time.Now().Unix(),
	}
	if taskID != nil {
		entry.TaskID = *taskID
	}

	index, _ := wm.loadIndex()
	index.Worktrees = append(index.Worktrees, entry)
	wm.saveIndex(index)

	if taskID != nil {
		wm.tasks.BindWorktree(*taskID, name, "")
	}

	wm.events.Emit("worktree.create.after", map[string]interface{}{"id": taskID}, map[string]interface{}{"name": name, "path": path, "branch": branch, "status": "active"}, "")
	result, _ := json.MarshalIndent(entry, "", "  ")
	return string(result)
}

func (wm *WorktreeManager) ListAll() string {
	index, err := wm.loadIndex()
	if err != nil || len(index.Worktrees) == 0 {
		return "No worktrees in index."
	}
	var lines []string
	for _, wt := range index.Worktrees {
		suffix := ""
		if wt.TaskID != -1 {
			suffix = fmt.Sprintf(" task=%d", wt.TaskID)
		}
		lines = append(lines, fmt.Sprintf("[%s] %s -> %s (%s)%s", wt.Status, wt.Name, wt.Path, wt.Branch, suffix))
	}
	return strings.Join(lines, "\n")
}

func (wm *WorktreeManager) Status(name string) string {
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}
	cmd := exec.Command("git", "status", "--short", "--branch")
	cmd.Dir = wt.Path
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func (wm *WorktreeManager) Run(name, command string) string {
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}

	// Check for dangerous commands
	if checkDangerousCommands(command) {
		return "Error: Dangerous command blocked"
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = wt.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Error: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func (wm *WorktreeManager) Remove(name string, force, completeTask bool) string {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}

	wm.events.Emit("worktree.remove.before", map[string]interface{}{"id": wt.TaskID}, map[string]interface{}{"name": name, "path": wt.Path}, "")

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)
	_, err := wm.runGit(args...)
	if err != nil {
		wm.events.Emit("worktree.remove.failed", map[string]interface{}{"id": wt.TaskID}, map[string]interface{}{"name": name}, err.Error())
		return fmt.Sprintf("Error: %v", err)
	}

	if completeTask && wt.TaskID != -1 {
		task, _ := wm.tasks.load(wt.TaskID)
		status := "completed"
		wm.tasks.Update(wt.TaskID, &status, nil)
		wm.tasks.UnbindWorktree(wt.TaskID)
		wm.events.Emit("task.completed", map[string]interface{}{"id": wt.TaskID, "subject": task.Subject, "status": "completed"}, map[string]interface{}{"name": name}, "")
	}

	index, _ := wm.loadIndex()
	for i := range index.Worktrees {
		if index.Worktrees[i].Name == name {
			index.Worktrees[i].Status = "removed"
			index.Worktrees[i].RemovedAt = time.Now().Unix()
			break
		}
	}
	wm.saveIndex(index)

	wm.events.Emit("worktree.remove.after", map[string]interface{}{"id": wt.TaskID}, map[string]interface{}{"name": name, "status": "removed"}, "")
	return fmt.Sprintf("Removed worktree '%s'", name)
}

func (wm *WorktreeManager) Keep(name string) string {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wt := wm.find(name)
	if wt == nil {
		return fmt.Sprintf("Error: Unknown worktree '%s'", name)
	}

	index, _ := wm.loadIndex()
	var kept *Worktree
	for i := range index.Worktrees {
		if index.Worktrees[i].Name == name {
			index.Worktrees[i].Status = "kept"
			index.Worktrees[i].KeptAt = time.Now().Unix()
			kept = &index.Worktrees[i]
			break
		}
	}
	wm.saveIndex(index)

	wm.events.Emit("worktree.keep", map[string]interface{}{"id": wt.TaskID}, map[string]interface{}{"name": name, "status": "kept"}, "")
	if kept != nil {
		result, _ := json.MarshalIndent(kept, "", "  ")
		return string(result)
	}
	return fmt.Sprintf("Error: Unknown worktree '%s'", name)
}

var worktrees = NewWorktreeManager(repoRoot, tasks, events)

// Base tools and OpenAI structs

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

// Tool Handlers
var toolHandlers = map[string]interface{}{
	"bash": func(args map[string]interface{}) string {
		return runBash(args["command"].(string))
	},
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
	"task_create": func(args map[string]interface{}) string {
		return tasks.Create(args["subject"].(string), args["description"].(string))
	},
	"task_list": func(args map[string]interface{}) string {
		return tasks.ListAll()
	},
	"task_get": func(args map[string]interface{}) string {
		return tasks.Get(int(args["task_id"].(float64)))
	},
	"task_update": func(args map[string]interface{}) string {
		var status, owner *string
		if s, ok := args["status"].(string); ok {
			status = &s
		}
		if o, ok := args["owner"].(string); ok {
			owner = &o
		}
		return tasks.Update(int(args["task_id"].(float64)), status, owner)
	},
	"task_bind_worktree": func(args map[string]interface{}) string {
		taskID := int(args["task_id"].(float64))
		worktree := args["worktree"].(string)
		owner := ""
		if o, ok := args["owner"].(string); ok {
			owner = o
		}
		return tasks.BindWorktree(taskID, worktree, owner)
	},
	"worktree_create": func(args map[string]interface{}) string {
		var taskID *int
		if id, ok := args["task_id"].(float64); ok {
			val := int(id)
			taskID = &val
		}
		baseRef, _ := args["base_ref"].(string)
		return worktrees.Create(args["name"].(string), taskID, baseRef)
	},
	"worktree_list": func(args map[string]interface{}) string {
		return worktrees.ListAll()
	},
	"worktree_status": func(args map[string]interface{}) string {
		return worktrees.Status(args["name"].(string))
	},
	"worktree_run": func(args map[string]interface{}) string {
		return worktrees.Run(args["name"].(string), args["command"].(string))
	},
	"worktree_remove": func(args map[string]interface{}) string {
		force, _ := args["force"].(bool)
		completeTask, _ := args["complete_task"].(bool)
		return worktrees.Remove(args["name"].(string), force, completeTask)
	},
	"worktree_keep": func(args map[string]interface{}) string {
		return worktrees.Keep(args["name"].(string))
	},
	"worktree_events": func(args map[string]interface{}) string {
		limit := 0
		if l, ok := args["limit"].(float64); ok {
			limit = int(l)
		}
		return events.ListRecent(limit)
	},
}

func openAITools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "bash", Description: "Run a shell command in the current workspace (blocking).", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]string{"type": "string"}}, "required": []string{"command"}}}},
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file contents.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "limit": map[string]interface{}{"type": "integer"}}, "required": []string{"path"}}}},
		{Type: "function", Function: FunctionDef{Name: "write_file", Description: "Write content to file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"path", "content"}}}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Replace exact text in file.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]string{"type": "string"}, "old_text": map[string]string{"type": "string"}, "new_text": map[string]string{"type": "string"}}, "required": []string{"path", "old_text", "new_text"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_create", Description: "Create a new task on the shared task board.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"subject": map[string]string{"type": "string"}, "description": map[string]string{"type": "string"}}, "required": []string{"subject"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_list", Description: "List all tasks with status, owner, and worktree binding.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "task_get", Description: "Get task details by ID.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}}, "required": []string{"task_id"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_update", Description: "Update task status or owner.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}, "status": map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}}, "owner": map[string]string{"type": "string"}}, "required": []string{"task_id"}}}},
		{Type: "function", Function: FunctionDef{Name: "task_bind_worktree", Description: "Bind a task to a worktree name.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"task_id": map[string]interface{}{"type": "integer"}, "worktree": map[string]string{"type": "string"}, "owner": map[string]string{"type": "string"}}, "required": []string{"task_id", "worktree"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_create", Description: "Create a git worktree and optionally bind it to a task.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}, "task_id": map[string]interface{}{"type": "integer"}, "base_ref": map[string]string{"type": "string"}}, "required": []string{"name"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_list", Description: "List worktrees tracked in .worktrees/index.json.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_status", Description: "Show git status for one worktree.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}}, "required": []string{"name"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_run", Description: "Run a shell command in a named worktree directory.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}, "command": map[string]string{"type": "string"}}, "required": []string{"name", "command"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_remove", Description: "Remove a worktree and optionally mark its bound task completed.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}, "force": map[string]interface{}{"type": "boolean"}, "complete_task": map[string]interface{}{"type": "boolean"}}, "required": []string{"name"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_keep", Description: "Mark a worktree as kept in lifecycle state without removing it.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]string{"type": "string"}}, "required": []string{"name"}}}},
		{Type: "function", Function: FunctionDef{Name: "worktree_events", Description: "List recent worktree/task lifecycle events from .worktrees/events.jsonl.", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"limit": map[string]interface{}{"type": "integer"}}}}},
	}
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
		fmt.Print("\033[36ms12 >> \033[0m")
		if !scanner.Scan() {
			break
		}
		query := scanner.Text()
		if strings.TrimSpace(strings.ToLower(query)) == "q" || strings.TrimSpace(strings.ToLower(query)) == "exit" || query == "" {
			break
		}

		if strings.TrimSpace(query) == "/tasks" {
			fmt.Println(tasks.ListAll())
			continue
		}
		if strings.TrimSpace(query) == "/worktrees" {
			fmt.Println(worktrees.ListAll())
			continue
		}
		if strings.TrimSpace(query) == "/events" {
			fmt.Println(events.ListRecent(20))
			continue
		}

		history = append(history, Message{Role: "user", Content: query})
		agentLoop(&history)
		fmt.Println()
	}
}

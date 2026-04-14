# s12: Worktree + Task Isolation (Worktree 任务隔离)

`s01 > s02 > s03 > s04 > s05 > s06 | s07 > s08 > s09 > s10 > s11 > [ s12 ]`

> _"各干各的目录, 互不干扰"_ -- 任务管目标, worktree 管目录, 按 ID 绑定。
>
> **Harness 层**: 目录隔离 -- 永不碰撞的并行执行通道。

## 问题

到 s11, 智能体已经能自主认领和完成任务。但所有任务共享一个目录。两个智能体同时重构不同模块 -- A 改 `config.py`, B 也改 `config.py`, 未提交的改动互相污染, 谁也没法干净回滚。

任务板管 "做什么" 但不管 "在哪做"。解法: 给每个任务一个独立的 git worktree 目录, 用任务 ID 把两边关联起来。

## 解决方案

```
Control plane (.tasks/)             Execution plane (.worktrees/)
+------------------+                +------------------------+
| task_1.json      |                | auth-refactor/         |
|   status: in_progress  <------>   branch: wt/auth-refactor
|   worktree: "auth-refactor"   |   task_id: 1             |
+------------------+                +------------------------+
| task_2.json      |                | ui-login/              |
|   status: pending    <------>     branch: wt/ui-login
|   worktree: "ui-login"       |   task_id: 2             |
+------------------+                +------------------------+
                                    |
                          index.json (worktree registry)
                          events.jsonl (lifecycle log)

State machines:
  Task:     pending -> in_progress -> completed
  Worktree: absent  -> active      -> removed | kept
```

## 工作原理

#### System Prompt

```
You are a coding agent at %s. Use task + worktree tools for multi-task work. For parallel or risky changes: create tasks, allocate worktree lanes, run commands in those lanes, then choose keep/remove for closeout. Use worktree_events when you need lifecycle visibility.
```

1. **创建任务。** 先把目标持久化。

```go
// 创建任务
result, err := tasks.Create("Implement auth refactor", "")
if err != nil {
    return fmt.Sprintf("Error: %v", err)
}
fmt.Println(result)
// -> .tasks/task_1.json  status=pending  worktree=""
```

2. **创建 worktree 并绑定任务。** 传入 `task_id` 自动将任务推进到 `in_progress`。

```go
// 创建 worktree 并绑定任务
worktreeManager := NewWorktreeManager()
result, err := worktreeManager.Create("auth-refactor", 1)
if err != nil {
    return fmt.Sprintf("Error: %v", err)
}
fmt.Println(result)
// -> git worktree add -b wt/auth-refactor .worktrees/auth-refactor HEAD
// -> index.json gets new entry, task_1.json gets worktree="auth-refactor"
```

绑定同时写入两侧状态:

```go
func (wtm *WorktreeManager) bindWorktree(taskID int, worktree string) error {
    // 加载任务
    task, err := tasks.Get(taskID)
    if err != nil {
        return err
    }

    // 解析任务JSON
    var taskData map[string]interface{}
    if err := json.Unmarshal([]byte(task), &taskData); err != nil {
        return err
    }

    // 绑定worktree
    taskData["worktree"] = worktree
    if taskData["status"] == "pending" {
        taskData["status"] = "in_progress"
    }

    // 保存任务
    updatedTask, _ := json.Marshal(taskData)
    _, err = tasks.Update(taskID, string(updatedTask), nil, nil)
    return err
}
```

3. **在 worktree 中执行命令。** `cwd` 指向隔离目录。

```go
func runInWorktree(command, worktreePath string) (string, error) {
    ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
    defer cancel()

    cmd := exec.CommandContext(ctx, "sh", "-c", command)
    cmd.Dir = worktreePath
    var out bytes.Buffer
    cmd.Stdout = &out
    cmd.Stderr = &out

    err := cmd.Run()
    output := out.String()

    if ctx.Err() == context.DeadlineExceeded {
        return "Error: Timeout (300s)", nil
    }
    if err != nil {
        return fmt.Sprintf("Error: %v\nOutput:\n%s", err, output), nil
    }

    return output, nil
}
```

4. **收尾。** 两种选择:
   - `worktree_keep(name)` -- 保留目录供后续使用。
   - `worktree_remove(name, complete_task=True)` -- 删除目录, 完成绑定任务, 发出事件。一个调用搞定拆除 + 完成。

```go
func (wtm *WorktreeManager) Remove(name string, force, completeTask bool) error {
    // 获取worktree信息
    wt, err := wtm.Get(name)
    if err != nil {
        return err
    }

    // 删除worktree
    cmd := exec.Command("git", "worktree", "remove", wt.Path)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("failed to remove worktree: %w", err)
    }

    if completeTask && wt.TaskID != nil {
        // 完成任务
        _, err := tasks.Update(*wt.TaskID, "completed", nil, nil)
        if err != nil {
            return err
        }

        // 解绑worktree
        wtm.unbindWorktree(*wt.TaskID)

        // 发出事件
        wtm.EmitEvent("task.completed", map[string]interface{}{
            "task_id": *wt.TaskID,
            "worktree": name,
        })
    }

    return nil
}
```

5. **事件流。** 每个生命周期步骤写入 `.worktrees/events.jsonl`:

```go
type WorktreeEvent struct {
    Event   string      `json:"event"`
    Task    Task        `json:"task"`
    Worktree Worktree    `json:"worktree"`
    TS      int64       `json:"ts"`
}

// 示例事件
func (wtm *WorktreeManager) EmitEvent(eventType string, data map[string]interface{}) error {
    event := WorktreeEvent{
        Event: eventType,
        TS:    time.Now().Unix(),
    }

    // 根据事件类型填充数据
    switch eventType {
    case "worktree.remove.after":
        if taskID, ok := data["task_id"].(int); ok {
            if task, err := tasks.Get(taskID); err == nil {
                var taskData Task
                json.Unmarshal([]byte(task), &taskData)
                event.Task = taskData
            }
        }
        if worktreeName, ok := data["worktree"].(string); ok {
            if wt, err := wtm.Get(worktreeName); err == nil {
                wt.Status = "removed"
                event.Worktree = *wt
            }
        }
    }

    // 写入事件日志
    return wtm.writeEvent(event)
}
```

事件类型: `worktree.create.before/after/failed`, `worktree.remove.before/after/failed`, `worktree.keep`, `task.completed`。

崩溃后从 `.tasks/` + `.worktrees/index.json` 重建现场。会话记忆是易失的; 磁盘状态是持久的。

## 相对 s11 的变更

| 组件           | 之前 (s11)            | 之后 (s12)                           |
| -------------- | --------------------- | ------------------------------------ |
| 协调           | 任务板 (owner/status) | 任务板 + worktree 显式绑定           |
| 执行范围       | 共享目录              | 每个任务独立目录                     |
| 可恢复性       | 仅任务状态            | 任务状态 + worktree 索引             |
| 收尾           | 任务完成              | 任务完成 + 显式 keep/remove          |
| 生命周期可见性 | 隐式日志              | `.worktrees/events.jsonl` 显式事件流 |

## 试一试

```sh
cd ai-agent-study/s12
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `Create tasks for backend auth and frontend login page, then list tasks.`
2. `Create worktree "auth-refactor" for task 1, then bind task 2 to a new worktree "ui-login".`
3. `Run "git status --short" in worktree "auth-refactor".`
4. `Keep worktree "ui-login", then list worktrees and inspect events.`
5. `Remove worktree "auth-refactor" with complete_task=true, then list tasks/worktrees/events.`

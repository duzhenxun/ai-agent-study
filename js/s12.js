// AI Agent 代码助手 - S12版本
// 在S11基础上增加了工作树任务隔离功能
import axios from 'axios';
import { exec } from 'child_process';
import { promisify } from 'util';
import readline from 'readline';
import { config } from 'dotenv';
import { existsSync } from 'fs';
import * as fs from 'fs/promises';
import * as path from 'path';

// Load environment variables from .env file
const envPath = './../.env'; // js 同级的 .env 文件
if (existsSync(envPath)) {
  config({ path: envPath });
  console.log(`✅ 已加载环境变量文件: ${envPath}`);
} else {
  console.log(`⚠️  警告: 未找到 ${envPath} 文件，使用默认配置`);
  console.log('💡 提示: 请复制 .env.example 为 .env 并填入你的 API 配置');
}

const execPromise = promisify(exec);

// ====================== 从环境变量读取配置======================
const MODEL_ID = process.env.MODEL_ID || "deepseek-chat";
const OPENAI_BASE_URL = process.env.OPENAI_BASE_URL || "https://api.deepseek.com/chat/completions";
const OPENAI_API_KEY = process.env.OPENAI_API_KEY || "";
const WORK_DIR = process.cwd();
const TASKS_DIR = path.join(WORK_DIR, '.tasks');
const WORKTREES_DIR = path.join(WORK_DIR, '.worktrees');
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use task + worktree tools for multi-task work. For parallel or risky changes: create tasks, allocate worktree lanes, run commands in those lanes, then choose keep/remove for closeout. Use worktree_events when you need lifecycle visibility.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

/**
 * TaskManager 任务管理器
 */
class TaskManager {
  constructor() {
    this.tasks = new Map();
  }

  /**
   * Create 创建新任务
   */
  async create(subject, worktree = null) {
    const id = Date.now().toString();
    const task = {
      id,
      subject,
      status: 'in_progress',
      worktree,
      created_at: new Date().toISOString()
    };
    
    this.tasks.set(id, task);
    
    try {
      const filePath = path.join(TASKS_DIR, `${id}.json`);
      await fs.mkdir(TASKS_DIR, { recursive: true });
      await fs.writeFile(filePath, JSON.stringify(task, null, 2), 'utf8');
      
      console.log(`✅ 创建任务 ${id}: ${subject}`);
      return { success: true, message: `Created task ${id}: ${subject}` };
    } catch (err) {
      return { success: false, error: `Failed to create task: ${err.message}` };
    }
  }

  /**
   * Update 更新任务状态
   */
  async updateStatus(id, status) {
    const task = this.tasks.get(id);
    if (!task) {
      return { success: false, error: `Task ${id} not found` };
    }

    task.status = status;
    task.updated_at = new Date().toISOString();
    
    try {
      const filePath = path.join(TASKS_DIR, `${id}.json`);
      await fs.mkdir(TASKS_DIR, { recursive: true });
      await fs.writeFile(filePath, JSON.stringify(task, null, 2), 'utf8');
      
      console.log(`✅ 更新任务 ${id} to ${status}`);
      return { success: true, message: `Updated task ${id} to ${status}` };
    } catch (err) {
      return { success: false, error: `Failed to update task: ${err.message}` };
    }
  }

  /**
   * Get 获取任务
   */
  get(id) {
    return this.tasks.get(id);
  }

  /**
   * Get 获取所有任务
   */
  getAll() {
    return Array.from(this.tasks.values());
  }

  /**
   * Get 获取未认领的任务
   */
  getUnclaimed() {
    return Array.from(this.tasks.values()).filter(task => task.status === 'in_progress');
  }
}

/**
 * WorktreeManager 工作树管理器
 */
class WorktreeManager {
  constructor() {
    this.worktreesDir = WORKTREES_DIR;
    this.worktrees = new Map();
  }

  /**
   * Create 创建工作树
   */
  async create(taskId) {
    const worktreeName = `wt-${taskId}`;
    const worktreePath = path.join(this.worktreesDir, worktreeName);
    
    try {
      // 创建工作树目录
      await fs.mkdir(worktreePath, { recursive: true });
      
      // 初始化工作树
      const initResult = execPromise(`git init ${worktreePath}`, { cwd: WORK_DIR });
      await initResult;
      
      // 添加工作树到任务
      const taskManager = new TaskManager();
      await taskManager.updateStatus(taskId, 'active');
      
      console.log(`✅ 创建工作树 ${worktreeName} for task ${taskId}`);
      return { success: true, worktree: worktreeName };
    } catch (err) {
      return { success: false, error: `Failed to create worktree: ${err.message}` };
    }
  }

  /**
   * Get 获取工作树路径
   */
  getWorktreePath(taskId) {
    const task = new TaskManager().get(taskId);
    return task?.worktree || null;
  }

  /**
   * Run 在工作树中执行命令
   */
  async runCommand(taskId, command) {
    const worktreePath = this.getWorktreePath(taskId);
    if (!worktreePath) {
      return { success: false, error: `No worktree found for task ${taskId}` };
    }

    try {
      const result = execPromise(`git -C ${worktreePath} ${command}`, { cwd: WORK_DIR });
      await result;
      
      console.log(`✅ 在工作树 ${worktreePath} 中执行: ${command}`);
      return { success: true, output: result.stdout || result.stderr };
    } catch (err) {
      return { success: false, error: `Failed to run command in worktree: ${err.message}` };
    }
  }

  /**
   * Close 关闭工作树
   */
  async close(taskId, action) {
    const worktreePath = this.getWorktreePath(taskId);
    if (!worktreePath) {
      return { success: false, error: `No worktree found for task ${taskId}` };
    }

    try {
      const taskManager = new TaskManager();
      
      switch (action) {
        case 'keep':
          console.log(`📁 保留工作树 ${worktreePath}`);
          break;
          
        case 'remove':
          console.log(`🗑️ 删除工作树 ${worktreePath}`);
          await fs.rm(worktreePath, { recursive: true, force: true });
          break;
      }
      
      await taskManager.updateStatus(taskId, 'completed');
      
      console.log(`✅ 任务 ${taskId} ${action} 工作树 ${worktreePath}`);
      return { success: true, message: `Task ${taskId} worktree ${action}d` };
    } catch (err) {
      return { success: false, error: `Failed to ${action} worktree: ${err.message}` };
    }
  }
}

/**
 * runBash 执行shell命令
 */
async function runBash(command) {
  try {
    const execProcess = execPromise(command, {
      cwd: WORK_DIR,
      shell: "/bin/bash",
    });

    const { stdout, stderr } = await execProcess;
    let output = (stdout + stderr).trim();
    
    if (!output) output = "(no output)";
    
    return output;
  } catch (err) {
    return `Error: ${err.message || err.toString()}`;
  }
}

/**
 * 调用 AI 接口
 */
async function chatCompletionsCreate(messages) {
  try {
    const response = await axios({
      method: "POST",
      url: OPENAI_BASE_URL,
      headers: {
        "Authorization": `Bearer ${OPENAI_API_KEY}`,
        "Content-Type": "application/json",
      },
      data: {
        model: MODEL_ID,
        messages: messages,
        tools: [
          {
            type: "function",
            function: {
              name: "bash",
              description: "Run a shell command.",
              parameters: {
                type: "object",
                properties: {
                  command: { type: "string" },
                },
                required: ["command"],
              },
            },
          {
            type: "function",
            function: {
              name: "task",
              description: "Manage tasks. Create, update, list, get, get_unclaimed, or get_all.",
              parameters: {
                type: "object",
                properties: {
                  action: { 
                    type: "string",
                    enum: ["create", "update", "list", "get", "get_unclaimed", "get_all"]
                  },
                  subject: { 
                    type: "string",
                    description: "Task subject for create action"
                  },
                  id: { 
                    type: "string",
                    description: "Task ID for get/update/get_unclaimed actions"
                  },
                  worktree: { 
                    type: "string",
                    description: "Worktree name for task"
                  }
                },
                required: ["action"],
              },
            },
          {
            type: "function",
            function: {
              name: "worktree",
              description: "Worktree management commands.",
              parameters: {
                type: "object",
                properties: {
                  task_id: { 
                    type: "string",
                    description: "Task ID for worktree operations"
                  },
                  command: { 
                    type: "string",
                    description: "Git command to run in worktree"
                  },
                  action: { 
                    type: "string",
                    enum: ["create", "run", "close"]
                  }
                },
                required: ["task_id", "command"],
              },
            },
          {
            type: "function",
            function: {
              name: "worktree_events",
              description: "Get worktree lifecycle events.",
              parameters: {
                type: "object",
                properties: {
                  task_id: { 
                    type: "string",
                    description: "Task ID for worktree operations"
                  }
                },
                required: ["task_id"],
              },
            },
        ],
        tool_choice: "auto",
        temperature: 0,
      },
      timeout: 120000,
    });

    if (response.data?.choices?.length === 0) {
      throw new Error("No choices in response");
    }

    return response.data.choices[0].message;
  } catch (err) {
    console.error("\nAPI 调用错误:", err.message);
    return null;
  }
}

/**
 * Agent 主循环：调用 AI → 执行工具 → 回传结果
 */
async function agentLoop(messages) {
  while (true) {
    const msg = await chatCompletionsCreate(messages);
    if (!msg) return;

    messages.push(msg);

    // 没有工具调用 → 直接输出内容并结束
    if (!msg.tool_calls || msg.tool_calls.length === 0) {
      if (msg.content) console.log("\n" + msg.content + "\n");
      return;
    }

    // 执行所有工具调用
    for (const tool of msg.tool_calls) {
      try {
        const args = JSON.parse(tool.function.arguments);
        let result;

        switch (tool.function.name) {
          case "bash":
            console.log(`\n\x1b[33m$ ${args.command}\x1b[0m`);
            result = await runBash(args.command);
            break;
            
          case "task":
            const taskManager = new TaskManager();
            
            switch (args.action) {
              case "create":
                result = await taskManager.create(args.subject, args.worktree);
                break;
                
              case "update":
                result = await taskManager.updateStatus(args.id, args.status);
                break;
                
              case "list":
                result = JSON.stringify(taskManager.getAll());
                break;
                
              case "get":
                result = JSON.stringify(taskManager.get(args.id));
                break;
                
              case "get_unclaimed":
                result = JSON.stringify(taskManager.getUnclaimed());
                break;
                
              case "get_all":
                result = JSON.stringify(taskManager.getAll());
                break;
                
              default:
                result = `Error: Unknown task action: ${args.action}`;
            }
            break;
            
          case "worktree":
            const worktreeManager = new WorktreeManager();
            
            switch (args.action) {
              case "create":
                result = await worktreeManager.create(args.task_id);
                break;
                
              case "run":
                result = await worktreeManager.runCommand(args.task_id, args.command);
                break;
                
              case "close":
                result = await worktreeManager.close(args.task_id, args.action);
                break;
                
              default:
                result = `Error: Unknown worktree action: ${args.action}`;
            }
            break;
            
          default:
            result = `Error: Unknown tool: ${tool.function.name}`;
        }

        // 输出预览
        if (result.length > 200) {
          console.log(result.slice(0, 200) + "...");
        } else {
          console.log(result);
        }

        // 把结果回传给 AI
        messages.push({
          role: "tool",
          tool_call_id: tool.id,
          content: result,
        });
      } catch (e) {
        console.error("工具执行错误:", e);
      }
    }
  }
}

/**
 * 主程序：命令行交互
 */
async function main() {
  console.log("=== AI 命令行助手 (S12 - 工作树任务隔离版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("任务目录:", TASKS_DIR);
  console.log("工作树目录:", WORKTREES_DIR);
  console.log("输入 q / exit 退出\n");

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms12 >> \x1b[0m", async (input) => {
      const query = input.trim();
      if (!query) return ask();

      const lower = query.toLowerCase();
      if (lower === "q" || lower === "exit") {
        rl.close();
        return;
      }

      messages.push({ role: "user", content: query });
      await agentLoop(messages);
      ask();
    });
  };

  ask();
}

// 启动
main().catch(console.error);

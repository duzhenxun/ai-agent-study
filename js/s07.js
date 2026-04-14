// AI Agent 代码助手 - S07版本
// 在S06基础上增加了任务系统功能
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
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use task tools to plan and track work.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

/**
 * TaskItem 表示一个任务项
 */
class TaskItem {
  constructor(id, text, status = 'pending') {
    this.id = id;
    this.text = text;
    this.status = status; // pending/in_progress/completed
  }
}

/**
 * TaskManager 任务管理器
 * 负责管理所有任务，支持创建、更新、状态管理和持久化存储
 */
class TaskManager {
  constructor(tasksDir) {
    this.tasksDir = tasksDir;
    this.items = new Map();
  }

  /**
   * Create 创建新任务
   */
  async create(text) {
    const id = Date.now().toString();
    const task = new TaskItem(id, text, 'pending');
    this.items.set(id, task);
    
    await this.saveTask(task);
    return { success: true, message: `Created task ${id}: ${text}` };
  }

  /**
   * Update 更新任务状态
   */
  async update(id, status) {
    const task = this.items.get(id);
    if (!task) {
      return { success: false, error: `Task ${id} not found` };
    }

    task.status = status;
    await this.saveTask(task);
    return { success: true, message: `Updated task ${id} to ${status}` };
  }

  /**
   * Get 获取任务
   */
  get(id) {
    return this.items.get(id);
  }

  /**
   * List 列出所有任务
   */
  list() {
    return Array.from(this.items.values());
  }

  /**
   * Get 获取可执行的任务（无依赖）
   */
  getAvailable() {
    return Array.from(this.items.values()).filter(task => task.status === 'pending');
  }

  /**
   * Save 保存任务到文件
   */
  async saveTask(task) {
    const filePath = path.join(this.tasksDir, `${task.id}.json`);
    await fs.mkdir(this.tasksDir, { recursive: true });
    await fs.writeFile(filePath, JSON.stringify(task), 'utf8');
  }

  /**
   * Load 加载所有任务
   */
  async load() {
    try {
      await fs.mkdir(this.tasksDir, { recursive: true });
      const entries = await fs.readdir(this.tasksDir, { withFileTypes: true });
      
      for (const entry of entries) {
        if (entry.isFile() && entry.name.endsWith('.json')) {
          const content = await fs.readFile(path.join(this.tasksDir, entry.name), 'utf8');
          const task = JSON.parse(content);
          this.items.set(task.id, task);
        }
      }
      
      console.log(`✅ 已加载 ${this.items.size} 个任务`);
    } catch (err) {
      console.error(`❌ 加载任务失败: ${err.message}`);
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
          },
          {
            type: "function",
            function: {
              name: "task",
              description: "Manage tasks. Create, update, list, or get available tasks.",
              parameters: {
                type: "object",
                properties: {
                  action: { 
                    type: "string",
                    enum: ["create", "update", "list", "get_available"]
                  },
                  id: { 
                    type: "string",
                    description: "Task ID for update action"
                  },
                  text: { 
                    type: "string",
                    description: "Task text for create/update action"
                  }
                },
                required: ["action"],
              },
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
            console.log(`\n📝 Managing tasks: ${args.action}`);
            const taskManager = new TaskManager(TASKS_DIR);
            await taskManager.load();
            
            switch (args.action) {
              case "create":
                result = await taskManager.create(args.text);
                break;
                
              case "update":
                result = await taskManager.update(args.id, args.status);
                break;
                
              case "list":
                result = JSON.stringify(taskManager.list());
                break;
                
              case "get_available":
                result = JSON.stringify(taskManager.getAvailable());
                break;
                
              default:
                result = `Error: Unknown task action: ${args.action}`;
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
  console.log("=== AI 命令行助手 (S07 - 任务系统版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("任务目录:", TASKS_DIR);
  console.log("输入 q / exit 退出\n");

  const taskManager = new TaskManager(TASKS_DIR);
  await taskManager.load();

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms07 >> \x1b[0m", async (input) => {
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

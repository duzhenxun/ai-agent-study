// AI Agent 代码助手 - S11版本
// 在S10基础上增加了自治智能体功能
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
const TEAM_DIR = path.join(WORK_DIR, '.team');
const TASKS_DIR = path.join(WORK_DIR, '.tasks');
const SYSTEM_PROMPT = `You are a team lead at ${WORK_DIR}. Teammates are autonomous.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

// 队友管理
let teammates = new Map();

/**
 * Teammate 智能体队友
 */
class Teammate {
  constructor(id, name, status = 'IDLE') {
    this.id = id;
    this.name = name;
    this.status = status; // IDLE/WORKING/SHUTDOWN
    this.process = null;
    this.lastActivity = Date.now();
  }

  /**
   * Start 启动队友进程
   */
  async start() {
    console.log(`🚀 启动队友 ${this.id} (${this.name})`);
    
    const process = exec(`node s11.js --mode=teammate --id=${this.id} --name=${this.name}`, {
      cwd: WORK_DIR,
      detached: true,
      stdio: 'ignore'
    });

    this.process = process;
    this.status = 'WORKING';
    this.lastActivity = Date.now();
  }

  /**
   * Stop 停止队友
   */
  async stop() {
    if (this.process) {
      this.process.kill('SIGTERM');
      this.process = null;
      this.status = 'SHUTDOWN';
      console.log(`🛑 停止队友 ${this.id} (${this.name})`);
    }
  }

  /**
   * Update 更新状态
   */
  updateStatus(status) {
    this.status = status;
    this.lastActivity = Date.now();
  }

  /**
   * Get 获取状态
   */
  getStatus() {
    return {
      id: this.id,
      name: this.name,
      status: this.status,
      lastActivity: this.lastActivity,
      process: this.process ? 'running' : 'stopped'
    };
  }
}

/**
 * TaskManager 任务管理器
 */
class TaskManager {
  constructor() {
    this.tasks = new Map();
  }

  /**
   * Load 加载任务
   */
  async load() {
    try {
      await fs.mkdir(TASKS_DIR, { recursive: true });
      const entries = await fs.readdir(TASKS_DIR, { withFileTypes: true });
      
      for (const entry of entries) {
        if (entry.isFile() && entry.name.endsWith('.json')) {
          const content = await fs.readFile(path.join(TASKS_DIR, entry.name), 'utf8');
          const task = JSON.parse(content);
          this.tasks.set(task.id, task);
        }
      }
      
      console.log(`✅ 已加载 ${this.tasks.size} 个任务`);
    } catch (err) {
      console.error(`❌ 加载任务失败: ${err.message}`);
    }
  }

  /**
   * Get 获取未认领的任务
   */
  getUnclaimedTasks() {
    return Array.from(this.tasks.values()).filter(task => task.status === 'pending');
  }

  /**
   * Claim 认领任务
   */
  async claimTask(taskId) {
    const task = this.tasks.get(taskId);
    if (!task) {
      return { success: false, error: `Task ${taskId} not found` };
    }

    task.status = 'in_progress';
    this.tasks.set(taskId, task);
    
    console.log(`🎯 队友认领任务 ${taskId}: ${task.text}`);
    return { success: true, message: `Claimed task ${taskId}` };
  }

  /**
   * Complete 完成任务
   */
  async completeTask(taskId) {
    const task = this.tasks.get(taskId);
    if (!task) {
      return { success: false, error: `Task ${taskId} not found` };
    }

    task.status = 'completed';
    this.tasks.set(taskId, task);
    
    console.log(`✅ 队友完成任务 ${taskId}: ${task.text}`);
    return { success: true, message: `Completed task ${taskId}` };
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
              name: "claim_task",
              description: "Claim an unclaimed task.",
              parameters: {
                type: "object",
                properties: {
                  task_id: { 
                    type: "string",
                    description: "ID of the task to claim"
                  }
                },
                required: ["task_id"],
              },
            },
          },
          {
            type: "function",
            function: {
              name: "complete_task",
              description: "Complete a claimed task.",
              parameters: {
                type: "object",
                properties: {
                  task_id: { 
                    type: "string",
                    description: "ID of the task to complete"
                  }
                },
                required: ["task_id"],
              },
            },
          }
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
async function agentLoop(messages, taskManager) {
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
            
          case "claim_task":
            console.log(`\n🎯 认领任务: ${args.task_id}`);
            result = await taskManager.claimTask(args.task_id);
            break;
            
          case "complete_task":
            console.log(`\n✅ 完成任务: ${args.task_id}`);
            result = await taskManager.completeTask(args.task_id);
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
  console.log("=== AI 命令行助手 (S11 - 自治智能体版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("任务目录:", TASKS_DIR);
  console.log("输入 q / exit 退出\n");

  const taskManager = new TaskManager();
  await taskManager.load();

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms11 >> \x1b[0m", async (input) => {
      const query = input.trim();
      if (!query) return ask();

      const lower = query.toLowerCase();
      if (lower === "q" || lower === "exit") {
        rl.close();
        return;
      }

      messages.push({ role: "user", content: query });
      await agentLoop(messages, taskManager);
      ask();
    });
  };

  ask();
}

// 启动
main().catch(console.error);

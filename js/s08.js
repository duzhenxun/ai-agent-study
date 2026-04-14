// AI Agent 代码助手 - S08版本
// 在S07基础上增加了后台任务功能
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
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use background_run for long-running commands.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

// 后台任务管理
let backgroundTasks = new Map();
let taskIdCounter = 0;

/**
 * BackgroundTask 后台任务
 */
class BackgroundTask {
  constructor(id, command) {
    this.id = id;
    this.command = command;
    this.status = 'running'; // running/completed/failed
    this.startTime = Date.now();
    this.endTime = null;
  }

  /**
   * Complete 完成任务
   */
  complete(result) {
    this.status = 'completed';
    this.endTime = Date.now();
    this.result = result;
  }

  /**
   * Fail 任务失败
   */
  fail(error) {
    this.status = 'failed';
    this.endTime = Date.now();
    this.error = error;
  }

  /**
   * Get 任务信息
   */
  getInfo() {
    const duration = this.endTime ? this.endTime - this.startTime : Date.now() - this.startTime;
    return {
      id: this.id,
      command: this.command,
      status: this.status,
      duration: duration,
      result: this.result || null,
      error: this.error || null
    };
  }
}

/**
 * BackgroundManager 后台任务管理器
 */
class BackgroundManager {
  constructor() {
    this.tasks = new Map();
  }

  /**
   * Spawn 启动后台任务
   */
  spawn(command) {
    const id = (++taskIdCounter).toString();
    const task = new BackgroundTask(id, command);
    this.tasks.set(id, task);

    console.log(`🚀 启动后台任务 ${id}: ${command}`);
    
    // 启动子进程
    const child = exec(command, {
      cwd: WORK_DIR,
      detached: true,
      stdio: 'ignore'
    });

    child.on('close', (code) => {
      if (code === 0) {
        task.complete(`Exit code: ${code}`);
      } else {
        task.fail(`Exit code: ${code}`);
      }
      
      console.log(`✅ 后台任务 ${id} 完成:`, task.getInfo());
    });

    child.on('error', (error) => {
      task.fail(error.message);
      console.log(`❌ 后台任务 ${id} 失败:`, task.getInfo());
    });

    return task.id;
  }

  /**
   * GetNotificationQueue 获取通知队列
   */
  getNotificationQueue() {
    const notifications = [];
    
    for (const [id, task] of this.tasks) {
      if (task.status === 'completed' || task.status === 'failed') {
        notifications.push({
          type: 'background_result',
          task_id: id,
          result: task.result || task.error
        });
      }
    }
    
    return notifications;
  }

  /**
   * ClearCompleted 清理已完成的任务
   */
  clearCompleted() {
    for (const [id, task] of this.tasks) {
      if (task.status === 'completed' || task.status === 'failed') {
        this.tasks.delete(id);
      }
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
 * backgroundRun 后台运行命令
 */
async function backgroundRun(command) {
  const backgroundManager = new BackgroundManager();
  const taskId = backgroundManager.spawn(command);
  
  return `Background task started: ${taskId}`;
}

/**
 * 调用 AI 接口
 */
async function chatCompletionsCreate(messages) {
  try {
    const backgroundManager = new BackgroundManager();
    const notifications = backgroundManager.getNotificationQueue();
    
    // 如果有通知，添加到消息中
    if (notifications.length > 0) {
      for (const notification of notifications) {
        messages.push({
          role: "system",
          content: `Background task ${notification.task_id} ${notification.type}: ${notification.result}`,
          timestamp: Date.now()
        });
      }
      
      // 清理已完成的任务
      backgroundManager.clearCompleted();
    }
    
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
              name: "background_run",
              description: "Run a command in the background.",
              parameters: {
                type: "object",
                properties: {
                  command: { type: "string" },
                },
                required: ["command"],
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
            
          case "background_run":
            console.log(`\n🚀 后台运行: ${args.command}`);
            result = backgroundRun(args.command);
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
  console.log("=== AI 命令行助手 (S08 - 后台任务版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("输入 q / exit 退出\n");

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms08 >> \x1b[0m", async (input) => {
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

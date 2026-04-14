// AI Agent 代码助手 - S03版本
// 在S02基础上增加了TODO任务管理功能
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
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}.\nUse the todo tool to plan multi-step tasks. Mark in_progress before starting, completed when done.\nPrefer tools over prose.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

/**
 * TodoItem 表示一个TODO任务项
 */
class TodoItem {
  constructor(id, text, status = 'pending') {
    this.id = id;
    this.text = text;
    this.status = status; // pending/in_progress/completed
  }
}

/**
 * TodoManager 任务管理器，负责管理所有TODO任务
 * 这是S03版本的核心特性，提供了完整的任务管理功能
 * 支持任务的创建、更新、状态管理和持久化存储
 */
class TodoManager {
  constructor() {
    this.items = [];
  }

  /**
   * Update 更新任务列表
   * 这个函数接收新的任务列表，进行验证后更新管理器中的任务
   * 返回更新结果消息和可能的错误
   */
  async update(newItems) {
    // 限制最大任务数量，防止内存溢出
    if (newItems.length > 20) {
      return { success: false, error: "max 20 todos allowed" };
    }

    // 验证和转换任务数据
    const validated = [];
    let inProgressCount = 0; // 统计进行中的任务数量

    for (let i = 0; i < newItems.length; i++) {
      const itemRaw = newItems[i];
      
      // 检查每个任务项是否为有效的对象
      if (typeof itemRaw !== 'object' || itemRaw === null) {
        return { success: false, error: `item ${i + 1} is not a valid object` };
      }

      // 检查必需字段
      if (!itemRaw.id || !itemRaw.text) {
        return { success: false, error: `item ${i + 1} missing required fields` };
      }

      // 验证状态值
      if (!['pending', 'in_progress', 'completed'].includes(itemRaw.status)) {
        return { success: false, error: `item ${i + 1} has invalid status` };
      }

      const item = new TodoItem(itemRaw.id, itemRaw.text, itemRaw.status);
      validated.push(item);

      // 统计进行中的任务数量
      if (item.status === 'in_progress') {
        inProgressCount++;
      }
    }

    // 检查进行中任务数量限制（最多3个）
    if (inProgressCount > 3) {
      return { success: false, error: "max 3 in-progress todos allowed" };
    }

    // 更新任务列表
    this.items = validated;
    
    return { 
      success: true, 
      message: `Updated with ${validated.length} todos (${inProgressCount} in progress)` 
    };
  }

  /**
   * Get 获取任务列表
   * 返回当前管理的所有任务
   */
  getItems() {
    return this.items;
  }

  /**
   * Get 获取指定状态的任务
   */
  getItemsByStatus(status) {
    return this.items.filter(item => item.status === status);
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
              name: "todo",
              description: "Manage TODO tasks. Create, update, or list tasks.",
              parameters: {
                type: "object",
                properties: {
                  action: { 
                    type: "string",
                    enum: ["create", "update", "list"]
                  },
                  items: {
                    type: "array",
                    description: "Array of todo items for update action",
                    items: {
                      type: "object",
                      properties: {
                        id: { type: "string" },
                        text: { type: "string" },
                        status: { 
                          type: "string", 
                          enum: ["pending", "in_progress", "completed"]
                        }
                      },
                      required: ["id", "text", "status"]
                    }
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
            
          case "todo":
            console.log(`\n📝 Managing todos: ${args.action}`);
            const todoManager = new TodoManager();
            
            switch (args.action) {
              case "list":
                result = JSON.stringify(todoManager.getItems());
                break;
                
              case "update":
                const updateResult = await todoManager.update(args.items);
                if (updateResult.success) {
                  result = updateResult.message;
                } else {
                  result = `Error: ${updateResult.error}`;
                }
                break;
                
              default:
                result = `Error: Unknown todo action: ${args.action}`;
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
  console.log("=== AI 命令行助手 (S03 - TODO管理版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("输入 q / exit 退出\n");

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms03 >> \x1b[0m", async (input) => {
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

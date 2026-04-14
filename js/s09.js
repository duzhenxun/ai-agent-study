// AI Agent 代码助手 - S09版本
// 在S08基础上增加了智能体团队功能
import axios from 'axios';
import { exec } from 'child_process';
import { promisify } from 'util';
import readline from 'readline';
import { config } from 'dotenv';
import { existsSync } from 'fs';
import * as fs from 'fs/promises';
import * as path from 'path';
import { v4 as uuidv4 } from 'uuid';

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
const SYSTEM_PROMPT = `You are a team lead at ${WORK_DIR}. Spawn teammates and communicate via inboxes.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

/**
 * Teammate 智能体队友
 */
class Teammate {
  constructor(id, name, status = 'IDLE') {
    this.id = id;
    this.name = name;
    this.status = status; // IDLE/WORKING/SHUTDOWN
    this.process = null;
  }

  /**
   * Start 启动队友进程
   */
  async start() {
    console.log(`🚀 启动队友 ${this.id} (${this.name})`);
    
    const process = exec(`node s09.js --mode=teammate --id=${this.id} --name=${this.name}`, {
      cwd: WORK_DIR,
      detached: true,
      stdio: 'ignore'
    });

    this.process = process;
    this.status = 'WORKING';
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
   * Get 获取状态
   */
  getStatus() {
    return {
      id: this.id,
      name: this.name,
      status: this.status,
      process: this.process ? 'running' : 'stopped'
    };
  }
}

/**
 * TeamManager 团队管理器
 */
class TeamManager {
  constructor() {
    this.teammates = new Map();
    this.configPath = path.join(TEAM_DIR, 'config.json');
  }

  /**
   * Load 加载团队配置
   */
  async load() {
    try {
      await fs.mkdir(TEAM_DIR, { recursive: true });
      
      const configContent = {
        teammates: [],
        phase: 'setup' // setup/working/idle/shutdown
      };

      // 读取现有配置
      if (existsSync(this.configPath)) {
        const content = await fs.readFile(this.configPath, 'utf8');
        Object.assign(configContent, JSON.parse(content));
      }

      // 保存配置
      await fs.writeFile(this.configPath, JSON.stringify(configContent, null, 2), 'utf8');
      
      console.log(`✅ 团队配置已加载: ${this.teammates.size} 个队友`);
    } catch (err) {
      console.error(`❌ 加载团队配置失败: ${err.message}`);
    }
  }

  /**
   * Add 添加队友
   */
  addTeammate(id, name) {
    const teammate = new Teammate(id, name);
    this.teammates.set(id, teammate);
    console.log(`👥 添加队友: ${id} (${name})`);
  }

  /**
   * Get 获取队友
   */
  getTeammate(id) {
    return this.teammates.get(id);
  }

  /**
   * Update 更新状态
   */
  async updateStatus() {
    try {
      const configContent = await fs.readFile(this.configPath, 'utf8');
      const config = JSON.parse(configContent);
      
      // 更新队友状态
      for (const [id, teammate] of this.teammates) {
        const status = teammate.getStatus();
        const existingTeammate = config.teammates.find(t => t.id === id);
        if (existingTeammate) {
          existingTeammate.status = status;
        } else {
          config.teammates.push({ id, name: teammate.name, status });
        }
      }
      
      await fs.writeFile(this.configPath, JSON.stringify(config, null, 2), 'utf8');
    } catch (err) {
      console.error(`❌ 更新团队状态失败: ${err.message}`);
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
 * sendInboxMessage 发送邮箱消息
 */
async function sendInboxMessage(fromId, toId, message) {
  try {
    const inboxPath = path.join(TEAM_DIR, 'inbox', `${toId}.jsonl`);
    const messageLine = JSON.stringify({
      from: fromId,
      to: toId,
      message: message,
      timestamp: new Date().toISOString()
    });
    
    await fs.mkdir(path.dirname(inboxPath), { recursive: true });
    await fs.appendFile(inboxPath, messageLine + '\n', 'utf8');
    
    console.log(`📧 发送消息: ${fromId} -> ${toId}: ${message}`);
  } catch (err) {
    console.error(`❌ 发送消息失败: ${err.message}`);
  }
}

/**
 * readInboxMessages 读取邮箱消息
 */
async function readInboxMessages(teammateId) {
  try {
    const inboxPath = path.join(TEAM_DIR, 'inbox', `${teammateId}.jsonl`);
    
    if (existsSync(inboxPath)) {
      const content = await fs.readFile(inboxPath, 'utf8');
      const messages = content.split('\n').filter(line => line.trim()).map(line => JSON.parse(line));
      
      // 清空文件（drain-on-read）
      await fs.writeFile(inboxPath, '', 'utf8');
      
      return messages;
    }
    
    return [];
  } catch (err) {
    console.error(`❌ 读取邮箱失败: ${err.message}`);
    return [];
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
              name: "spawn_teammate",
              description: "Spawn a new teammate.",
              parameters: {
                type: "object",
                properties: {
                  name: { type: "string" },
                  id: { type: "string" },
                },
                required: ["name", "id"],
              },
            },
          },
          {
            type: "function",
            function: {
              name: "send_message",
              description: "Send a message to a teammate.",
              parameters: {
                type: "object",
                properties: {
                  to: { type: "string" },
                  message: { type: "string" },
                },
                required: ["to", "message"],
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
async function agentLoop(messages, teamManager) {
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
            
          case "spawn_teammate":
            console.log(`\n👥 生成队友: ${args.name} (${args.id})`);
            teamManager.addTeammate(args.id, args.name);
            result = `Teammate ${args.id} (${args.name}) spawned`;
            break;
            
          case "send_message":
            console.log(`\n📧 发送消息: ${args.to}: ${args.message}`);
            result = await sendInboxMessage('lead', args.to, args.message);
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
  const mode = process.argv[2];
  const id = process.argv[3];
  const name = process.argv[4];

  if (mode === '--teammate' && id && name) {
    console.log(`=== AI 智能体队友模式 ===`);
    console.log(`队友ID: ${id}, 名称: ${name}`);
    
    const teamManager = new TeamManager();
    await teamManager.load();
    
    const teammate = new Teammate(id, name);
    await teammate.start();
    
    // 队友主循环
    while (true) {
      try {
        const messages = await readInboxMessages(id);
        if (messages.length > 0) {
          const msg = await chatCompletionsCreate(messages);
          if (msg) {
            console.log(`\n📨 收到消息: ${msg.content}`);
            
            // 处理消息
            for (const tool of msg.tool_calls || []) {
              const args = JSON.parse(tool.function.arguments);
              let result;
              
              switch (tool.function.name) {
                case "bash":
                  console.log(`\n\x1b[33m$ ${args.command}\x1b[0m`);
                  result = await runBash(args.command);
                  break;
                  
                case "send_message":
                  console.log(`\n📧 转发消息: ${args.to}: ${args.message}`);
                  result = await sendInboxMessage(id, args.to, args.message);
                  break;
                  
                default:
                  result = `Unknown tool: ${tool.function.name}`;
              }
              
              // 发送回复
              if (result) {
                await sendInboxMessage(id, 'lead', result);
              }
            }
          }
        }
        
        // 更新状态
        await teamManager.updateStatus();
        
        // 检查是否应该关闭
        const configContent = await fs.readFile(teamManager.configPath, 'utf8');
        const config = JSON.parse(configContent);
        
        if (config.phase === 'shutdown') {
          console.log(`🛑 接收到关闭信号，停止队友 ${id}`);
          await teammate.stop();
          break;
        }
        
        await new Promise(resolve => setTimeout(resolve, 1000));
      } catch (err) {
        console.error(`❌ 队友循环错误: ${err.message}`);
      }
    }
  } else {
    console.log("=== AI 命令行助手 (S09 - 智能体团队版本) ===");
    console.log("模型:", MODEL_ID);
    console.log("输入 q / exit 退出\n");
    console.log("使用方法:");
    console.log("  领导模式: node s09.js");
    console.log("  队友模式: node s09.js --teammate --id=<id> --name=<name>");
    console.log("  退出: 在队友模式下按 Ctrl+C");

    const messages = [{ role: "system", content: SYSTEM_PROMPT }];

    const rl = readline.createInterface({
      input: process.stdin,
      output: process.stdout,
    });

    const ask = () => {
      rl.question("\x1b[36ms09 >> \x1b[0m", async (input) => {
        const query = input.trim();
        if (!query) return ask();

        const lower = query.toLowerCase();
        if (lower === "q" || lower === "exit") {
          rl.close();
          return;
        }

        messages.push({ role: "user", content: query });
        await agentLoop(messages, new TeamManager());
        ask();
      });
    };

  ask();
}
}
// 启动
main().catch(console.error);

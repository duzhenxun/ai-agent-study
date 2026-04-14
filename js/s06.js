// AI Agent 代码助手 - S06版本
// 在S05基础上增加了上下文压缩功能
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
const TRANSCRIPT_DIR = path.join(WORK_DIR, '.transcripts');
const KEEP_RECENT = 3;
const THRESHOLD = 50000;
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use tools to solve tasks.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

// 消息历史记录
let messages = [];

/**
 * microCompact 微压缩
 * 替换3轮之前的tool_result为占位符
 */
function microCompact() {
  const now = Date.now();
  
  for (let i = messages.length - 1; i >= 0; i--) {
    const msg = messages[i];
    if (msg.role === 'tool' && now - msg.timestamp > 3 * 60 * 1000) {
      messages[i] = {
        role: 'tool',
        content: `[Previous: used ${msg.tool_name}]`,
        timestamp: msg.timestamp
      };
    }
  }
}

/**
 * saveTranscript 保存对话记录
 * 将当前对话保存到transcripts目录
 */
async function saveTranscript() {
  try {
    const timestamp = new Date().toISOString().replace(/[:.]/g, '-');
    const filename = `conversation_${timestamp}.jsonl`;
    const filepath = path.join(TRANSCRIPT_DIR, filename);
    
    // 确保transcripts目录存在
    await fs.mkdir(TRANSCRIPT_DIR, { recursive: true });
    
    // 保存对话记录
    const content = messages.map(msg => 
      JSON.stringify({ 
        timestamp: new Date(msg.timestamp).toISOString(),
        role: msg.role,
        content: msg.content,
        tool_calls: msg.tool_calls
      })
    ).join('\n');
    
    await fs.writeFile(filepath, content, 'utf8');
    console.log(`📝 已保存对话记录: ${filename}`);
  } catch (err) {
    console.error(`❌ 保存对话记录失败: ${err.message}`);
  }
}

/**
 * autoCompact 自动压缩
 * 当token数量超过阈值时，使用LLM压缩对话
 */
async function autoCompact() {
  try {
    console.log(`🔄 开始自动压缩，当前消息数: ${messages.length}`);
    
    const response = await axios({
      method: "POST",
      url: OPENAI_BASE_URL,
      headers: {
        "Authorization": `Bearer ${OPENAI_API_KEY}`,
        "Content-Type": "application/json",
      },
      data: {
        model: MODEL_ID,
        messages: [
          { 
            role: "system", 
            content: `Please summarize the following conversation in a concise way. Keep only the most important information and decisions made. Original messages:\n\n${messages.map(msg => `${msg.role}: ${msg.content}`).join('\n\n')}\n\nSummary:` 
          }
        ],
        temperature: 0.3,
      },
      timeout: 120000,
    });

    if (response.data?.choices?.length === 0) {
      throw new Error("No choices in response");
    }

    const summary = response.data.choices[0].message.content;
    console.log(`📋 压缩完成: ${summary}`);
    
    // 用压缩后的摘要替换所有消息
    messages = [
      { 
        role: "system", 
        content: SYSTEM_PROMPT,
        timestamp: Date.now()
      },
      { 
        role: "user", 
        content: "Conversation was auto-compacted to save context space.",
        timestamp: Date.now()
      },
      { 
        role: "assistant", 
        content: summary,
        timestamp: Date.now()
      }
    ];
    
    await saveTranscript();
  } catch (err) {
    console.error(`❌ 自动压缩失败: ${err.message}`);
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
    
    // 限制输出长度
    if (output.length > THRESHOLD) {
      output = output.slice(0, THRESHOLD) + "...";
    }
    
    // 添加时间戳
    const result = {
      content: output,
      timestamp: Date.now()
    };
    
    return result;
  } catch (err) {
    return { 
      content: `Error: ${err.message || err.toString()}`,
      timestamp: Date.now()
    };
  }
}

/**
 * 调用 AI 接口
 */
async function chatCompletionsCreate() {
  try {
    // 检查是否需要压缩
    let totalTokens = 0;
    for (const msg of messages) {
      totalTokens += msg.content.length;
    }
    
    if (totalTokens > THRESHOLD) {
      await autoCompact();
    } else {
      // 正常情况下进行微压缩
      microCompact();
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
async function agentLoop() {
  while (true) {
    const msg = await chatCompletionsCreate();
    if (!msg) return;

    // 添加时间戳
    msg.timestamp = Date.now();
    messages.push(msg);

    // 没有工具调用 → 直接输出内容并结束
    if (!msg.tool_calls || msg.tool_calls.length === 0) {
      if (msg.content) console.log("\n" + msg.content + "\n");
      
      // 保存对话记录
      await saveTranscript();
      return;
    }

    // 执行所有工具调用
    for (const tool of msg.tool_calls) {
      try {
        const args = JSON.parse(tool.function.arguments);
        const command = args.command?.trim() || "";

        console.log(`\n\x1b[33m$ ${command}\x1b[0m`);
        
        const result = await runBash(command);
        
        // 输出预览
        if (result.content.length > 200) {
          console.log(result.content.slice(0, 200) + "...");
        } else {
          console.log(result.content);
        }

        // 把结果回传给 AI
        messages.push({
          role: "tool",
          tool_call_id: tool.id,
          content: result.content,
          timestamp: Date.now()
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
  console.log("=== AI 命令行助手 (S06 - 上下文压缩版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("转录目录:", TRANSCRIPT_DIR);
  console.log("输入 q / exit 退出\n");

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms06 >> \x1b[0m", async (input) => {
      const query = input.trim();
      if (!query) return ask();

      const lower = query.toLowerCase();
      if (lower === "q" || lower === "exit") {
        // 退出前保存对话记录
        await saveTranscript();
        rl.close();
        return;
      }

      messages.push({ 
        role: "user", 
        content: query,
        timestamp: Date.now()
      });
      
      await agentLoop();
      ask();
    });
  };

  ask();
}

// 启动
main().catch(console.error);

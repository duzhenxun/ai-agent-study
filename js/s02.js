// AI Agent 代码助手 - S02版本
// 在S01基础上增加了安全路径检查和工具系统
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
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use tools to solve tasks. Act, don't explain.`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

// 危险命令拦截
const DANGEROUS_COMMANDS = ["rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"];

/**
 * safePath 解析路径并确保它在工作目录内
 * 这是S02版本的重要安全特性，防止路径遍历攻击
 * 确保所有文件操作都在允许的工作目录范围内进行
 */
function safePath(p) {
  // 获取工作目录的绝对路径
  const workdirAbs = path.resolve(WORK_DIR);
  
  // 获取目标路径的绝对路径
  const absPath = path.resolve(p);
  
  // 检查目标路径是否在工作目录内
  // 这是防止路径遍历攻击的关键检查
  if (!absPath.startsWith(workdirAbs)) {
    throw new Error(`path escapes workspace: ${p}`);
  }
  
  return absPath;
}

/**
 * runBash 执行shell命令
 * 相比S01，增加了危险命令检查和超时控制
 */
async function runBash(command) {
  // 定义危险命令列表，防止执行破坏性操作
  for (const d of DANGEROUS_COMMANDS) {
    if (command.includes(d)) {
      return "Error: Dangerous command blocked";
    }
  }
  
  try {
    // 设置120秒超时，防止命令无限期运行
    const timeoutPromise = new Promise((_, reject) =>
      setTimeout(() => reject(new Error("Timeout (120s)")), 120000)
    );

    const execProcess = execPromise(command, {
      cwd: WORK_DIR,
      shell: "/bin/bash",
    });

    const { stdout, stderr } = await Promise.race([execProcess, timeoutPromise]);
    let output = (stdout + stderr).trim();
    
    if (!output) output = "(no output)";
    
    // 限制输出长度
    if (output.length > 50000) {
      output = output.slice(0, 50000) + "...";
    }
    
    return output;
  } catch (err) {
    return err.message === "Timeout (120s)"
      ? "Error: Timeout (120s)"
      : err.message || err.toString();
  }
}

/**
 * runRead 读取文件内容
 * 这个函数使用safePath确保文件路径安全，然后读取文件内容
 * 支持可选的行数限制参数，防止读取过大的文件
 */
async function runRead(filePath, limit) {
  try {
    // 使用safePath确保文件路径在允许的工作目录内
    const fp = safePath(filePath);
    
    // 读取文件内容
    const content = await fs.readFile(fp, 'utf8');
    let contentStr = content;
    
    // 如果指定了行数限制，截取内容
    if (limit && limit > 0) {
      const lines = contentStr.split('\n');
      if (lines.length > limit) {
        contentStr = lines.slice(0, limit).join('\n');
        contentStr += `\n\n... (truncated, showing first ${limit} lines)`;
      }
    }
    
    return contentStr;
  } catch (err) {
    return `Error: ${err.message}`;
  }
}

/**
 * runWrite 写入内容到文件
 * 这个函数使用safePath确保文件路径安全，然后将内容写入文件
 */
async function runWrite(filePath, content) {
  try {
    // 使用safePath确保文件路径在允许的工作目录内
    const fp = safePath(filePath);
    
    // 创建所有必要的父目录
    await fs.mkdir(path.dirname(fp), { recursive: true });
    
    // 将内容写入文件
    await fs.writeFile(fp, content, 'utf8');
    
    // 返回写入成功的消息
    return `Wrote ${content.length} bytes to ${filePath}`;
  } catch (err) {
    return `Error: ${err.message}`;
  }
}

/**
 * runEdit 替换文件中的文本
 * 这个函数在文件中查找指定的旧文本，并替换为新文本
 * 只替换第一个匹配项，确保精确的文本替换
 */
async function runEdit(filePath, oldText, newText) {
  try {
    // 使用safePath确保文件路径在允许的工作目录内
    const fp = safePath(filePath);
    
    // 读取文件内容
    const content = await fs.readFile(fp, 'utf8');
    
    // 检查旧文本是否存在于文件中
    if (!content.includes(oldText)) {
      return `Error: Text not found in ${filePath}`;
    }
    
    // 执行文本替换（只替换第一个匹配项）
    const newContent = content.replace(oldText, newText);
    await fs.writeFile(fp, newContent, 'utf8');
    
    // 返回编辑成功的消息
    return `Edited ${filePath}`;
  } catch (err) {
    return `Error: ${err.message}`;
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
              name: "read_file",
              description: "Read file contents with optional line limit.",
              parameters: {
                type: "object",
                properties: {
                  path: { type: "string" },
                  limit: { 
                    type: "integer",
                    description: "Optional line limit to prevent reading large files"
                  }
                },
                required: ["path"],
              },
            },
          },
          {
            type: "function",
            function: {
              name: "write_file",
              description: "Write content to a file.",
              parameters: {
                type: "object",
                properties: {
                  path: { type: "string" },
                  content: { type: "string" },
                },
                required: ["path", "content"],
              },
            },
          },
          {
            type: "function",
            function: {
              name: "edit_file",
              description: "Replace text in a file.",
              parameters: {
                type: "object",
                properties: {
                  path: { type: "string" },
                  old_text: { type: "string" },
                  new_text: { type: "string" },
                },
                required: ["path", "old_text", "new_text"],
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
        const command = args.command?.trim() || "";

        let result;
        
        switch (tool.function.name) {
          case "bash":
            console.log(`\n\x1b[33m$ ${command}\x1b[0m`);
            result = await runBash(command);
            break;
            
          case "read_file":
            console.log(`\n📖 Reading file: ${args.path}`);
            result = await runRead(args.path, args.limit);
            break;
            
          case "write_file":
            console.log(`\n📝 Writing to file: ${args.path}`);
            result = await runWrite(args.path, args.content);
            break;
            
          case "edit_file":
            console.log(`\n✏️  Editing file: ${args.path}`);
            result = await runEdit(args.path, args.old_text, args.new_text);
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
  console.log("=== AI 命令行助手 (S02 - 安全工具版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("输入 q / exit 退出\n");

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms02 >> \x1b[0m", async (input) => {
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

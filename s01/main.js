import axios from 'axios';
import { exec } from 'child_process';
import { promisify } from 'util';
import readline from 'readline';

// 执行 exec 的 promise 版本
    // npm init -y
    // npm install axios
    // node main.js

const execPromise = promisify(exec);

// ====================== 直接写死配置（简化版）======================
const MODEL_ID = "deepseek-chat";
const OPENAI_BASE_URL = "https://ai.0532888.cn/openai/chat/completions";
const OPENAI_API_KEY = "zane";
// ==================================================================

const WORK_DIR = process.cwd();
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}. Use bash to solve tasks. Act, don't explain.`;

// 危险命令拦截
const DANGEROUS_COMMANDS = ["rm -rf /", "sudo", "shutdown", "reboot", "> /dev/"];

/**
 * 执行 bash 命令
 */
async function runBash(command) {
  // 拦截危险命令
  for (const cmd of DANGEROUS_COMMANDS) {
    if (command.includes(cmd)) {
      return "Error: Dangerous command blocked";
    }
  }

  try {
    // 120 秒超时
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

        console.log(`\n\x1b[33m$ ${command}\x1b[0m`);
        const output = await runBash(command);

        // 输出预览
        if (output.length > 200) {
          console.log(output.slice(0, 200) + "...");
        } else {
          console.log(output);
        }

        // 把结果回传给 AI
        messages.push({
          role: "tool",
          tool_call_id: tool.id,
          content: output,
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
  console.log("=== AI 命令行助手 ===");
  console.log("模型:", MODEL_ID);
  console.log("输入 q / exit 退出\n");

  const messages = [{ role: "system", content: SYSTEM_PROMPT }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms01 >> \x1b[0m", async (input) => {
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
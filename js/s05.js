// AI Agent 代码助手 - S05版本
// 在S04基础上增加了技能加载功能
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
const SKILLS_DIR = path.join(WORK_DIR, 'skills');
const SYSTEM_PROMPT = `You are a coding agent at ${WORK_DIR}.When executing scripts, If you need to use some scripts in the skill, Use load_skill to access specialized knowledge before tackling unfamiliar topics.\n\nSkills available:\n%s\n\nWhen executing scripts, you must include the skill name in the path:skill_name/scripts/xxx, you cannot omit the skill name.\n`;

// 调试：打印环境变量
console.log('=== 调试信息 ===');
console.log('MODEL_ID:', MODEL_ID);
console.log('OPENAI_BASE_URL:', OPENAI_BASE_URL);
console.log('OPENAI_API_KEY:', OPENAI_API_KEY ? '已设置' : '未设置');
// ==================================================================

/**
 * Skill 表示一个技能，包含元数据、主体内容和路径信息
 * 这是S05版本的核心数据结构，用于定义和管理可加载的技能
 */
class Skill {
  constructor(meta, body, skillPath) {
    this.meta = meta;        // 技能元数据（名称、描述、参数等）
    this.body = body;        // 技能主体内容（通常是Markdown格式的说明）
    this.path = skillPath;      // 技能文件路径
  }

  /**
   * 获取技能描述
   */
  getDescription() {
    return this.meta.description || 'No description';
  }
}

/**
 * SkillLoader 技能加载器
 * 负责从指定目录扫描、加载和管理所有技能文件
 */
class SkillLoader {
  constructor(skillsDir) {
    this.skillsDir = skillsDir;
    this.skills = new Map();
  }

  /**
   * Load 加载所有技能
   */
  async load() {
    try {
      // 检查技能目录是否存在
      if (!existsSync(this.skillsDir)) {
        console.log(`⚠️  警告: 技能目录不存在: ${this.skillsDir}`);
        return;
      }

      // 读取技能目录中的所有文件
      const entries = await fs.readdir(this.skillsDir, { withFileTypes: true });
      
      for (const entry of entries) {
        if (entry.isDirectory()) {
          const skillPath = path.join(entry.path, 'SKILL.md');
          
          // 检查技能文件是否存在
          if (existsSync(skillPath)) {
            await this.loadSkill(entry.name, skillPath);
          }
        }
      }
      
      console.log(`✅ 已加载 ${this.skills.size} 个技能`);
    } catch (err) {
      console.error(`❌ 技能加载失败: ${err.message}`);
    }
  }

  /**
   * Load 加载单个技能
   */
  async loadSkill(skillName, skillPath) {
    try {
      const content = await fs.readFile(skillPath, 'utf8');
      const lines = content.split('\n');
      
      let meta = {};
      let body = [];
      let inBody = false;
      
      // 解析YAML前置元数据
      for (const line of lines) {
        const trimmed = line.trim();
        
        if (trimmed === '---') {
          inBody = true;
          continue;
        }
        
        if (trimmed === '...' && inBody) {
          inBody = false;
          continue;
        }
        
        if (inBody && trimmed.includes(':')) {
          const [key, ...value] = trimmed.split(':').map(s => s.trim());
          if (key && value) {
            meta[key] = value.join(':');
          }
        } else if (inBody) {
          body.push(trimmed);
        }
      }
      
      const skill = new Skill(meta, body.join('\n'), skillPath);
      this.skills.set(skillName, skill);
      
    } catch (err) {
      console.error(`❌ 加载技能 ${skillName} 失败: ${err.message}`);
    }
  }

  /**
   * Get 获取技能描述列表
   */
  getDescriptions() {
    const descriptions = [];
    for (const [name, skill] of this.skills) {
      descriptions.push(`- ${name}: ${skill.getDescription()}`);
    }
    return descriptions.join('\n');
  }

  /**
   * Get 获取技能内容
   */
  getSkill(skillName) {
    const skill = this.skills.get(skillName);
    return skill ? skill.body : '';
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
 * loadSkill 加载技能内容
 */
async function loadSkill(skillName) {
  const skillLoader = new SkillLoader(SKILLS_DIR);
  await skillLoader.load();
  return skillLoader.getSkill(skillName);
}

/**
 * 调用 AI 接口
 */
async function chatCompletionsCreate(messages) {
  try {
    const skillLoader = new SkillLoader(SKILLS_DIR);
    await skillLoader.load();
    
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
              name: "load_skill",
              description: "Load a skill's content.",
              parameters: {
                type: "object",
                properties: {
                  skill_name: { 
                    type: "string",
                    description: "Name of the skill to load"
                  }
                },
                required: ["skill_name"],
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
            
          case "load_skill":
            console.log(`\n📚 Loading skill: ${args.skill_name}`);
            result = await loadSkill(args.skill_name);
            if (result) {
              console.log(`\n📋 Skill content loaded successfully`);
            } else {
              result = `Skill ${args.skill_name} not found`;
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
  console.log("=== AI 命令行助手 (S05 - 技能加载版本) ===");
  console.log("模型:", MODEL_ID);
  console.log("技能目录:", SKILLS_DIR);
  console.log("输入 q / exit 退出\n");

  const skillLoader = new SkillLoader(SKILLS_DIR);
  await skillLoader.load();

  const messages = [{ 
    role: "system", 
    content: SYSTEM_PROMPT.replace('%s', skillLoader.getDescriptions())
  }];

  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });

  const ask = () => {
    rl.question("\x1b[36ms05 >> \x1b[0m", async (input) => {
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

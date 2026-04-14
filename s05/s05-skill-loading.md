# s05: Skills (技能加载)

`s01 > s02 > s03 > s04 > [ s05 ] s06 | s07 > s08 > s09 > s10 > s11 > s12`

> _"用到什么知识, 临时加载什么知识"_ -- 通过 tool_result 注入, 不塞 system prompt。
>
> **Harness 层**: 按需知识 -- 模型开口要时才给的领域专长。

## 问题

你希望智能体遵循特定领域的工作流: git 约定、测试模式、代码审查清单。全塞进系统提示太浪费 -- 10 个技能, 每个 2000 token, 就是 20,000 token, 大部分跟当前任务毫无关系。

## 解决方案

```
System prompt (Layer 1 -- always present):
+--------------------------------------+
| You are a coding agent.              |
| Skills available:                    |
|   - git: Git workflow helpers        |  ~100 tokens/skill
|   - test: Testing best practices     |
+--------------------------------------+

When model calls load_skill("git"):
+--------------------------------------+
| tool_result (Layer 2 -- on demand):  |
| <skill name="git">                   |
|   Full git workflow instructions...  |  ~2000 tokens
|   Step 1: ...                        |
| </skill>                             |
+--------------------------------------+
```

第一层: 系统提示中放技能名称 (低成本)。第二层: tool_result 中按需放完整内容。

## 工作原理

### 系统提示

```
You are a coding agent at %s.When executing scripts, If you need to use some scripts in the skill, Use load_skill to access specialized knowledge before tackling unfamiliar topics.

Skills available:
%s

When executing scripts, you must include the skill name in the path:skill_name/scripts/xxx, you cannot omit the skill name.
```

1. 每个技能是一个目录, 包含 `SKILL.md` 文件和 YAML frontmatter。

```
skills/
  pdf/
    SKILL.md       # ---\n name: pdf\n description: Process PDF files\n ---\n ...
  code-review/
    SKILL.md       # ---\n name: code-review\n description: Review code\n ---\n ...
```

2. SkillLoader 递归扫描 `SKILL.md` 文件, 用目录名作为技能标识。

```go
// SkillLoader 技能加载器
type SkillLoader struct {
	skillsDir string           // 技能目录路径
	skills    map[string]Skill // 已加载的技能映射表
}

// Skill 表示一个技能，包含元数据、主体内容和路径信息
type Skill struct {
	Meta map[string]string // 技能元数据（名称、描述、参数等）
	Body string            // 技能主体内容（通常是Markdown格式的说明）
	Path string            // 技能文件路径
}

// NewSkillLoader 创建新的技能加载器
// 初始化技能加载器并自动加载所有可用技能
func NewSkillLoader(skillsDir string) *SkillLoader {
	// 创建技能加载器实例
	sl := &SkillLoader{
		skillsDir: skillsDir,              // 设置技能目录
		skills:    make(map[string]Skill), // 初始化技能映射表
	}
	// 自动加载所有技能
	sl.loadAll()
	return sl
}

// getDescriptions 获取所有技能的描述列表
func (sl *SkillLoader) getDescriptions() string {
	var lines []string
	for name, skill := range sl.skills {
		desc := skill.Meta["description"]
		lines = append(lines, fmt.Sprintf("  - %s: %s", name, desc))
	}
	return strings.Join(lines, "\n")
}

// getContent 获取指定技能的完整内容
func (sl *SkillLoader) getContent(name string) string {
	skill, ok := sl.skills[name]
	if !ok {
		return fmt.Sprintf("Error: Unknown skill '%s'", name)
	}
	return fmt.Sprintf("<skill name=\"%s\">%s</skill>", name, skill.Body)
}
```

3. 第一层写入系统提示。第二层不过是 dispatch map 中的又一个工具。

```go
func initSystem() {
	system = fmt.Sprintf("You are a coding agent at %s.\nSkills available:\n%s", workdir, skillLoader.getDescriptions())
}

// Tool Handlers Map
var toolHandlers = map[string]interface{}{
	// ...base tools...
	"load_skill": func(args map[string]interface{}) string {
		name := args["name"].(string)
		return skillLoader.getContent(name)
	},
}
```

模型知道有哪些技能 (便宜), 需要时再加载完整内容 (贵)。

## 相对 s04 的变更

| 组件     | 之前 (s04)      | 之后 (s05)               |
| -------- | --------------- | ------------------------ |
| Tools    | 5 (基础 + task) | 5 (基础 + load_skill)    |
| 系统提示 | 静态字符串      | + 技能描述列表           |
| 知识库   | 无              | skills/\*/SKILL.md 文件  |
| 注入方式 | 无              | 两层 (系统提示 + result) |

## 试一试

```sh
cd ai-agent-study/s05
go run main.go
```

试试这些 prompt (英文 prompt 对 LLM 效果更好, 也可以用中文):

1. `What skills are available?`
2. `Load the agent-builder skill and follow its instructions`
3. `I need to do a code review -- load the relevant skill first`
4. `Build an MCP server using the mcp-builder skill`

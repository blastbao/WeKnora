// Package skills provides Agent Skills functionality following Claude's Progressive Disclosure pattern.
// Skills are modular capabilities that extend the agent's functionality through instruction files.

// Package skills 实现了基于“渐进式披露”（Progressive Disclosure）模式的 Agent 技能系统。
//
// 本包遵循 Claude 技能规范，将技能定义为模块化的能力扩展，通过指令文件（SKILL.md）和附加资源文件来增强 Agent 功能。
// 技能加载分为三个层级：
//   - Level 1 (元数据): 仅加载名称和描述，用于系统提示词注入和技能发现。
//   - Level 2 (指令): 按需加载 SKILL.md 的主体指令内容。
//   - Level 3 (资源): 按需加载技能目录下的附加文件（如脚本、表单等）。

// 提示词生成器与大模型各自承担的职责。
//
// | 步骤 | 执行者 (Who)            | 动作描述 (Action)                                   | 关键点 (Key Point)           |
// | :--- | :---------------------| :------------------------------------------------- | :--------------------------- |
// | 1    | 后端代码               | 注册并告诉模型有哪些工具可用 (如 read_skill)。           | 提供“武器库” (Tool Definitions) |
// | 2    | formatSkillsMetadata  | 生成提示词，规定模型何时及为何使用工具 (匹配协议)。        | 提供“作战手册” (Prompt Logic)   |
// | 3    | 大模型 (LLM)           | 语义理解用户问题，对比技能描述，识别潜在匹配项。           | 核心智能：语义匹配 (Matching)   |
// | 4    | 大模型 (LLM)           | 依据“作战手册”逻辑，决定发起工具调用请求。                | 输出函数调用指令 (Function Call)|
// | 5    | 后端代码               | 拦截调用请求，执行实际逻辑 (读文件/查库)，返回结果。       | 闭环执行 (Execution & Feedback) |

package skills

import (
	"bufio"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill validation constants following Claude's specification
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
	SkillFileName        = "SKILL.md"
)

// Reserved words that cannot be used in skill names
var reservedWords = []string{"anthropic", "claude"}

// namePattern validates skill names: unicode letters, numbers only
var namePattern = regexp.MustCompile(`^[\p{L}\p{N}-]+$`)

// xmlTagPattern detects XML tags in content
var xmlTagPattern = regexp.MustCompile(`<[^>]+>`)

// Skill 代表一个已加载的技能，包含其元数据、指令和文件系统信息。
//
// 该结构体遵循“渐进式披露”模式：
//   - Level 1 (Metadata): Name 和 Description 始终加载，用于快速索引。
//   - Level 2 (Instructions): 主体指令内容，仅在需要执行技能逻辑时加载。
//   - Level 3 (Resources): 通过 SkillFile 结构体单独管理，按需加载。

// Skill represents a loaded skill with its metadata and content
// It follows the Progressive Disclosure pattern:
// - Level 1 (Metadata): Name and Description are always loaded
// - Level 2 (Instructions): The main body of SKILL.md, loaded on demand
// - Level 3 (Resources): Additional files in the skill directory, loaded as needed
type Skill struct {
	// Metadata (Level 1) - always loaded
	Name        string `yaml:"name"`        // 技能唯一标识符
	Description string `yaml:"description"` // 技能的简短描述

	// Filesystem information
	BasePath string // 技能目录的绝对路径
	FilePath string // SKILL.md 文件的绝对路径

	// Instructions (Level 2) - loaded on demand
	Instructions string // SKILL.md 的主体内容（去除 Frontmatter 后）
	Loaded       bool   // 标记 Level 2 指令是否已加载
}

// SkillMetadata 代表技能的轻量级元数据表示 (Level 1)。
//
// 专用于系统提示词注入和技能发现阶段，不包含庞大的指令内容，以优化内存使用和启动性能。

// SkillMetadata represents the minimal metadata for system prompt injection (Level 1)
// This is the lightweight representation used during skill discovery
type SkillMetadata struct {
	Name        string // 技能名称
	Description string // 技能描述
	BasePath    string // 技能目录路径，用于后续加载完整内容
}

// SkillFile 代表技能目录内的附加文件 (Level 3)。
//
// 这些文件可以是文档、配置文件或可执行脚本，按需加载。

// SkillFile represents an additional file within a skill directory (Level 3)
type SkillFile struct {
	Name     string // 文件名 (e.g., "FORMS.md", "scripts/validate.py")
	Path     string // 文件的绝对路径
	Content  string // 文件内容
	IsScript bool   // 标记该文件是否为可执行脚本
}

// Validate 检查技能元数据是否符合 Claude 规范。
//
// 该函数对 Skill 结构体中的 Name 和 Description 进行严格校验，验证规则：
// 1. 名称验证：
//    - 不能为空
//    - 长度不超过64字符
//    - 只能包含字母、数字和连字符
//    - 不能包含 "anthropic" 或 "claude" 等保留字
//    - 不能包含XML标签
// 2. 描述验证：
//    - 不能为空
//    - 长度不超过1024字符
//    - 不能包含XML标签
//
// 参数：
//   - 无
//
// 返回：
//   - error: 验证失败返回错误，成功返回nil

// Validate checks if the skill metadata is valid according to Claude's specification
func (s *Skill) Validate() error {
	// Validate name
	if s.Name == "" {
		return errors.New("skill name is required")
	}
	if len(s.Name) > MaxNameLength {
		return fmt.Errorf("skill name exceeds maximum length of %d characters", MaxNameLength)
	}
	if !namePattern.MatchString(s.Name) {
		return errors.New("skill name must contain only lowercase letters, numbers, and hyphens")
	}
	for _, reserved := range reservedWords {
		if strings.Contains(s.Name, reserved) {
			return fmt.Errorf("skill name cannot contain reserved word: %s", reserved)
		}
	}
	if xmlTagPattern.MatchString(s.Name) {
		return errors.New("skill name cannot contain XML tags")
	}

	// Validate description
	if s.Description == "" {
		return errors.New("skill description is required")
	}
	if len(s.Description) > MaxDescriptionLength {
		return fmt.Errorf("skill description exceeds maximum length of %d characters", MaxDescriptionLength)
	}
	if xmlTagPattern.MatchString(s.Description) {
		return errors.New("skill description cannot contain XML tags")
	}

	return nil
}

// ToMetadata
//
// 将完整的 Skill 对象转换为其轻量级元数据表示 (Level 1)。
// 用于技能发现阶段，仅提取 Name、Description 和 BasePath，
// 丢弃庞大的 Instructions 内容，以减少内存占用并提高系统启动速度。
//
// 返回值：
//   - *SkillMetadata: 包含最小必要信息的元数据对象。

// ToMetadata converts a Skill to its lightweight metadata representation
func (s *Skill) ToMetadata() *SkillMetadata {
	return &SkillMetadata{
		Name:        s.Name,
		Description: s.Description,
		BasePath:    s.BasePath,
	}
}

// ParseSkillFile 解析 SKILL.md 文件内容，提取元数据和主体指令。
//
// 该函数处理符合 YAML Frontmatter 规范的文件格式：
// 	1. 检查文件格式：
//    - 验证文件是否以 YAML frontmatter 开头（---），并以 "---" 结束 Frontmatter 部分。
// 	2. 分离 Frontmatter 和 Body ：
//    - 扫描文件内容，分离 YAML 前端部分和主体部分
// 	3. 解析YAML：
//    - 解析 YAML frontmatter 到 Skill 结构体（Name, Description）。
// 	4. 提取指令内容：
//    - 设置 Instructions 字段为文件主体内容。
// 	5. 验证技能：
//    - 调用Validate()方法进行合规性检查。
// 	6. 状态标记：
//	  - 设置 Loaded = true，表示 Level 2 内容已加载。
//
// 参数：
//   - content: SKILL.md 文件的完整字符串内容。
//
// 返回值：
//   - *Skill: 解析后的技能对象。
//   - error: 若格式错误、YAML 解析失败或校验不通过，返回相应错误。

// ParseSkillFile parses a SKILL.md file content and extracts metadata and body
// It handles YAML frontmatter enclosed in --- delimiters
func ParseSkillFile(content string) (*Skill, error) {
	skill := &Skill{}

	// Check for YAML frontmatter
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return nil, errors.New("SKILL.md must start with YAML frontmatter (---)")
	}

	// Find the end of frontmatter
	scanner := bufio.NewScanner(strings.NewReader(content))
	var frontmatterLines []string
	var bodyLines []string
	inFrontmatter := false
	frontmatterEnded := false

	for scanner.Scan() {
		line := scanner.Text()

		if !inFrontmatter && !frontmatterEnded && strings.TrimSpace(line) == "---" {
			inFrontmatter = true
			continue
		}

		if inFrontmatter && strings.TrimSpace(line) == "---" {
			inFrontmatter = false
			frontmatterEnded = true
			continue
		}

		if inFrontmatter {
			frontmatterLines = append(frontmatterLines, line)
		} else if frontmatterEnded {
			bodyLines = append(bodyLines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading SKILL.md: %w", err)
	}

	if !frontmatterEnded {
		return nil, errors.New("SKILL.md frontmatter is not properly closed with ---")
	}

	// Parse YAML frontmatter
	frontmatter := strings.Join(frontmatterLines, "\n")
	if err := yaml.Unmarshal([]byte(frontmatter), skill); err != nil {
		return nil, fmt.Errorf("failed to parse YAML frontmatter: %w", err)
	}

	// Set body instructions
	skill.Instructions = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	skill.Loaded = true

	// Validate
	if err := skill.Validate(); err != nil {
		return nil, fmt.Errorf("skill validation failed: %w", err)
	}

	return skill, nil
}

// ParseSkillMetadata 仅从 SKILL.md 文件内容中解析元数据 (Level 1)。
//
// 这是一个轻量级操作，专为技能发现（Discovery）阶段设计。
// 它内部调用 ParseSkillFile 进行完整解析，然后立即通过 ToMetadata() 提取最小信息集，
// 避免在不需要指令内容时消耗过多内存。
//
//
// 解析流程：
// 1. 调用ParseSkillFile解析完整文件
// 2. 提取元数据部分
// 3. 返回SkillMetadata结构
//
// 参数：
//   - content: SKILL.md 文件的完整字符串内容。
//
// 返回值：
//   - *SkillMetadata: 仅包含 Name, Description, BasePath 的元数据对象。
//   - error: 若底层解析失败，返回相应错误。

// ParseSkillMetadata parses only the metadata from a SKILL.md file content
// This is a lightweight operation for skill discovery (Level 1 only)
func ParseSkillMetadata(content string) (*SkillMetadata, error) {
	skill, err := ParseSkillFile(content)
	if err != nil {
		return nil, err
	}
	return skill.ToMetadata(), nil
}

// IsScript
//
// 功能说明：
// 	检查文件路径是否表示可执行脚本
// 	通过文件扩展名判断是否为脚本文件（忽略大小写）
//
// 支持的脚本扩展名：
//   - .py: Python脚本
//   - .sh/.bash: Shell脚本
//   - .js: JavaScript脚本
//   - .ts: TypeScript脚本
//   - .rb: Ruby脚本
//   - .pl: Perl脚本
//   - .php: PHP脚本
//
//
// 参数：
//   - path: 文件的路径（绝对或相对）。
//
// 返回值：
//   - bool: 如果是支持的脚本类型，返回 true；否则返回 false。

// IsScript checks if a file path represents an executable script
func IsScript(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	scriptExtensions := map[string]bool{
		".py":   true,
		".sh":   true,
		".bash": true,
		".js":   true,
		".ts":   true,
		".rb":   true,
		".pl":   true,
		".php":  true,
	}
	return scriptExtensions[ext]
}

// GetScriptLanguage 根据文件路径返回对应的脚本语言/解释器名称。
//
// 功能说明：
// 	获取脚本文件对应的编程语言/解释器
// 	根据文件扩展名映射到相应的语言名称
//
// 扩展名映射表：
//   - .py → "python"
//   - .sh/.bash → "bash"
//   - .js → "node"
//   - .ts → "ts-node"
//   - .rb → "ruby"
//   - .pl → "perl"
//   - .php → "php"
//   - 其他 → "unknown"
//
// 参数：
//   - path: string类型，文件路径
//
// 返回值：
//   - string: 解释器名称（如 "python"）。若扩展名不支持，返回 "unknown"。

// GetScriptLanguage returns the language/interpreter for a script file
func GetScriptLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	languages := map[string]string{
		".py":   "python",
		".sh":   "bash",
		".bash": "bash",
		".js":   "node",
		".ts":   "ts-node",
		".rb":   "ruby",
		".pl":   "perl",
		".php":  "php",
	}
	if lang, ok := languages[ext]; ok {
		return lang
	}
	return "unknown"
}

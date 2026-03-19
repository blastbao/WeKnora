package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 三层加载机制
//	Level 1	DiscoverSkills()		只读取元数据（Frontmatter），轻量级扫描
//	Level 2	LoadSkillInstructions()	加载完整 Skill 说明文档
//	Level 3	LoadSkillFile()			按需加载 Skill 目录下的额外文件

// Level 1: 发现与元数据提取 (DiscoverSkills)
//	功能：扫描所有配置的目录 (skillDirs)，寻找包含 SKILL.md 的子文件夹。
//	行为：
//		只读取 SKILL.md 文件。
//		解析 YAML Frontmatter，提取 Name 和 Description。
//		不加载具体的指令正文 (Instructions)。
//		将结果缓存到 l.discoveredSkills 中（此时 Loaded=false）。
//	目的：快速构建“技能清单”，供 System Prompt 使用。即使有 100 个技能，每个技能有 5000 字指令，这一步也几乎不消耗 Token 和内存。
//
// Level 2: 按需加载指令 (LoadSkillInstructions)
//	触发时机：当 LLM 决定调用某个具体技能时（例如用户说“帮我执行 web-search”）。
//	行为：
//		检查缓存：如果已加载且 Loaded=true，直接返回。
//		如果未加载：去文件系统读取完整的 SKILL.md，解析出 Instructions 正文。
//		更新缓存状态：标记 Loaded=true。
//	智能查找：支持两种查找方式：
//		路径匹配：直接尝试 dir/skillName/SKILL.md（最快）。
//		名称扫描：如果路径不匹配，遍历目录下所有子文件夹，比对 name 字段（容错性强）。
//
// Level 3: 安全加载资源文件 (LoadSkillFile)
//	触发时机：当技能指令中需要读取额外的脚本、模板或文档时（例如 resources/script.py）。
//	关键特性：安全性 (Security)
//		路径清洗：使用 filepath.Clean 标准化路径。
//		防穿越攻击 (Path Traversal)：
//			禁止 .. 开头。
//			禁止绝对路径。
//			双重验证：计算文件的绝对路径，确保它必须以技能目录的绝对路径为前缀。
//			例子：如果技能在 /skills/search，试图读取 /skills/search/../../etc/passwd 会被拦截，因为解析后的真实路径不在 /skills/search 下。
//	自动识别：
//		返回的 SkillFile 对象会自动标记 IsScript 和对应的语言，方便后续执行。

// 流程示例
//
//	假设系统配置了 ./plugins 目录，里面有一个 weather 技能。
//
//	启动时：
//		调用 loader.DiscoverSkills()。
//		扫描到 ./plugins/weather/SKILL.md。
//		读取头部 YAML，得到 {name: "weather", desc: "查询天气"}。
//		存入缓存。此时 Instructions 字段为空。
//
//	用户问：“今天北京天气怎么样？”
//		LLM 分析后决定调用 weather 技能。
//		主程序调用 loader.LoadSkillInstructions("weather")。
//		检测到缓存中 Loaded=false。
//		读取 ./plugins/weather/SKILL.md 全文，解析出详细的 API 调用步骤。
//		更新缓存 Loaded=true。
//		返回完整 Skill 对象给 LLM 执行。
//	执行中：
//		技能指令说：“请运行 scripts/fetch.py”。
//		主程序调用 loader.LoadSkillFile("weather", "scripts/fetch.py")。
//		进行安全校验，确认文件在 ./plugins/weather 内。
//		读取文件内容，标记为 Python 脚本。
//		返回内容供执行器运行。

// Loader handles skill discovery and loading from the filesystem
// It implements the Progressive Disclosure pattern by separating
// metadata discovery (Level 1) from instructions loading (Level 2/3)
type Loader struct {
	// skillDirs are the directories to search for skills
	skillDirs []string
	// discoveredSkills caches discovered skill metadata
	discoveredSkills map[string]*Skill
}

// NewLoader creates a new skill loader with the specified search directories
func NewLoader(skillDirs []string) *Loader {
	return &Loader{
		skillDirs:        skillDirs,
		discoveredSkills: make(map[string]*Skill),
	}
}

// DiscoverSkills scans all configured directories for SKILL.md files
// and extracts their metadata (Level 1). This is a lightweight operation
// that only reads the frontmatter of each skill file.
func (l *Loader) DiscoverSkills() ([]*SkillMetadata, error) {
	var allMetadata []*SkillMetadata

	for _, dir := range l.skillDirs {
		metadata, err := l.discoverInDirectory(dir)
		if err != nil {
			// Log warning but continue with other directories
			continue
		}
		allMetadata = append(allMetadata, metadata...)
	}

	return allMetadata, nil
}

// discoverInDirectory scans a single directory for skill subdirectories
func (l *Loader) discoverInDirectory(dir string) ([]*SkillMetadata, error) {
	var metadata []*SkillMetadata

	// Check if directory exists
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Directory doesn't exist, skip silently
		}
		return nil, fmt.Errorf("failed to access skill directory %s: %w", dir, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	// Read directory entries
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read skill directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillPath, SkillFileName)

		// Check if SKILL.md exists
		if _, err := os.Stat(skillFile); os.IsNotExist(err) {
			continue
		}

		// Read and parse SKILL.md
		content, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		skill, err := ParseSkillFile(string(content))
		if err != nil {
			continue
		}

		// Set filesystem paths
		skill.BasePath = skillPath
		skill.FilePath = skillFile

		// Cache the skill
		l.discoveredSkills[skill.Name] = skill

		metadata = append(metadata, skill.ToMetadata())
	}

	return metadata, nil
}

// LoadSkillInstructions loads the full instructions of a skill (Level 2)
// Returns the cached skill if already loaded
func (l *Loader) LoadSkillInstructions(skillName string) (*Skill, error) {
	// Check cache first
	if skill, ok := l.discoveredSkills[skillName]; ok {
		if skill.Loaded {
			return skill, nil
		}
	}

	// Search for the skill in all directories
	for _, dir := range l.skillDirs {
		skill, err := l.loadSkillFromDirectory(dir, skillName)
		if err == nil {
			l.discoveredSkills[skillName] = skill
			return skill, nil
		}
	}

	return nil, fmt.Errorf("skill not found: %s", skillName)
}

// loadSkillFromDirectory attempts to load a skill from a specific directory
func (l *Loader) loadSkillFromDirectory(dir, skillName string) (*Skill, error) {
	// First, check if we can find by directory name matching skill name
	skillPath := filepath.Join(dir, skillName)
	skillFile := filepath.Join(skillPath, SkillFileName)

	if _, err := os.Stat(skillFile); err == nil {
		return l.loadSkillFile(skillPath, skillFile)
	}

	// Otherwise, scan all subdirectories to find the skill by name
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillPath, SkillFileName)

		if _, err := os.Stat(skillFile); os.IsNotExist(err) {
			continue
		}

		content, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		skill, err := ParseSkillFile(string(content))
		if err != nil {
			continue
		}

		if skill.Name == skillName {
			skill.BasePath = skillPath
			skill.FilePath = skillFile
			return skill, nil
		}
	}

	return nil, fmt.Errorf("skill not found in %s: %s", dir, skillName)
}

// loadSkillFile reads and parses a SKILL.md file
func (l *Loader) loadSkillFile(basePath, filePath string) (*Skill, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read skill file: %w", err)
	}

	skill, err := ParseSkillFile(string(content))
	if err != nil {
		return nil, err
	}

	skill.BasePath = basePath
	skill.FilePath = filePath

	return skill, nil
}

// LoadSkillFile loads an additional file from a skill directory (Level 3)
// The filePath should be relative to the skill's base directory
func (l *Loader) LoadSkillFile(skillName, relativePath string) (*SkillFile, error) {
	// Get the skill first
	skill, ok := l.discoveredSkills[skillName]
	if !ok {
		// Try to load the skill
		var err error
		skill, err = l.LoadSkillInstructions(skillName)
		if err != nil {
			return nil, fmt.Errorf("skill not found: %s", skillName)
		}
	}

	// Validate and resolve the file path
	cleanPath := filepath.Clean(relativePath)

	// Security: prevent path traversal
	if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
		return nil, fmt.Errorf("invalid file path: %s", relativePath)
	}

	fullPath := filepath.Join(skill.BasePath, cleanPath)

	// Verify the file is within the skill directory
	absSkillPath, err := filepath.Abs(skill.BasePath)
	if err != nil {
		return nil, err
	}
	absFilePath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(absFilePath, absSkillPath) {
		return nil, fmt.Errorf("file path outside skill directory: %s", relativePath)
	}

	// Read the file
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return &SkillFile{
		Name:     relativePath,
		Path:     absFilePath, // Use absolute path for sandbox execution
		Content:  string(content),
		IsScript: IsScript(relativePath),
	}, nil
}

// ListSkillFiles lists all files in a skill directory
func (l *Loader) ListSkillFiles(skillName string) ([]string, error) {
	skill, ok := l.discoveredSkills[skillName]
	if !ok {
		var err error
		skill, err = l.LoadSkillInstructions(skillName)
		if err != nil {
			return nil, fmt.Errorf("skill not found: %s", skillName)
		}
	}

	var files []string

	err := filepath.Walk(skill.BasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(skill.BasePath, path)
		if err != nil {
			return err
		}

		files = append(files, relPath)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list skill files: %w", err)
	}

	return files, nil
}

// GetSkillByName returns a cached skill by name
func (l *Loader) GetSkillByName(name string) (*Skill, bool) {
	skill, ok := l.discoveredSkills[name]
	return skill, ok
}

// GetSkillBasePath returns the base path for a skill (always absolute)
func (l *Loader) GetSkillBasePath(skillName string) (string, error) {
	skill, ok := l.discoveredSkills[skillName]
	if !ok {
		var err error
		skill, err = l.LoadSkillInstructions(skillName)
		if err != nil {
			return "", fmt.Errorf("skill not found: %s", skillName)
		}
	}
	// Return absolute path for consistent sandbox execution
	return filepath.Abs(skill.BasePath)
}

// Reload clears the cache and rediscovers all skills
func (l *Loader) Reload() ([]*SkillMetadata, error) {
	l.discoveredSkills = make(map[string]*Skill)
	return l.DiscoverSkills()
}

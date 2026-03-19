package skills

import (
	"context"
	"fmt"
	"sync"

	"github.com/Tencent/WeKnora/internal/sandbox"
)

// Manager manages skills lifecycle including discovery, loading, and script execution
// It coordinates between the Loader (filesystem operations) and Sandbox (script execution)
type Manager struct {
	loader     *Loader
	sandboxMgr sandbox.Manager

	// Configuration
	skillDirs     []string
	allowedSkills []string // Empty means all skills are allowed
	enabled       bool

	// Cache
	metadataCache []*SkillMetadata
	mu            sync.RWMutex
}

// ManagerConfig holds configuration for the skill manager
type ManagerConfig struct {
	SkillDirs     []string // Directories to search for skills
	AllowedSkills []string // Skill names whitelist (empty = allow all)
	Enabled       bool     // Whether skills are enabled
}

// NewManager creates a new skill manager with the given configuration
func NewManager(config *ManagerConfig, sandboxMgr sandbox.Manager) *Manager {
	if config == nil {
		config = &ManagerConfig{
			Enabled: false,
		}
	}

	return &Manager{
		loader:        NewLoader(config.SkillDirs),
		sandboxMgr:    sandboxMgr,
		skillDirs:     config.SkillDirs,
		allowedSkills: config.AllowedSkills,
		enabled:       config.Enabled,
	}
}

// IsEnabled returns whether skills are enabled
func (m *Manager) IsEnabled() bool {
	return m.enabled
}

// Initialize discovers all skills and caches their metadata
// This should be called at startup
func (m *Manager) Initialize(ctx context.Context) error {
	if !m.enabled {
		return nil
	}

	metadata, err := m.loader.DiscoverSkills()
	if err != nil {
		return fmt.Errorf("failed to discover skills: %w", err)
	}

	// Filter by allowed skills if specified
	if len(m.allowedSkills) > 0 {
		metadata = m.filterAllowedSkills(metadata)
	}

	m.mu.Lock()
	m.metadataCache = metadata
	m.mu.Unlock()

	return nil
}

// filterAllowedSkills filters metadata to only include allowed skills
func (m *Manager) filterAllowedSkills(metadata []*SkillMetadata) []*SkillMetadata {
	if len(m.allowedSkills) == 0 {
		return metadata
	}

	allowedSet := make(map[string]bool)
	for _, name := range m.allowedSkills {
		allowedSet[name] = true
	}

	var filtered []*SkillMetadata
	for _, meta := range metadata {
		if allowedSet[meta.Name] {
			filtered = append(filtered, meta)
		}
	}
	return filtered
}

// GetAllMetadata returns metadata for all discovered skills
// This is used for system prompt injection (Level 1)
func (m *Manager) GetAllMetadata() []*SkillMetadata {
	if !m.enabled {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to prevent external modification
	result := make([]*SkillMetadata, len(m.metadataCache))
	copy(result, m.metadataCache)
	return result
}

// LoadSkill loads the full instructions of a skill (Level 2)
func (m *Manager) LoadSkill(ctx context.Context, skillName string) (*Skill, error) {
	if !m.enabled {
		return nil, fmt.Errorf("skills are not enabled")
	}

	// Check if skill is allowed
	if !m.isSkillAllowed(skillName) {
		return nil, fmt.Errorf("skill not allowed: %s", skillName)
	}

	return m.loader.LoadSkillInstructions(skillName)
}

// isSkillAllowed checks if a skill is in the allowed list
func (m *Manager) isSkillAllowed(skillName string) bool {
	if len(m.allowedSkills) == 0 {
		return true
	}
	for _, name := range m.allowedSkills {
		if name == skillName {
			return true
		}
	}
	return false
}

// ReadSkillFile reads an additional file from a skill directory (Level 3)
func (m *Manager) ReadSkillFile(ctx context.Context, skillName, filePath string) (string, error) {
	if !m.enabled {
		return "", fmt.Errorf("skills are not enabled")
	}

	if !m.isSkillAllowed(skillName) {
		return "", fmt.Errorf("skill not allowed: %s", skillName)
	}

	file, err := m.loader.LoadSkillFile(skillName, filePath)
	if err != nil {
		return "", err
	}

	return file.Content, nil
}

// ListSkillFiles lists all files in a skill directory
func (m *Manager) ListSkillFiles(ctx context.Context, skillName string) ([]string, error) {
	if !m.enabled {
		return nil, fmt.Errorf("skills are not enabled")
	}

	if !m.isSkillAllowed(skillName) {
		return nil, fmt.Errorf("skill not allowed: %s", skillName)
	}

	return m.loader.ListSkillFiles(skillName)
}

// ExecuteScript 在沙箱环境中执行指定技能下的脚本文件。
//
// 该函数是运行技能相关可执行代码的安全入口，实施了多层防御策略以确保安全性和隔离性：
//
// 1. 授权与配置检查：
//   - 验证技能系统全局开关是否已启用。
//   - 校验请求的技能名称是否在允许列表（白名单）中。
//   - 确认沙箱管理器已正确配置且可用。
//
// 2. 文件验证与安全解析：
//   - 通过 m.loader.GetSkillBasePath 获取技能的绝对根目录路径。
//   - 使用安全路径解析机制加载目标文件（防止路径穿越攻击）。
//   - 验证文件类型标记，确保只有被识别为可执行脚本的文件（如 .py, .sh）才能运行。
//
// 3. 沙箱隔离执行：
//   - 构建执行配置，使用文件的绝对路径。
//   - 将工作目录 (WorkDir) 强制锁定在技能的基础路径下，限制文件访问范围。
//   - 委托沙箱管理器执行，确保脚本在隔离环境（容器/受限进程）中运行，无法危害主机。
//
// 参数说明：
//   - ctx: 上下文对象，用于控制执行超时和取消操作。
//   - skillName: 技能名称。
//   - scriptPath: 脚本文件相对路径。
//   - args: 命令行参数。
//   - stdin: 标准输入。
//
// 返回值：
//   - *sandbox.ExecuteResult: 脚本执行结果（包含输出、退出码等）。
//   - error: 若任何验证步骤失败或执行出错，返回相应错误。
//		  - 技能系统未启用
//		  - 技能未授权访问
//		  - 沙箱未配置
//		  - 技能目录不存在
//		  - 脚本文件不存在或无效
//		  - 沙箱执行失败
//
// 安全设计：
// - 权限隔离：技能必须在白名单中才允许执行
// - 沙箱隔离：在独立沙箱环境中运行脚本
// - 路径安全：使用绝对路径，工作目录限制在技能路径内

// ExecuteScript executes a script from a skill in the sandbox
func (m *Manager) ExecuteScript(ctx context.Context, skillName, scriptPath string, args []string, stdin string) (*sandbox.ExecuteResult, error) {
	if !m.enabled {
		return nil, fmt.Errorf("skills are not enabled")
	}

	if !m.isSkillAllowed(skillName) {
		return nil, fmt.Errorf("skill not allowed: %s", skillName)
	}

	// Verify sandbox manager is available
	if m.sandboxMgr == nil {
		return nil, fmt.Errorf("sandbox is not configured")
	}

	// Get the skill base path
	basePath, err := m.loader.GetSkillBasePath(skillName)
	if err != nil {
		return nil, err
	}

	// Load the script file to verify it exists and is a script
	file, err := m.loader.LoadSkillFile(skillName, scriptPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load script: %w", err)
	}

	if !file.IsScript {
		return nil, fmt.Errorf("file is not an executable script: %s", scriptPath)
	}

	// Prepare execution config
	config := &sandbox.ExecuteConfig{
		Script:  file.Path,
		Args:    args,
		WorkDir: basePath,
		Stdin:   stdin,
	}

	// Execute in sandbox
	return m.sandboxMgr.Execute(ctx, config)
}

// GetSkillInfo returns detailed information about a skill
func (m *Manager) GetSkillInfo(ctx context.Context, skillName string) (*SkillInfo, error) {
	if !m.enabled {
		return nil, fmt.Errorf("skills are not enabled")
	}

	if !m.isSkillAllowed(skillName) {
		return nil, fmt.Errorf("skill not allowed: %s", skillName)
	}

	skill, err := m.loader.LoadSkillInstructions(skillName)
	if err != nil {
		return nil, err
	}

	files, err := m.loader.ListSkillFiles(skillName)
	if err != nil {
		files = []string{} // Non-fatal error
	}

	return &SkillInfo{
		Name:         skill.Name,
		Description:  skill.Description,
		BasePath:     skill.BasePath,
		Instructions: skill.Instructions,
		Files:        files,
	}, nil
}

// SkillInfo provides detailed information about a skill
type SkillInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	BasePath     string   `json:"base_path"`
	Instructions string   `json:"instructions"`
	Files        []string `json:"files"`
}

// Reload refreshes the skill cache by rediscovering all skills
func (m *Manager) Reload(ctx context.Context) error {
	if !m.enabled {
		return nil
	}

	metadata, err := m.loader.Reload()
	if err != nil {
		return err
	}

	if len(m.allowedSkills) > 0 {
		metadata = m.filterAllowedSkills(metadata)
	}

	m.mu.Lock()
	m.metadataCache = metadata
	m.mu.Unlock()

	return nil
}

// Cleanup releases resources
func (m *Manager) Cleanup(ctx context.Context) error {
	if m.sandboxMgr != nil {
		return m.sandboxMgr.Cleanup(ctx)
	}
	return nil
}

package repository

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// CREATE TABLE sessions (
//    -- ==========================================
//    -- 基础标识与会话归属
//    -- ==========================================
//    id VARCHAR(36) PRIMARY KEY,                 -- 主键：全局唯一会话 ID (UUID)，对应一次完整的对话线程
//    tenant_id INTEGER NOT NULL,                 -- 租户 ID：数据隔离，确保用户只能访问自己租户下的会话
//    title VARCHAR(255),                         -- 会话标题：通常由第一条用户消息生成，或由用户自定义，用于侧边栏展示
//    description TEXT,                           -- 会话描述：可选的摘要或备注，帮助用户快速回忆会话内容
//    knowledge_base_id VARCHAR(36),              -- 关联知识库 ID：此会话绑定的特定知识库。
//                                                -- 若为 NULL，可能表示通用闲聊模式，或使用租户默认知识库。
//                                                -- 检索时仅在此知识库范围内搜索向量。
//
//    -- ==========================================
//    -- 对话流程控制 (状态机与边界)
//    -- ==========================================
//    max_rounds INT NOT NULL DEFAULT 5,          	-- 最大轮次：限制单次会话的最大交互轮数（1 轮=1 问 +1 答）。
//                                                	-- 用途：防止长上下文导致 Token 爆炸，或强制用户在达到限制后开启新话题以保持专注。
//    enable_rewrite BOOLEAN NOT NULL DEFAULT TRUE, -- 启用查询重写：是否开启“多轮对话改写”功能。
//                                                	-- 若为 TRUE，在检索前会将“它多少钱？”结合上文改写为“iPhone 15 多少钱？”，提高检索准确率。
//
//    -- ==========================================
//    -- 兜底与异常处理策略 (用户体验保障)
//    -- ==========================================
//    fallback_strategy VARCHAR(255) NOT NULL DEFAULT 'fixed', 	-- 兜底策略：当检索不到相关知识或模型出错时的处理方式。
//                                                				-- 枚举值示例：
//                                                				-- - 'fixed': 返回预设的固定话术 (见 fallback_response)
//                                                				-- - 'llm_general': 忽略知识库，直接让 LLM 用通用知识回答
//                                                				-- - 'human_handoff': 标记为需要人工介入 (高级功能)
//    fallback_response VARCHAR(255) NOT NULL DEFAULT '很抱歉，我暂时无法回答这个问题。', -- 兜底回复文案：当策略为 'fixed' 时返回的具体文本。
//    keyword_threshold FLOAT NOT NULL DEFAULT 0.5, 			-- 关键词匹配阈值：如果使用混合检索（关键词+向量），此参数控制关键词匹配的最低相似度分数。
//    vector_threshold FLOAT NOT NULL DEFAULT 0.5,  			-- 向量检索阈值：向量相似度的最低分数线。低于此分数的片段将被过滤，避免引入噪声。
//
//    -- ==========================================
//    -- 检索增强配置 (RAG 核心参数)
//    -- ==========================================
//    rerank_model_id VARCHAR(64),                	-- 重排序模型 ID：指定用于对初步检索结果进行精排的模型。
//                                                	-- 若为 NULL，则跳过重排序步骤，直接使用初始检索结果。
//    embedding_top_k INTEGER NOT NULL DEFAULT 10, 	-- 初检召回数量：从向量库中初步检索出的候选片段数量 (Recall)。
//                                                	-- 通常设置较大值 (如 20-50)，以保证召回率。
//    rerank_top_k INTEGER NOT NULL DEFAULT 10,   	-- 精排保留数量：经过 ReRank 模型打分后，最终保留并送入 LLM 上下文的片段数量。
//                                                	-- 通常设置较小值 (如 3-10)，以保证精准度并节省 Token。
//    rerank_threshold FLOAT NOT NULL DEFAULT 0.65, -- 重排序过滤阈值：ReRank 得分低于此值的片段，即使排在前面也会被丢弃，确保进入上下文的内容高度相关。
//
//    -- ==========================================
//    -- 总结与模型路由
//    -- ==========================================
//    summary_model_id VARCHAR(64),               -- 摘要模型 ID：专门用于生成会话标题或长对话总结的轻量级模型。
//                                                -- 分离此配置可避免占用主对话模型的高昂配额。
//    summary_parameters JSON NOT NULL,           -- 摘要模型参数：针对摘要任务的特定参数配置。
//                                                -- 示例：{ "max_tokens": 50, "temperature": 0.7, "prompt_template": "..." }
//
//    -- ==========================================
//    -- 高级扩展配置 (JSON 动态字段)
//    -- ==========================================
//    agent_config JSON DEFAULT NULL,             -- 智能体配置：会话级别的 Agent 设定。
//                                                -- 允许在当前会话中临时覆盖全局 Agent 设定。
//                                                -- 示例：{ "tools": ["calculator", "search"], "persona": "strict_auditor", "memory_type": "long_term" }
//
//    context_config JSON DEFAULT NULL,           -- 上下文管理配置：精细控制 LLM 的上下文窗口行为。
//                                                -- 注意：这不存储具体的消息内容（消息通常在 `messages` 表），而是存储“如何组装上下文”的规则。
//                                                -- 示例：
//                                                -- {
//                                                --   "max_tokens": 4096,                 // 上下文窗口上限
//                                                --   "strategy": "sliding_window",       // 策略：滑动窗口 / 关键帧保留 / 摘要压缩
//                                                --   "include_system_prompt": true,      // 是否包含系统提示词
//                                                --   "compression_ratio": 0.5            // 触发压缩的阈值
//                                                -- }
//) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
//

// sessionRepository implements the SessionRepository interface
type sessionRepository struct {
	db *gorm.DB
}

// NewSessionRepository creates a new session repository instance
func NewSessionRepository(db *gorm.DB) interfaces.SessionRepository {
	return &sessionRepository{db: db}
}

// Create creates a new session
func (r *sessionRepository) Create(ctx context.Context, session *types.Session) (*types.Session, error) {
	session.CreatedAt = time.Now()
	session.UpdatedAt = time.Now()
	if err := r.db.WithContext(ctx).Create(session).Error; err != nil {
		return nil, err
	}
	// Return the session with generated ID
	return session, nil
}

// Get retrieves a session by ID
func (r *sessionRepository) Get(ctx context.Context, tenantID uint64, id string) (*types.Session, error) {
	var session types.Session
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).First(&session, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// GetByTenantID retrieves all sessions for a tenant
func (r *sessionRepository) GetByTenantID(ctx context.Context, tenantID uint64) ([]*types.Session, error) {
	var sessions []*types.Session
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&sessions).Error
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// GetPagedByTenantID retrieves sessions for a tenant with pagination
func (r *sessionRepository) GetPagedByTenantID(
	ctx context.Context, tenantID uint64, page *types.Pagination,
) ([]*types.Session, int64, error) {
	var sessions []*types.Session
	var total int64

	// First query the total count
	err := r.db.WithContext(ctx).Model(&types.Session{}).Where("tenant_id = ?", tenantID).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// Then query the paginated data
	err = r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at DESC").
		Offset(page.Offset()).
		Limit(page.Limit()).
		Find(&sessions).Error
	if err != nil {
		return nil, 0, err
	}

	return sessions, total, nil
}

// Update updates a session
func (r *sessionRepository) Update(ctx context.Context, session *types.Session) error {
	session.UpdatedAt = time.Now()
	return r.db.WithContext(ctx).Where("tenant_id = ?", session.TenantID).Save(session).Error
}

// Delete deletes a session
func (r *sessionRepository) Delete(ctx context.Context, tenantID uint64, id string) error {
	return r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Delete(&types.Session{}, "id = ?", id).Error
}

// BatchDelete deletes multiple sessions by IDs
func (r *sessionRepository) BatchDelete(ctx context.Context, tenantID uint64, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Where("tenant_id = ? AND id IN ?", tenantID, ids).Delete(&types.Session{}).Error
}

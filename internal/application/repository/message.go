package repository

import (
	"context"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// CREATE TABLE messages (
//
//	-- ==========================================
//	-- 基础标识与关联关系
//	-- ==========================================
//	id VARCHAR(36) PRIMARY KEY,                 -- 主键：全局唯一消息 ID (UUID)，标识单条对话记录
//	request_id VARCHAR(36) NOT NULL,            -- 请求批次 ID：关联一次完整的“用户提问 -> 系统回答”交互。
//	                                            -- 关键作用：
//	                                            -- 1. 将用户的 Question (role='user') 和 AI 的 Answer (role='assistant') 绑定为一组。
//	                                            -- 2. 用于幂等性控制：如果重试生成，同一 request_id 下可能有多条 assistant 消息（取最新或标记旧的为废弃）。
//	                                            -- 3. 链路追踪：配合日志系统追踪单次请求的全生命周期耗时。
//	session_id VARCHAR(36) NOT NULL,            -- 会话 ID：外键，关联 `sessions` 表。所有消息归属于特定会话线程。
//	role VARCHAR(50) NOT NULL,                  -- 角色标识：枚举值。
//	                                            -- - 'user': 用户输入
//	                                            -- - 'assistant': AI 回复
//	                                            -- - 'system': 系统提示词 (通常只在上下文组装时动态注入，较少持久化，除非需要审计)
//	                                            -- - 'tool': 工具执行结果 (如搜索 API 返回的原始数据)
//	content TEXT NOT NULL,                      -- 消息内容：核心文本。
//	                                            -- 对于 user：原始提问。
//	                                            -- 对于 assistant：最终生成的回答（可能包含 Markdown）。
//	                                            -- 对于 tool：JSON 格式的工具返回数据。
//
//	-- ==========================================
//	-- RAG 溯源与可解释性 (核心亮点)
//	-- ==========================================
//	knowledge_references JSON NOT NULL,         -- 知识引用源：RAG 系统的“证据链”。
//	                                            -- 存储本次回答参考了哪些知识片段，用于前端展示“引用角标” [1], [2]。
//	                                            -- 结构示例：
//	                                            -- [
//	                                            --   {
//	                                            --     "chunk_id": "chk_123",
//	                                            --     "knowledge_id": "know_456",
//	                                            --     "file_name": "员工手册.pdf",
//	                                            --     "page_number": 12,
//	                                            --     "content_snippet": "请假需提前3天...",
//	                                            --     "score": 0.92,              -- 相似度得分
//	                                            --     "rank": 1                   -- 重排序后的位次
//	                                            --   }
//	                                            -- ]
//	                                            -- 设计价值：解决大模型“幻觉”问题，让用户可点击溯源查看原文。
//
//	-- ==========================================
//	-- 智能体执行过程 (Agent Reasoning)
//	-- ==========================================
//	agent_steps JSON DEFAULT NULL,              -- 智能体执行步骤：记录 Complex Agent 的思考过程和工具调用链 (Chain of Thought)。
//	                                            -- 仅当 role='assistant' 且涉及工具调用时填充。
//	                                            -- 结构示例 (ReAct 模式)：
//	                                            -- [
//	                                            --   {
//	                                            --     "step": 1,
//	                                            --     "thought": "用户需要查天气，我需要调用 weather_tool",
//	                                            --     "action": "weather_tool",
//	                                            --     "action_input": {"city": "Beijing"},
//	                                            --     "observation": "{\"temp\": 25, \"condition\": \"Sunny\"}",
//	                                            --     "duration_ms": 1200
//	                                            --   },
//	                                            --   {
//	                                            --     "step": 2,
//	                                            --     "thought": "获取到天气信息，现在可以回答用户了",
//	                                            --     "final_answer": "北京今天晴朗，气温25度..."
//	                                            --   }
//	                                            -- ]
//	                                            -- 用途：前端可展示“思考中...”的折叠面板，增强用户对 AI 能力的信任感；也可用于调试 Agent 逻辑。
//
//	-- ==========================================
//	-- 状态与生命周期
//	-- ==========================================
//	is_completed BOOLEAN NOT NULL DEFAULT FALSE,-- 完成状态标志：
//	                                            -- FALSE: 消息正在生成中 (流式输出未完成) 或 工具执行中。
//	                                            -- TRUE: 消息已完整生成并落库。
//	                                            -- 用途：
//	                                            -- 1. 前端轮询或 WebSocket 监听此字段判断何时停止 loading 动画。
//	                                            -- 2. 防止在生成过程中重复消费同一条消息。
//	                                            -- 3. 异常恢复：系统重启后，可扫描 `is_completed=FALSE` 且 `updated_at` 久远的记录进行清理或重试。
//
// ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
// messageRepository implements the message repository interface
type messageRepository struct {
	db *gorm.DB
}

// NewMessageRepository creates a new message repository
func NewMessageRepository(db *gorm.DB) interfaces.MessageRepository {
	return &messageRepository{
		db: db,
	}
}

// CreateMessage creates a new message
func (r *messageRepository) CreateMessage(
	ctx context.Context, message *types.Message,
) (*types.Message, error) {
	if err := r.db.WithContext(ctx).Create(message).Error; err != nil {
		return nil, err
	}
	return message, nil
}

// GetMessage retrieves a message
func (r *messageRepository) GetMessage(
	ctx context.Context, sessionID string, messageID string,
) (*types.Message, error) {
	var message types.Message
	if err := r.db.WithContext(ctx).Where(
		"id = ? AND session_id = ?", messageID, sessionID,
	).First(&message).Error; err != nil {
		return nil, err
	}
	return &message, nil
}

// GetMessagesBySession retrieves all messages for a session with pagination
func (r *messageRepository) GetMessagesBySession(
	ctx context.Context, sessionID string, page int, pageSize int,
) ([]*types.Message, error) {
	var messages []*types.Message
	if err := r.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("created_at ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// GetRecentMessagesBySession retrieves recent messages for a session
func (r *messageRepository) GetRecentMessagesBySession(
	ctx context.Context, sessionID string, limit int,
) ([]*types.Message, error) {
	var messages []*types.Message
	if err := r.db.WithContext(ctx).Where(
		"session_id = ?", sessionID,
	).Order("created_at DESC").Limit(limit).Find(&messages).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	slices.SortFunc(messages, func(a, b *types.Message) int {
		cmp := a.CreatedAt.Compare(b.CreatedAt)
		if cmp == 0 {
			if a.Role == "user" { // User messages come first
				return -1
			}
			return 1 // Assistant messages come last
		}
		return cmp
	})
	return messages, nil
}

// GetMessagesBySessionBeforeTime retrieves messages from a session created before a specific time
func (r *messageRepository) GetMessagesBySessionBeforeTime(
	ctx context.Context, sessionID string, beforeTime time.Time, limit int,
) ([]*types.Message, error) {
	var messages []*types.Message
	if err := r.db.WithContext(ctx).Where(
		"session_id = ? AND created_at < ?", sessionID, beforeTime,
	).Order("created_at DESC").Limit(limit).Find(&messages).Error; err != nil {
		return nil, err
	}
	slices.SortFunc(messages, func(a, b *types.Message) int {
		cmp := a.CreatedAt.Compare(b.CreatedAt)
		if cmp == 0 {
			if a.Role == "user" { // User messages come first
				return -1
			}
			return 1 // Assistant messages come last
		}
		return cmp
	})
	return messages, nil
}

// UpdateMessage updates an existing message
func (r *messageRepository) UpdateMessage(ctx context.Context, message *types.Message) error {
	return r.db.WithContext(ctx).Model(&types.Message{}).Where(
		"id = ? AND session_id = ?", message.ID, message.SessionID,
	).Updates(message).Error
}

// DeleteMessage deletes a message
func (r *messageRepository) DeleteMessage(ctx context.Context, sessionID string, messageID string) error {
	return r.db.WithContext(ctx).Where(
		"id = ? AND session_id = ?", messageID, sessionID,
	).Delete(&types.Message{}).Error
}

// GetFirstMessageOfUser retrieves the first message from a user in a session
func (r *messageRepository) GetFirstMessageOfUser(ctx context.Context, sessionID string) (*types.Message, error) {
	var message types.Message
	if err := r.db.WithContext(ctx).Where(
		"session_id = ? and role = ?", sessionID, "user",
	).Order("created_at ASC").First(&message).Error; err != nil {
		return nil, err
	}
	return &message, nil
}

// GetMessageByRequestID retrieves a message by request ID
func (r *messageRepository) GetMessageByRequestID(
	ctx context.Context, sessionID string, requestID string,
) (*types.Message, error) {
	var message types.Message

	result := r.db.WithContext(ctx).
		Where("session_id = ? AND request_id = ?", sessionID, requestID).
		First(&message)

	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, result.Error
	}

	return &message, nil
}

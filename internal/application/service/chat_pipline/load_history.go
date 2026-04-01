// Package chatpipline provides chat pipeline processing capabilities
package chatpipline

import (
	"context"
	"regexp"
	"slices"
	"sort"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 当用户开启多轮对话时，AI 需要知道之前聊了什么。
// 这个插件负责从数据库加载历史聊天记录，并将其注入到当前的上下文中，以便大模型能理解之前的对话背景。

// PluginLoadHistory is a plugin for loading conversation history without query rewriting
// It loads historical dialog context for multi-turn conversations
type PluginLoadHistory struct {
	messageService interfaces.MessageService // Message service for retrieving historical messages
	config         *config.Config            // System configuration
}

// regThink is a regular expression used to match and remove content between <think></think> tags
var regThink = regexp.MustCompile(`(?s)<think>.*?</think>`)

// NewPluginLoadHistory creates a new history loading plugin instance
// Also registers the plugin with the event manager
func NewPluginLoadHistory(eventManager *EventManager,
	messageService interfaces.MessageService,
	config *config.Config,
) *PluginLoadHistory {
	res := &PluginLoadHistory{
		messageService: messageService,
		config:         config,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the list of event types this plugin responds to
// This plugin only responds to LOAD_HISTORY events
func (p *PluginLoadHistory) ActivationEvents() []types.EventType {
	return []types.EventType{types.LOAD_HISTORY}
}

// OnEvent 处理 LOAD_HISTORY 事件，负责从持久化存储中加载并整理历史对话记录。
//
// 该函数执行以下核心处理逻辑：
// 1. 动态配额：根据系统配置或当前请求参数确定需要加载的历史轮数（MaxRounds）。
// 2. 冗余检索：从消息服务中抓取超量（2N+10）的历史消息，以确保在存在残缺对话时仍能凑齐完整的问答对。
// 3. 结构化聚合：利用 RequestID 将散乱的单条消息（user/assistant）聚合成问答对（History 结构）。
// 4. 噪声剔除：使用正则（regThink）识别并移除 Assistant 回复中的推理思考过程（<think> 标签内容），节省 Token 消耗。
// 5. 排序与截断：对聚合后的对话进行时间倒序排列并截取最近的 N 轮，最后反转为时间正序（从旧到新）。
//
// 处理后的历史记录将注入 chatManage.History，为后续插件（如提示词组装或查询改写）提供对话上下文。

// OnEvent processes triggered events
// When receiving a LOAD_HISTORY event, it loads conversation history without rewriting the query
func (p *PluginLoadHistory) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// Determine max rounds from config or request
	maxRounds := p.config.Conversation.MaxRounds
	if chatManage.MaxRounds > 0 {
		maxRounds = chatManage.MaxRounds
	}

	pipelineInfo(ctx, "LoadHistory", "input", map[string]interface{}{
		"session_id": chatManage.SessionID,
		"max_rounds": maxRounds,
	})

	// Get conversation history (fetch more to account for incomplete pairs)
	history, err := p.messageService.GetRecentMessagesBySession(ctx, chatManage.SessionID, maxRounds*2+10)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "history_fetch", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return next()
	}

	pipelineInfo(ctx, "LoadHistory", "fetched", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"message_count": len(history),
	})

	// 1. 利用 Map 结构，将相同 RequestID 的 User 消息与 System 消息归入同一轮 History。
	// 2. 使用正则剔除 Answer 中的推理过程 (<think> 标签)，防止冗余的思考逻辑干扰多轮对话性能。
	// 3. 强制要求 Query 与 Answer 同时存在，过滤掉因报错或中断产生的“孤儿消息”，确保上下文逻辑链条完整。

	// Convert historical messages to conversation history structure
	historyMap := make(map[string]*types.History)

	// Process historical messages, grouped by requestID
	for _, message := range history {
		h, ok := historyMap[message.RequestID]
		if !ok {
			h = &types.History{}
		}
		if message.Role == "user" {
			// User message as query
			h.Query = message.Content
			h.CreateAt = message.CreatedAt
		} else {
			// System message as answer, while removing thinking process
			h.Answer = regThink.ReplaceAllString(message.Content, "")
			h.KnowledgeReferences = message.KnowledgeReferences
		}
		historyMap[message.RequestID] = h
	}

	// Convert to list and filter incomplete conversations
	historyList := make([]*types.History, 0)
	for _, h := range historyMap {
		if h.Answer != "" && h.Query != "" {
			historyList = append(historyList, h)
		}
	}

	// Sort by time, keep the most recent conversations
	sort.Slice(historyList, func(i, j int) bool {
		return historyList[i].CreateAt.After(historyList[j].CreateAt)
	})

	// Limit the number of historical records
	if len(historyList) > maxRounds {
		historyList = historyList[:maxRounds]
	}

	// Reverse to chronological order
	slices.Reverse(historyList)
	chatManage.History = historyList

	pipelineInfo(ctx, "LoadHistory", "output", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"history_rounds": len(historyList),
		"max_rounds":     maxRounds,
	})

	return next()
}

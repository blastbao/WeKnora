// Package chatpipline provides chat pipeline processing capabilities
// Including query rewriting, history processing, model invocation and other features
package chatpipline

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 该插件旨在解决多轮对话中的“指代消解”和“语境缺失”问题。
// 它通过检索历史对话记录，利用大语言模型（LLM）的语义理解能力，
// 将用户当前简短、模糊或指代不明的查询（Query），改写为独立、完整且包含必要上下文的语句。
//
// 在 RAG 系统中，用户往往很懒，喜欢说“它多少钱？”或者“那怎么弄？”，
// 如果直接拿这种指代不明的词去搜知识库，肯定搜不到东西。
// 这个插件利用历史对话记录，把这种“半截话”补全成“完整的问题”。

// PluginRewrite is a plugin for rewriting user queries
// It uses historical dialog context and large language models to optimize the user's original query
type PluginRewrite struct {
	modelService   interfaces.ModelService   // Model service for calling large language models
	messageService interfaces.MessageService // Message service for retrieving historical messages
	config         *config.Config            // System configuration
}

// reg is a regular expression used to match and remove content between <think></think> tags
var reg = regexp.MustCompile(`(?s)<think>.*?</think>`)

// NewPluginRewrite creates a new query rewriting plugin instance
// Also registers the plugin with the event manager
func NewPluginRewrite(eventManager *EventManager,
	modelService interfaces.ModelService, messageService interfaces.MessageService,
	config *config.Config,
) *PluginRewrite {
	res := &PluginRewrite{
		modelService:   modelService,
		messageService: messageService,
		config:         config,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the list of event types this plugin responds to
// This plugin only responds to REWRITE_QUERY events
func (p *PluginRewrite) ActivationEvents() []types.EventType {
	return []types.EventType{types.REWRITE_QUERY}
}

// OnEvent 执行基于上下文的查询改写流程
//
// 利用大语言模型将用户当前可能存在指代不明（如“它多少钱”、“那怎么弄”）的简短查询，
// 改写为语义完整、独立的句子，从而显著提升下游检索系统的召回准确率。
//
// 执行流程详解 (Execution Phases):
//
// 1. 环境预检:
//    - 初始化：初始化时将 RewriteQuery 设为原始 Query，确保后续流程至少有原始输入可用。
//    - 开关检查：若 EnableRewrite 为 false，直接透传至下一插件。
//
// 2. 对话历史处理（上下文构建）:
//    - 获取历史：从 messageService 拉取最近 20 条消息
//    - 清洗与过滤：
//      • 正则脱敏：使用 reg 剔除 <thinking>标签内的思考过程，避免模型输出干扰改写
//      • 对话配对：按 RequestID 将 user 和 assistant 消息组合为 History 对象
//      • 完整性检查：仅保留同时包含提问与回答的完整轮次
//    - 轮次截断：根据 MaxRounds 配置保留最近 N 轮，按时间正序排列
//
// 3. Prompt 模板渲染:
//    - 模板加载：读取系统或用户自定义的 System/User 角色 Prompt 提示词。
//    - 变量注入：将格式化后的 {{conversation}}、{{query}}、{{current_time}}、{{yesterday}} 填入模板。
//    - 时间锚点：注入“当前时间”和“昨天”的具体日期，辅助模型解析相对时间词。
//
// 4. LLM 调用与结果回填:
//    - 模型推理：调用 LLM 改写模型（Temperature=0.3，低随机性）进行改写，确保输出结果的确定性和稳定性。
//    - 结果回填：将模型生成的完整问题赋值给 chatManage.RewriteQuery。
//    - 故障降级：若模型调用失败，记录错误日志并执行 next()，下游将直接使用原始 Query，确保服务高可用。
//
// 参数:
//   - ctx: 上下文环境，用于链路追踪和日志记录。
//   - eventType: 事件类型（此处为 REWRITE_QUERY）。
//   - chatManage: 聊天管理对象，承载原始查询、历史配置及改写后的查询结果。
//   - next: 流水线下一个插件的执行函数。
//
// 返回值:
//   - *PluginError: 若执行过程中出现关键异常则返回，否则返回 next() 继续流水线。

// OnEvent processes triggered events
// When receiving a REWRITE_QUERY event, it rewrites the user query using conversation history and the language model
func (p *PluginRewrite) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// Initialize rewritten query as original query
	chatManage.RewriteQuery = chatManage.Query

	if !chatManage.EnableRewrite {
		pipelineInfo(ctx, "Rewrite", "skip", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"reason":     "rewrite_disabled",
		})
		return next()
	}

	pipelineInfo(ctx, "Rewrite", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"tenant_id":      chatManage.TenantID,
		"user_query":     chatManage.Query,
		"enable_rewrite": chatManage.EnableRewrite,
	})

	// Get conversation history
	history, err := p.messageService.GetRecentMessagesBySession(ctx, chatManage.SessionID, 20)
	if err != nil {
		pipelineWarn(ctx, "Rewrite", "history_fetch", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
	}

	// Convert historical messages to conversation history structure
	historyMap := make(map[string]*types.History)

	// Process historical messages, grouped by requestID
	for _, message := range history {
		history, ok := historyMap[message.RequestID]
		if !ok {
			history = &types.History{}
		}
		if message.Role == "user" {
			// User message as query
			history.Query = message.Content
			history.CreateAt = message.CreatedAt
		} else {
			// System message as answer, while removing thinking process
			history.Answer = reg.ReplaceAllString(message.Content, "")
			history.KnowledgeReferences = message.KnowledgeReferences
		}
		historyMap[message.RequestID] = history
	}

	// Convert to list and filter incomplete conversations
	historyList := make([]*types.History, 0)
	for _, history := range historyMap {
		if history.Answer != "" && history.Query != "" {
			historyList = append(historyList, history)
		}
	}

	// Sort by time, keep the most recent conversations
	sort.Slice(historyList, func(i, j int) bool {
		return historyList[i].CreateAt.After(historyList[j].CreateAt)
	})

	// Limit the number of historical records
	maxRounds := p.config.Conversation.MaxRounds
	if chatManage.MaxRounds > 0 {
		maxRounds = chatManage.MaxRounds
	}
	if len(historyList) > maxRounds {
		historyList = historyList[:maxRounds]
	}

	// Reverse to chronological order
	slices.Reverse(historyList)
	chatManage.History = historyList
	if len(historyList) == 0 {
		pipelineInfo(ctx, "Rewrite", "skip", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"reason":     "empty_history",
		})
		return next()
	}
	pipelineInfo(ctx, "Rewrite", "history_ready", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"history_rounds": len(historyList),
		"max_rounds":     maxRounds,
	})

	userPrompt := p.config.Conversation.RewritePromptUser
	if chatManage.RewritePromptUser != "" {
		userPrompt = chatManage.RewritePromptUser
	}
	systemPrompt := p.config.Conversation.RewritePromptSystem
	if chatManage.RewritePromptSystem != "" {
		systemPrompt = chatManage.RewritePromptSystem
	}

	// Format conversation history for template
	conversationText := formatConversationHistory(historyList)
	currentTime := time.Now().Format("2006-01-02 15:04:05")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	// Replace placeholders in prompts
	userContent := strings.ReplaceAll(userPrompt, "{{conversation}}", conversationText)
	userContent = strings.ReplaceAll(userContent, "{{query}}", chatManage.Query)
	userContent = strings.ReplaceAll(userContent, "{{current_time}}", currentTime)
	userContent = strings.ReplaceAll(userContent, "{{yesterday}}", yesterday)

	systemContent := strings.ReplaceAll(systemPrompt, "{{conversation}}", conversationText)
	systemContent = strings.ReplaceAll(systemContent, "{{query}}", chatManage.Query)
	systemContent = strings.ReplaceAll(systemContent, "{{current_time}}", currentTime)
	systemContent = strings.ReplaceAll(systemContent, "{{yesterday}}", yesterday)

	rewriteModel, err := p.modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		pipelineError(ctx, "Rewrite", "get_model", map[string]interface{}{
			"session_id":    chatManage.SessionID,
			"chat_model_id": chatManage.ChatModelID,
			"error":         err.Error(),
		})
		return next()
	}

	// 调用重写模型
	//
	//	Temperature: 0.3：这是一个低随机性设置。
	//		重写查询需要的是“准确”和“稳定”，而不是“创意”。
	//		较低的温度能确保模型不会胡乱发挥，而是严格基于上下文进行补全。
	//	MaxCompletionTokens: 50：这是一个长度限制。
	//		因为重写后的结果通常只是一个搜索词或短句，限制在 50 个 Token 以内，
	//		既能节省算力成本，又能防止模型输出冗长的废话。
	//	Thinking: &thinking：这里显式关闭了模型的“思考过程”。
	//		重写插件只需要最终的重写结果，不需要看到 AI 的内心戏。

	// Call model to rewrite query
	thinking := false
	response, err := rewriteModel.Chat(ctx, []chat.Message{
		{
			Role:    "system",
			Content: systemContent,
		},
		{
			Role:    "user",
			Content: userContent,
		},
	}, &chat.ChatOptions{
		Temperature:         0.3,
		MaxCompletionTokens: 50,
		Thinking:            &thinking,
	})
	if err != nil {
		pipelineError(ctx, "Rewrite", "model_call", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return next()
	}

	if response.Content != "" {
		// Update rewritten query
		chatManage.RewriteQuery = response.Content
	}
	pipelineInfo(ctx, "Rewrite", "output", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"rewrite_query": chatManage.RewriteQuery,
	})
	return next()
}

// formatConversationHistory formats conversation history for prompt template
func formatConversationHistory(historyList []*types.History) string {
	if len(historyList) == 0 {
		return ""
	}

	var builder strings.Builder
	for _, h := range historyList {
		builder.WriteString("------BEGIN------\n")
		builder.WriteString("用户的问题是：")
		builder.WriteString(h.Query)
		builder.WriteString("\n助手的回答是：")
		builder.WriteString(h.Answer)
		builder.WriteString("\n------END------\n")
	}
	return builder.String()
}

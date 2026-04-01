package chatpipline

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginChatCompletion implements chat completion functionality
// as a plugin that can be registered to EventManager
type PluginChatCompletion struct {
	modelService interfaces.ModelService // Interface for model operations
}

// NewPluginChatCompletion creates a new PluginChatCompletion instance
// and registers it with the EventManager
func NewPluginChatCompletion(eventManager *EventManager, modelService interfaces.ModelService) *PluginChatCompletion {
	res := &PluginChatCompletion{
		modelService: modelService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginChatCompletion) ActivationEvents() []types.EventType {

	return []types.EventType{types.CHAT_COMPLETION}
}

// OnEvent 处理同步对话补全事件，负责将整合后的上下文提交给大模型生成回答。
//
// 该插件是 Pipeline 生成阶段的核心，它通过调用底层模型服务，将 System Prompt、
// 检索到的知识分片、历史对话及当前意图转化为模型最终的文本输出。
//
// 执行过程：
//  1. 输入审计：记录当前会话的基本信息（SessionID、用户提问、历史轮数、模型 ID）至 Pipeline 日志。
//  2. 模型准备 (prepareChatModel)：
//     - 根据配置的 ChatModelID 获取对应的模型实例。
//     - 初始化推理选项（如 Temperature, TopP, MaxTokens 等）。
//  3. 消息封装 (prepareMessagesWithHistory)：
//     - 渲染系统提示词（System Prompt）。
//     - 组装包含系统提示词和历史对话的消息列表。
//  4. 同步请求：
//     - 发起阻塞式的 chatModel.Chat 模型调用。
//     - 若调用超时或网络异常，记录错误日志并封装为 ErrModelCall 返回。
//  5. 响应归档：
//     - 解析模型返回的内容及 Token 消耗（Prompt/Completion Tokens）统计。
//     - 将模型生成的完整响应对象存入 chatManage.ChatResponse，供后续后处理插件使用。
//  6. 流程推进：调用 next() 进入下一个流水线环节（如持久化或消息发送）。

// OnEvent handles chat completion events
// It prepares the chat model, messages, and calls the model to generate responses
func (p *PluginChatCompletion) OnEvent(
	ctx context.Context,
	eventType types.EventType,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "Completion", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"user_question":  chatManage.UserContent,
		"history_rounds": len(chatManage.History),
		"chat_model":     chatManage.ChatModelID,
	})

	// Prepare chat model and options
	chatModel, opt, err := prepareChatModel(ctx, p.modelService, chatManage)
	if err != nil {
		return ErrGetChatModel.WithError(err)
	}

	// Prepare messages including conversation history
	pipelineInfo(ctx, "Completion", "messages_ready", map[string]interface{}{
		"message_count": len(chatManage.History) + 2,
	})
	chatMessages := prepareMessagesWithHistory(chatManage)

	// Call the chat model to generate response
	pipelineInfo(ctx, "Completion", "model_call", map[string]interface{}{
		"chat_model": chatManage.ChatModelID,
	})
	chatResponse, err := chatModel.Chat(ctx, chatMessages, opt)
	if err != nil {
		pipelineError(ctx, "Completion", "model_call", map[string]interface{}{
			"chat_model": chatManage.ChatModelID,
			"error":      err.Error(),
		})
		return ErrModelCall.WithError(err)
	}

	pipelineInfo(ctx, "Completion", "output", map[string]interface{}{
		"answer_preview":    chatResponse.Content,
		"finish_reason":     chatResponse.FinishReason,
		"completion_tokens": chatResponse.Usage.CompletionTokens,
		"prompt_tokens":     chatResponse.Usage.PromptTokens,
	})
	chatManage.ChatResponse = chatResponse
	return next()
}

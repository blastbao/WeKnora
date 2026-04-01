package chatpipline

import (
	"context"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// pipelineInfo logs pipeline info level entries.
func pipelineInfo(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineInfo(ctx, stage, action, fields)
}

// pipelineWarn logs pipeline warning level entries.
func pipelineWarn(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineWarn(ctx, stage, action, fields)
}

// pipelineError logs pipeline error level entries.
func pipelineError(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineError(ctx, stage, action, fields)
}

// prepareChatModel 准备聊天模型及选项
//
// 该函数负责根据会话管理对象(chatManage)获取对应的聊天模型，
// 并基于会话配置(SummaryConfig)构建聊天选项(ChatOptions)，
// 以便后续调用模型生成回复。

// prepareChatModel shared logic to prepare chat model and options
// it gets the chat model and sets up the chat options based on the chat manage.
func prepareChatModel(
	ctx context.Context,
	modelService interfaces.ModelService,
	chatManage *types.ChatManage,
) (
	chat.Chat,
	*chat.ChatOptions,
	error,
) {

	chatModel, err := modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		return nil, nil, err
	}

	opt := &chat.ChatOptions{
		Temperature:         chatManage.SummaryConfig.Temperature,
		TopP:                chatManage.SummaryConfig.TopP,
		Seed:                chatManage.SummaryConfig.Seed,
		MaxTokens:           chatManage.SummaryConfig.MaxTokens,
		MaxCompletionTokens: chatManage.SummaryConfig.MaxCompletionTokens,
		FrequencyPenalty:    chatManage.SummaryConfig.FrequencyPenalty,
		PresencePenalty:     chatManage.SummaryConfig.PresencePenalty,
		Thinking:            chatManage.SummaryConfig.Thinking,
	}

	return chatModel, opt, nil
}

// prepareMessagesWithHistory 组装包含系统指令、历史对话及当前问题的完整消息序列。
//
// 该函数负责将 RAG 流程中散落的上下文信息转化为大模型可识别的对话格式。
// 它通过预定义的角色（System/User/Assistant）构建逻辑链路，确保模型能够基于历史背景和检索到的知识进行回答。
//
// 执行过程：
//  1. 提示词渲染：调用 renderSystemPromptPlaceholders 填充系统提示词中的动态占位符（如背景知识、变量等）。
//  2. 初始构建：将渲染后的系统提示词作为首条 "system" 消息放入序列。
//  3. 历史加载：遍历会话历史，将每一轮对话按顺序拆解为 "user" 提问与 "assistant" 回答并追加到序列中。
//  4. 当前输入追加：将用户本次最新的输入内容作为最新的 "user" 消息置于序列末尾。
//  5. 交付：返回构建完成的消息切片，供后续模型接口（chatModel.Chat）调用。
//
// 参数:
//   - chatManage: 聊天管理对象，包含系统提示词模板、对话历史列表及当前用户输入
//
// 返回值:
//   - []chat.Message: 组装好的消息切片，按 "system" -> "history" -> "current user" 的顺序排列，
//     可直接用于调用聊天模型的 Chat 方法

// prepareMessagesWithHistory prepare complete messages including history
func prepareMessagesWithHistory(chatManage *types.ChatManage) []chat.Message {
	// Replace placeholders in system prompt
	systemPrompt := renderSystemPromptPlaceholders(chatManage.SummaryConfig.Prompt)

	chatMessages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}

	// Add conversation history (already limited by maxRounds in load_history/rewrite plugins)
	for _, history := range chatManage.History {
		chatMessages = append(chatMessages, chat.Message{Role: "user", Content: history.Query})
		chatMessages = append(chatMessages, chat.Message{Role: "assistant", Content: history.Answer})
	}

	// Add current user message
	chatMessages = append(chatMessages, chat.Message{Role: "user", Content: chatManage.UserContent})

	return chatMessages
}

// renderSystemPromptPlaceholders replaces placeholders in system prompt
// Supported placeholders:
//   - {{current_time}} -> current time in RFC3339 format
func renderSystemPromptPlaceholders(prompt string) string {
	result := prompt

	// Replace {{current_time}} placeholder
	if strings.Contains(result, "{{current_time}}") {
		currentTime := time.Now().Format(time.RFC3339)
		result = strings.ReplaceAll(result, "{{current_time}}", currentTime)
	}

	return result
}

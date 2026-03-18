package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

// AgentEngine 是 ReAct (Reasoning and Acting) 代理的核心驱动引擎。
// 它通过迭代循环协调大模型 (LLM) 与工具集 (Tools) 的交互。
//
// 1. 核心架构：ReAct 循环 (executeLoop)
//    引擎在 MaxIterations 限制内运行迭代逻辑，每轮包含以下标准动作：
//    - Think (思考): 通过 streamThinkingToEventBus 调用 LLM。AI 基于当前上下文决定是否调用工具。
//      该步骤支持流式输出 (Streaming)，允许用户实时观察 AI 的推理过程。
//    - Act (行动): 若 AI 触发 Function Call，引擎解析参数并通过 toolRegistry 调度具体工具执行
//      (如 GrepChunks, DataSchemaTool 等)。
//    - Observe (观察): 将工具执行结果 (ToolResult) 作为 Observation 反馈给 AI，构筑下一轮推理的基础。
//
// 2. 状态机与上下文管理 (AgentState & ContextManager)
//    - AgentState: 实时记录执行全过程，包括每一轮的思维链 (Thought) 和工具调用详情 (ToolCalls)。
//    - ContextManager: 负责将 AI 的思考过程、工具调用参数及执行结果持久化至数据库。
//      这确保了对话的连续性，支持在会话中断后能够恢复完整的推理上下文。
//
// 3. 事件驱动机制 (EventBus)
//    采用高度解耦的设计，通过 EventBus 实时向前端推送执行状态：
//    - EventAgentThought: 推送 AI 的实时推理流片段。
//    - EventAgentToolCall: 标记工具调用开始（如“正在检索知识库...”）。
//    - EventAgentToolResult: 传输工具执行后的结构化数据 (Data)，用于前端组件化渲染。
//    这种机制确保了 UI 层的生动反馈，使用户能够感知 AI 复杂的后台决策链路。

// AgentEngine 是执行 ReAct (Reasoning + Acting) 模式的核心智能体引擎。
// 它通过依赖注入整合了以下关键组件以构建完整的智能体运行时环境：
// - config: 控制最大迭代次数、温度及反思策略等行为参数。
// - chatModel: LLM 推理接口，负责生成思考链与工具调用决策。
// - toolRegistry: 工具注册中心，负责解析并执行具体的外部动作。
// - skillsManager: (可选) 实现技能渐进式披露，动态优化 Prompt 中的技能元数据。
// - knowledgeBasesInfo & selectedDocs: 注入知识库元数据与用户选定文档，构建 RAG 上下文。
// - contextManager: 负责多轮对话历史的状态管理与持久化存储。
// - eventBus: 事件总线，实时广播思考过程、工具执行状态及最终结果，实现前后端解耦。
// - sessionID & systemPromptTemplate: 标识会话上下文并支持自定义系统提示词模板。

// AgentEngine is the core engine for running ReAct agents
type AgentEngine struct {
	config               *types.AgentConfig
	toolRegistry         *tools.ToolRegistry
	chatModel            chat.Chat
	eventBus             *event.EventBus
	knowledgeBasesInfo   []*KnowledgeBaseInfo      // Detailed knowledge base information for prompt
	selectedDocs         []*SelectedDocumentInfo   // User-selected documents (via @ mention)
	contextManager       interfaces.ContextManager // Context manager for writing agent conversation to LLM context
	sessionID            string                    // Session ID for context management
	systemPromptTemplate string                    // System prompt template (optional, uses default if empty)
	skillsManager        *skills.Manager           // Skills manager for Progressive Disclosure (optional)
}

// listToolNames returns tool.function names for logging
func listToolNames(ts []chat.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Function.Name)
	}
	return names
}

// NewAgentEngine creates a new agent engine
func NewAgentEngine(
	config *types.AgentConfig,
	chatModel chat.Chat,
	toolRegistry *tools.ToolRegistry,
	eventBus *event.EventBus,
	knowledgeBasesInfo []*KnowledgeBaseInfo,
	selectedDocs []*SelectedDocumentInfo,
	contextManager interfaces.ContextManager,
	sessionID string,
	systemPromptTemplate string,
) *AgentEngine {
	if eventBus == nil {
		eventBus = event.NewEventBus()
	}
	return &AgentEngine{
		config:               config,
		toolRegistry:         toolRegistry,
		chatModel:            chatModel,
		eventBus:             eventBus,
		knowledgeBasesInfo:   knowledgeBasesInfo,
		selectedDocs:         selectedDocs,
		contextManager:       contextManager,
		sessionID:            sessionID,
		systemPromptTemplate: systemPromptTemplate,
	}
}

// NewAgentEngineWithSkills creates a new agent engine with skills support
func NewAgentEngineWithSkills(
	config *types.AgentConfig,
	chatModel chat.Chat,
	toolRegistry *tools.ToolRegistry,
	eventBus *event.EventBus,
	knowledgeBasesInfo []*KnowledgeBaseInfo,
	selectedDocs []*SelectedDocumentInfo,
	contextManager interfaces.ContextManager,
	sessionID string,
	systemPromptTemplate string,
	skillsManager *skills.Manager,
) *AgentEngine {
	engine := NewAgentEngine(
		config,
		chatModel,
		toolRegistry,
		eventBus,
		knowledgeBasesInfo,
		selectedDocs,
		contextManager,
		sessionID,
		systemPromptTemplate,
	)
	engine.skillsManager = skillsManager
	return engine
}

// SetSkillsManager sets the skills manager for the engine
func (e *AgentEngine) SetSkillsManager(manager *skills.Manager) {
	e.skillsManager = manager
}

// GetSkillsManager returns the skills manager
func (e *AgentEngine) GetSkillsManager() *skills.Manager {
	return e.skillsManager
}

// Execute executes the agent with conversation history and streaming output
// All events are emitted to EventBus and handled by subscribers (like Handler layer)
func (e *AgentEngine) Execute(
	ctx context.Context,
	sessionID, messageID, query string,
	llmContext []chat.Message,
) (*types.AgentState, error) {
	logger.Infof(ctx, "========== Agent Execution Started ==========")
	// Ensure tools are cleaned up after execution
	defer e.toolRegistry.Cleanup(ctx)

	logger.Infof(ctx, "[Agent] SessionID: %s, MessageID: %s", sessionID, messageID)
	logger.Infof(ctx, "[Agent] User Query: %s", query)
	logger.Infof(ctx, "[Agent] LLM Context Messages: %d", len(llmContext))
	common.PipelineInfo(ctx, "Agent", "execute_start", map[string]interface{}{
		"session_id":   sessionID,
		"message_id":   messageID,
		"query":        query,
		"context_msgs": len(llmContext),
	})

	// Initialize state
	state := &types.AgentState{
		RoundSteps:    []types.AgentStep{},
		KnowledgeRefs: []*types.SearchResult{},
		IsComplete:    false,
		CurrentRound:  0,
	}

	// Build system prompt using progressive RAG prompt
	// If skills are enabled, include skills metadata (Level 1 - Progressive Disclosure)
	var systemPrompt string
	if e.skillsManager != nil && e.skillsManager.IsEnabled() {
		skillsMetadata := e.skillsManager.GetAllMetadata()
		systemPrompt = BuildSystemPromptWithOptions(
			e.knowledgeBasesInfo,
			e.config.WebSearchEnabled,
			e.selectedDocs,
			&BuildSystemPromptOptions{
				SkillsMetadata: skillsMetadata,
			},
			e.systemPromptTemplate,
		)
	} else {
		systemPrompt = BuildSystemPrompt(
			e.knowledgeBasesInfo,
			e.config.WebSearchEnabled,
			e.selectedDocs,
			e.systemPromptTemplate,
		)
	}
	logger.Debugf(ctx, "[Agent] SystemPrompt Length: %d characters", len(systemPrompt))
	logger.Debugf(ctx, "[Agent] SystemPrompt (stream)\n----\n%s\n----", systemPrompt)

	// Initialize messages with history
	messages := e.buildMessagesWithLLMContext(systemPrompt, query, llmContext)
	logger.Infof(ctx, "[Agent] Total messages for LLM: %d (system: 1, history: %d, user query: 1)",
		len(messages), len(llmContext))

	// Get tool definitions for function calling
	tools := e.buildToolsForLLM()
	toolListStr := strings.Join(listToolNames(tools), ", ")
	logger.Infof(ctx, "[Agent] Tools enabled (%d): %s", len(tools), toolListStr)
	common.PipelineInfo(ctx, "Agent", "tools_ready", map[string]interface{}{
		"session_id": sessionID,
		"tool_count": len(tools),
		"tools":      toolListStr,
	})

	_, err := e.executeLoop(ctx, state, query, messages, tools, sessionID, messageID)
	if err != nil {
		logger.Errorf(ctx, "[Agent] Execution failed: %v", err)
		e.eventBus.Emit(ctx, event.Event{
			ID:        generateEventID("error"),
			Type:      event.EventError,
			SessionID: sessionID,
			Data: event.ErrorData{
				Error:     err.Error(),
				Stage:     "agent_execution",
				SessionID: sessionID,
			},
		})
		return nil, err
	}

	logger.Infof(ctx, "========== Agent Execution Completed Successfully ==========")
	logger.Infof(ctx, "[Agent] Total rounds: %d, Round steps: %d, Is complete: %v",
		state.CurrentRound, len(state.RoundSteps), state.IsComplete)
	common.PipelineInfo(ctx, "Agent", "execute_complete", map[string]interface{}{
		"session_id": sessionID,
		"rounds":     state.CurrentRound,
		"steps":      len(state.RoundSteps),
		"complete":   state.IsComplete,
	})
	return state, nil
}

// 核心流程
//
//	函数 executeLoop 采用了一个 for 循环，直到达到最大迭代次数（MaxIterations）或 LLM 给出最终答复。
//	每一轮（Round）迭代遵循以下四个阶段：
//	 - 思考 (Think)：调用 LLM，获取下一步动作（文本思考或工具调用）。
//	 - 判断 (Check)：分析 LLM 的停止原因。如果没有工具调用且模型指示停止，则结束循环。
//	 - 行动 (Act)：遍历 LLM 要求的工具调用，通过 toolRegistry 执行具体逻辑。
//	 - 观察与反馈 (Observe)：将工具执行结果格式化，反馈给 LLM 进入下一轮。

// executeLoop 执行 ReAct (Reasoning + Acting) 核心循环，驱动智能体完成“思考 - 行动 - 观察”的迭代过程。
// 该函数通过事件总线 (EventBus) 实时流式推送思考内容、工具调用状态及最终结果，实现前后端解耦与高交互体验。
//
// 主要执行流程：
// 1. 循环控制：基于 MaxIterations 防止死循环，每轮记录上下文状态与埋点信息。
// 2. Think (思考)：调用 LLM 生成推理链与工具调用决策，流式输出思考过程；若 LLM 调用失败则立即终止。
// 3. Check (终止判断)：若 LLM 返回 "stop" 且无工具调用，视为任务完成，设置最终答案并跳出循环。
// 4. Act (行动)：串行执行所有工具调用。
//    - 具容错机制：工具执行错误不中断流程，而是封装为 Observation 供 LLM 自我修正 (Self-Correction)。
//    - 实时反馈：发射工具调用开始、结果及结构化数据事件，支持前端富文本渲染。
//    - 可选反思：若启用 Reflection，在每次工具执行后邀请 LLM 评估结果有效性。
// 5. Observe (观察)：将本轮的思考、工具请求及执行结果按标准格式追加至消息历史，并持久化到 ContextManager。
// 6. 兜底策略：若达到最大迭代次数仍未完成，强制调用 LLM 基于已有工具结果合成最终答案，确保用户体验闭环。
// 7. 收尾：发射包含完整执行轨迹 (AgentSteps)、知识引用及耗时统计的完成事件 (EventAgentComplete)。
//
// 参数说明:
// - ctx: 上下文，用于控制超时与链路追踪。
// - state: 智能体运行时状态，记录步骤、答案及完成标志。
// - query: 用户原始查询。
// - messages: 当前对话历史消息列表 (含 System Prompt)。
// - tools: 当前可用的工具定义列表，用于 LLM Function Calling。
// - sessionID/messageID: 会话与消息唯一标识，用于事件关联与上下文持久化。
//
// 返回:
// - *types.AgentState: 执行结束后的完整状态（含最终答案与执行轨迹）。
// - error: 若发生不可恢复错误（如 LLM 服务不可用）则返回错误，工具级错误由内部消化处理。

// executeLoop executes the main ReAct loop
// All events are emitted through EventBus with the given sessionID
func (e *AgentEngine) executeLoop(
	ctx context.Context,
	state *types.AgentState,
	query string,
	messages []chat.Message,
	tools []chat.Tool,
	sessionID string,
	messageID string,
) (*types.AgentState, error) {
	startTime := time.Now()
	common.PipelineInfo(ctx, "Agent", "loop_start", map[string]interface{}{
		"max_iterations": e.config.MaxIterations,
	})
	for state.CurrentRound < e.config.MaxIterations {
		roundStart := time.Now()
		logger.Infof(ctx, "========== Round %d/%d Started ==========", state.CurrentRound+1, e.config.MaxIterations)
		logger.Infof(ctx, "[Agent][Round-%d] Message history size: %d messages", state.CurrentRound+1, len(messages))
		common.PipelineInfo(ctx, "Agent", "round_start", map[string]interface{}{
			"iteration":      state.CurrentRound,
			"round":          state.CurrentRound + 1,
			"message_count":  len(messages),
			"pending_tools":  len(tools),
			"max_iterations": e.config.MaxIterations,
		})

		// 1. Think: Call LLM with function calling and stream thinking through EventBus
		logger.Infof(ctx, "[Agent][Round-%d] Calling LLM with %d tools available...", state.CurrentRound+1, len(tools))
		common.PipelineInfo(ctx, "Agent", "think_start", map[string]interface{}{
			"iteration": state.CurrentRound,
			"round":     state.CurrentRound + 1,
			"tool_cnt":  len(tools),
		})
		response, err := e.streamThinkingToEventBus(ctx, messages, tools, state.CurrentRound, sessionID)
		if err != nil {
			logger.Errorf(ctx, "[Agent][Round-%d] LLM call failed: %v", state.CurrentRound+1, err)
			common.PipelineError(ctx, "Agent", "think_failed", map[string]interface{}{
				"iteration": state.CurrentRound,
				"error":     err.Error(),
			})
			return state, fmt.Errorf("LLM call failed: %w", err)
		}

		common.PipelineInfo(ctx, "Agent", "think_result", map[string]interface{}{
			"iteration":     state.CurrentRound,
			"finish_reason": response.FinishReason,
			"tool_calls":    len(response.ToolCalls),
			"content_len":   len(response.Content),
		})

		// Debug: log finish reason and tool call count from LLM
		logger.Infof(ctx, "[Agent][Round-%d] LLM response received: finish_reason=%s, tool_calls=%d, content_length=%d",
			state.CurrentRound+1, response.FinishReason, len(response.ToolCalls), len(response.Content))
		logger.Debugf(
			ctx,
			"[Agent] LLM response finish=%s, toolCalls=%d",
			response.FinishReason,
			len(response.ToolCalls),
		)
		if response.Content != "" {
			logger.Debugf(ctx, "[Agent][Round-%d] LLM thought content:\n%s", state.CurrentRound+1, response.Content)
		}

		// Create agent step
		step := types.AgentStep{
			Iteration: state.CurrentRound,
			Thought:   response.Content,
			ToolCalls: make([]types.ToolCall, 0),
			Timestamp: time.Now(),
		}

		// 2. Check finish reason - if stop and no tool calls, agent is done
		if response.FinishReason == "stop" && len(response.ToolCalls) == 0 {
			logger.Infof(ctx, "[Agent][Round-%d] Agent finished - no more tool calls needed", state.CurrentRound+1)
			logger.Infof(ctx, "[Agent] Final answer length: %d characters", len(response.Content))
			common.PipelineInfo(ctx, "Agent", "round_final_answer", map[string]interface{}{
				"iteration":  state.CurrentRound,
				"round":      state.CurrentRound + 1,
				"answer_len": len(response.Content),
			})
			state.FinalAnswer = response.Content
			state.IsComplete = true
			state.RoundSteps = append(state.RoundSteps, step)

			// Emit final answer done marker
			e.eventBus.Emit(ctx, event.Event{
				ID:        generateEventID("answer-done"),
				Type:      event.EventAgentFinalAnswer,
				SessionID: sessionID,
				Data: event.AgentFinalAnswerData{
					Content: "",
					Done:    true,
				},
			})
			logger.Infof(
				ctx,
				"[Agent][Round-%d] Duration: %dms",
				state.CurrentRound+1,
				time.Since(roundStart).Milliseconds(),
			)
			break
		}

		// 3. Act: Execute tool calls if any
		if len(response.ToolCalls) > 0 {
			logger.Infof(
				ctx,
				"[Agent][Round-%d] Executing %d tool calls...",
				state.CurrentRound+1,
				len(response.ToolCalls),
			)

			for i, tc := range response.ToolCalls {
				logger.Infof(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool: %s, ID: %s",
					state.CurrentRound+1, i+1, len(response.ToolCalls), tc.Function.Name, tc.ID)

				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					logger.Errorf(ctx, "[Agent][Round-%d][Tool-%d/%d] Failed to parse tool arguments: %v",
						state.CurrentRound+1, i+1, len(response.ToolCalls), err)
					continue
				}

				// Log the arguments in a readable format
				argsJSON, _ := json.MarshalIndent(args, "", "  ")
				logger.Infof(ctx, "[Agent][Round-%d][Tool-%d/%d] Arguments:\n%s",
					state.CurrentRound+1, i+1, len(response.ToolCalls), string(argsJSON))

				toolCallStartTime := time.Now()
				e.eventBus.Emit(ctx, event.Event{
					ID:        tc.ID + "-tool-call",
					Type:      event.EventAgentToolCall,
					SessionID: sessionID,
					Data: event.AgentToolCallData{
						ToolCallID: tc.ID,
						ToolName:   tc.Function.Name,
						Arguments:  args,
						Iteration:  state.CurrentRound,
					},
				})
				logger.Debugf(ctx, "[Agent] ToolCall -> %s args=%s", tc.Function.Name, tc.Function.Arguments)

				// Execute tool
				logger.Infof(ctx, "[Agent][Round-%d][Tool-%d/%d] Executing tool: %s...",
					state.CurrentRound+1, i+1, len(response.ToolCalls), tc.Function.Name)
				common.PipelineInfo(ctx, "Agent", "tool_call_start", map[string]interface{}{
					"iteration":    state.CurrentRound,
					"round":        state.CurrentRound + 1,
					"tool":         tc.Function.Name,
					"tool_call_id": tc.ID,
					"tool_index":   fmt.Sprintf("%d/%d", i+1, len(response.ToolCalls)),
				})
				result, err := e.toolRegistry.ExecuteTool(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
				duration := time.Since(toolCallStartTime).Milliseconds()
				logger.Infof(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool execution completed in %dms",
					state.CurrentRound+1, i+1, len(response.ToolCalls), duration)

				toolCall := types.ToolCall{
					ID:       tc.ID,
					Name:     tc.Function.Name,
					Args:     args,
					Result:   result,
					Duration: duration,
				}

				if err != nil {
					logger.Errorf(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool call failed: %s, error: %v",
						state.CurrentRound+1, i+1, len(response.ToolCalls), tc.Function.Name, err)
					toolCall.Result = &types.ToolResult{
						Success: false,
						Error:   err.Error(),
					}
				}

				toolSuccess := toolCall.Result != nil && toolCall.Result.Success
				pipelineFields := map[string]interface{}{
					"iteration":    state.CurrentRound,
					"round":        state.CurrentRound + 1,
					"tool":         tc.Function.Name,
					"tool_call_id": tc.ID,
					"duration_ms":  duration,
					"success":      toolSuccess,
				}
				if toolCall.Result != nil && toolCall.Result.Error != "" {
					pipelineFields["error"] = toolCall.Result.Error
				}
				if err != nil {
					common.PipelineError(ctx, "Agent", "tool_call_result", pipelineFields)
				} else if toolSuccess {
					common.PipelineInfo(ctx, "Agent", "tool_call_result", pipelineFields)
				} else {
					common.PipelineWarn(ctx, "Agent", "tool_call_result", pipelineFields)
				}

				if toolCall.Result != nil {
					logger.Infof(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool result: success=%v, output_length=%d",
						state.CurrentRound+1, i+1, len(response.ToolCalls),
						toolCall.Result.Success, len(toolCall.Result.Output))
					logger.Debugf(ctx, "[Agent] ToolResult <- %s success=%v len(output)=%d",
						tc.Function.Name, toolCall.Result.Success, len(toolCall.Result.Output))

					// Log the output content for debugging
					if toolCall.Result.Output != "" {
						// Truncate if too long for logging
						outputPreview := toolCall.Result.Output
						if len(outputPreview) > 500 {
							outputPreview = outputPreview[:500] + "... (truncated)"
						}
						logger.Debugf(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool output preview:\n%s",
							state.CurrentRound+1, i+1, len(response.ToolCalls), outputPreview)
					}

					if toolCall.Result.Error != "" {
						logger.Warnf(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool error: %s",
							state.CurrentRound+1, i+1, len(response.ToolCalls), toolCall.Result.Error)
					}

					// Log structured data if present
					if toolCall.Result.Data != nil {
						dataJSON, _ := json.MarshalIndent(toolCall.Result.Data, "", "  ")
						logger.Debugf(ctx, "[Agent][Round-%d][Tool-%d/%d] Tool data:\n%s",
							state.CurrentRound+1, i+1, len(response.ToolCalls), string(dataJSON))
					}
				}

				// Store tool call (Observations are now derived from ToolCall.Result.Output)
				step.ToolCalls = append(step.ToolCalls, toolCall)

				// Emit tool result event (include structured data from tool result)
				e.eventBus.Emit(ctx, event.Event{
					ID:        tc.ID + "-tool-result",
					Type:      event.EventAgentToolResult,
					SessionID: sessionID,
					Data: event.AgentToolResultData{
						ToolCallID: tc.ID,
						ToolName:   tc.Function.Name,
						Output:     result.Output,
						Error:      result.Error,
						Success:    result.Success,
						Duration:   duration,
						Iteration:  state.CurrentRound,
						Data:       result.Data, // Pass structured data for frontend rendering
					},
				})

				// Emit tool execution event (for internal monitoring)
				e.eventBus.Emit(ctx, event.Event{
					ID:        tc.ID + "-tool-exec",
					Type:      event.EventAgentTool,
					SessionID: sessionID,
					Data: event.AgentActionData{
						Iteration:  state.CurrentRound,
						ToolName:   tc.Function.Name,
						ToolInput:  args,
						ToolOutput: result.Output,
						Success:    result.Success,
						Error:      result.Error,
						Duration:   duration,
					},
				})

				// Optional: Reflection after each tool call (streaming)
				if e.config.ReflectionEnabled && result != nil {
					reflection, err := e.streamReflectionToEventBus(
						ctx, tc.ID, tc.Function.Name, result.Output,
						state.CurrentRound, sessionID,
					)
					if err != nil {
						logger.Warnf(ctx, "Reflection failed: %v", err)
					} else if reflection != "" {
						// Store reflection in the corresponding tool call
						// Find the tool call we just added and update it
						if len(step.ToolCalls) > 0 {
							lastIdx := len(step.ToolCalls) - 1
							step.ToolCalls[lastIdx].Reflection = reflection
						}
					}
				}
			}
		}

		state.RoundSteps = append(state.RoundSteps, step)
		// 4. Observe: Add tool results to messages and write to context
		messages = e.appendToolResults(ctx, messages, step)
		common.PipelineInfo(ctx, "Agent", "round_end", map[string]interface{}{
			"iteration":   state.CurrentRound,
			"round":       state.CurrentRound + 1,
			"tool_calls":  len(step.ToolCalls),
			"thought_len": len(step.Thought),
		})
		// 5. Check if we should continue
		state.CurrentRound++
	}

	// If loop finished without final answer, generate one
	if !state.IsComplete {
		logger.Info(ctx, "Reached max iterations, generating final answer")
		common.PipelineWarn(ctx, "Agent", "max_iterations_reached", map[string]interface{}{
			"iterations": state.CurrentRound,
			"max":        e.config.MaxIterations,
		})

		// Stream final answer generation through EventBus
		if err := e.streamFinalAnswerToEventBus(ctx, query, state, sessionID); err != nil {
			logger.Errorf(ctx, "Failed to synthesize final answer: %v", err)
			common.PipelineError(ctx, "Agent", "final_answer_failed", map[string]interface{}{
				"error": err.Error(),
			})
			state.FinalAnswer = "抱歉，我无法生成完整的答案。"
		}
		state.IsComplete = true
	}

	// Emit completion event
	// Convert knowledge refs to interface{} slice for event data
	knowledgeRefsInterface := make([]interface{}, 0, len(state.KnowledgeRefs))
	for _, ref := range state.KnowledgeRefs {
		knowledgeRefsInterface = append(knowledgeRefsInterface, ref)
	}

	e.eventBus.Emit(ctx, event.Event{
		ID:        generateEventID("complete"),
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		Data: event.AgentCompleteData{
			FinalAnswer:     state.FinalAnswer,
			KnowledgeRefs:   knowledgeRefsInterface,
			AgentSteps:      state.RoundSteps, // Include detailed execution steps for message storage
			TotalSteps:      len(state.RoundSteps),
			TotalDurationMs: time.Since(startTime).Milliseconds(),
			MessageID:       messageID, // Include message ID for proper message update
		},
	})

	logger.Infof(ctx, "Agent execution completed in %d rounds", state.CurrentRound)
	return state, nil
}

// buildToolsForLLM builds the tools list for LLM function calling
func (e *AgentEngine) buildToolsForLLM() []chat.Tool {
	functionDefs := e.toolRegistry.GetFunctionDefinitions()
	tools := make([]chat.Tool, 0, len(functionDefs))
	for _, def := range functionDefs {
		tools = append(tools, chat.Tool{
			Type: "function",
			Function: chat.FunctionDef{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  def.Parameters,
			},
		})
	}

	return tools
}

// appendToolResults adds tool results to the message history following OpenAI's tool calling format
// Also writes these messages to the context manager for persistence
func (e *AgentEngine) appendToolResults(
	ctx context.Context,
	messages []chat.Message,
	step types.AgentStep,
) []chat.Message {
	// Add assistant message with tool calls (if any)
	if step.Thought != "" || len(step.ToolCalls) > 0 {
		assistantMsg := chat.Message{
			Role:    "assistant",
			Content: step.Thought,
		}

		// Add tool calls to assistant message (following OpenAI format)
		if len(step.ToolCalls) > 0 {
			assistantMsg.ToolCalls = make([]chat.ToolCall, 0, len(step.ToolCalls))
			for _, tc := range step.ToolCalls {
				// Convert arguments back to JSON string
				argsJSON, _ := json.Marshal(tc.Args)

				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, chat.ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: chat.FunctionCall{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}

		messages = append(messages, assistantMsg)

		// Write assistant message to context
		if e.contextManager != nil {
			if err := e.contextManager.AddMessage(ctx, e.sessionID, assistantMsg); err != nil {
				logger.Warnf(ctx, "[Agent] Failed to add assistant message to context: %v", err)
			} else {
				logger.Debugf(ctx, "[Agent] Added assistant message to context (session: %s)", e.sessionID)
			}
		}
	}

	// Add tool result messages (role: "tool", following OpenAI format)
	for _, toolCall := range step.ToolCalls {
		resultContent := toolCall.Result.Output
		if !toolCall.Result.Success {
			resultContent = fmt.Sprintf("Error: %s", toolCall.Result.Error)
		}

		toolMsg := chat.Message{
			Role:       "tool",
			Content:    resultContent,
			ToolCallID: toolCall.ID,
			Name:       toolCall.Name,
		}

		messages = append(messages, toolMsg)

		// Write tool message to context
		if e.contextManager != nil {
			if err := e.contextManager.AddMessage(ctx, e.sessionID, toolMsg); err != nil {
				logger.Warnf(ctx, "[Agent] Failed to add tool message to context: %v", err)
			} else {
				logger.Debugf(ctx, "[Agent] Added tool message to context (session: %s, tool: %s)", e.sessionID, toolCall.Name)
			}
		}
	}

	return messages
}

// streamLLMToEventBus 驱动 LLM 的流式交互过程，实现“边推理、边分发、边汇总”的全链路处理。
//
// 该函数作为 Agent 引擎的通用底层驱动，负责建立与模型的长连接，通过增量迭代方式持续监听数据流，
// 并在实时回调中完成事件分发，最终归纳为静态结果返回。
//
// 详细执行阶段:
//  1. 建立连接 (Handshake):
//     调用 ChatStream 启动与 LLM 的长连接。
//     若因模型负载过高或网络异常导致启动失败，将立即捕获错误并返回，防止 Agent 进入无效的等待循环。
//
//  2. 增量迭代 (Incremental Iteration):
//     通过 range stream 持续监听模型泵出的数据块 (Chunks)，执行双重累积：
//     - 文本累加 (Thought Accumulation):
//    	 实时拼接 chunk.Content 至 fullContent，解决字符碎片化问题，确保最终拥有完整的推理文本以存入上下文历史。
//     - 工具意图捕获 (Tool Call Capture):
//    	 动态更新 chunk.ToolCalls。
//   	 鉴于流式协议中工具指令（ID、名称、参数）是分段发送的，此步骤确保在流结束时能捕获到最新且完整的工具指令集合。
//
//  3. 实时响应回调 (Real-time Feedback Loop):
//     每接收一个有效 Chunk，立即触发 emitFunc 回调，实现低延迟交互：
//     - 思考流分发: 让前端能像 ChatGPT 一样逐字渲染 Agent 的“心路历程”。
//     - 工具调用预警:
//       允许上层在工具参数完全传完前，提前向 EventBus 发送“准备中”状态信号，提升用户的交互即时感。
//
//  4. 状态归纳 (State Resolution):
//     当 stream 通道关闭，完成从“动态流”到“静态对象”的转换。
//     返回汇总后的 fullContent 和 toolCalls，供后续环节进行精准的 JSON 解析与工具执行。
//
// 参数说明:
//   - ctx: 上下文，用于控制请求取消、超时及链路追踪。
//   - messages: 发送给 LLM 的完整对话历史消息列表。
//   - opts: 聊天配置选项，包含温度 (temperature)、TopP 及可用工具定义等。
//   - emitFunc: 核心回调函数。每收到一个数据块即被调用，接收当前原始块 (chunk)
//     和截止到当前的完整累积内容 (fullContent)，由调用者决定具体的分发逻辑。
//
// 返回值:
//   - string: 整个流过程中累计生成的完整文本字符串 (fullContent)。
//   - []types.LLMToolCall: 模型在本轮迭代中生成的最终工具调用指令列表。
//   - error: 流读取过程中的连接中断、超时或模型解析错误。

// streamLLMToEventBus streams LLM response through EventBus (generic method)
// emitFunc: callback to emit each chunk event
// Returns: full accumulated content, tool calls (if any), error
func (e *AgentEngine) streamLLMToEventBus(
	ctx context.Context,
	messages []chat.Message,
	opts *chat.ChatOptions,
	emitFunc func(chunk *types.StreamResponse, fullContent string),
) (string, []types.LLMToolCall, error) {
	logger.Debugf(ctx, "[Agent][Stream] Starting LLM stream with %d messages", len(messages))

	stream, err := e.chatModel.ChatStream(ctx, messages, opts)
	if err != nil {
		logger.Errorf(ctx, "[Agent][Stream] Failed to start LLM stream: %v", err)
		return "", nil, err
	}

	fullContent := ""
	var toolCalls []types.LLMToolCall
	chunkCount := 0

	for chunk := range stream {
		chunkCount++

		if chunk.Content != "" {
			fullContent += chunk.Content
		}

		// Collect tool calls if present
		if len(chunk.ToolCalls) > 0 {
			toolCalls = chunk.ToolCalls
		}

		// Emit event through callback
		if emitFunc != nil {
			emitFunc(&chunk, fullContent)
		}
	}

	return fullContent, toolCalls, nil
}

// streamReflectionToEventBus 执行 Agent 的“自省”逻辑，评估工具执行结果的有效性，
// streamReflectionToEventBus 是连接工具执行结果与下一轮智能决策之间的桥梁。
//
// 详细处理流程：
// 1. 场景还原：根据工具名和执行结果构建反思 Prompt，引导模型进入“结果审计”模式。
// 2. 异步流转：复用 streamLLMToEventBus 开启流式会话，将自省过程透明化。
// 3. 实时播报：通过 EventBus 广播 EventAgentReflection 事件，允许 UI 实时展示 Agent
//    对当前进度的自我评估（如：确认结果是否满足需求，规划下一阶段行动）。
// 4. 结果持久化：汇总完整的反思文本并返回，供 executeLoop 存入 AgentStep 以备后续回溯。
//
// 参数说明：
//   - toolCallID: 关联的工具调用标识，用于 UI 定位反思发生的上下文。
//   - result: 工具执行后的原始输出内容，作为反思的基础素材。

// 反思过程和主流程，是如何融合的？
//
// 	// --- executeLoop 主循环内部 ---
//	//
//	// 1. Think: LLM 决定调用工具 (streamThinkingToEventBus)
//	// 2. Act: 遍历执行工具 (e.toolRegistry.ExecuteTool)
//	for _, tc := range response.ToolCalls {
//    	result, err := e.toolRegistry.ExecuteTool(...)
//
//    	// 【融合点】：工具刚执行完，拿到 result 之后立即触发
//    	if e.config.ReflectionEnabled && result != nil {
//        	// 调用反思函数
//        	reflection, _ := e.streamReflectionToEventBus(ctx, tc.ID, tc.Name, result.Output, ...)
//        	// 将反思结果挂载到当前步骤中，作为下一轮 Think 的重要参考
//        	step.ToolCalls[lastIdx].Reflection = reflection
//    	}
//	}
//
//	// 3. Observe: 将「工具结果 + 反思内容」一起喂给下一轮 LLM
//	// 4. 下一轮循环开始...
//
// 反思并不是孤立的，它的输出会改变 Agent 的“记忆”：
//	- 输入融合：本函数将「工具名 + 执行结果」组合成一段特定的 Prompt，强制模型进入“审计模式”。
//	- 输出融合：
//		反思产生的 fullReflection 文本会被存入 AgentStep 。
//		在下一轮 executeLoop 开始时，e.appendToolResults 会把这个反思建议连同工具原始数据一起传给 LLM。
//
//		示例：
//			LLM 下一轮看到的不仅仅是“搜索结果为空”，可能还有反思结论 ——
//			“搜索关键词太长了，我应该尝试缩短关键词重试”。
//
//
// 在用户界面上，这种融合表现为「事件链」：
//	- 事件 A (EventAgentToolCall): UI 显示“正在查询天气...”。
//	- 事件 B (EventAgentToolResult): UI 显示“查询成功，气温 25度”。
//	- 事件 C (EventAgentReflection): UI 在下方立即跳出动态文本——“Agent 反思：数据已获取，但缺少风力信息，我决定再补调一个接口。”

// [ 主流程 executeLoop ]
//       │
//       ├─> [ 第一次调用 streamLLMToEventBus ] ────> 目的：决策（Think）
//       │
//       └─> (工具执行完毕后)
//             │
//             └─> [ streamReflectionToEventBus ]
//                       │
//                       └─> [ 第二次调用 streamLLMToEventBus ] ──> 目的：反思（Reflection）
//
// 虽然都是流式调用 LLM，但这两次调用的上下文（Context）完全不同：
//	- 主流程调用：传入的是整个对话历史，目的是让模型决定“下一步调哪个工具”。
//	- 反思流程调用：传入的是一个专门构造的 reflectionPrompt（只包含刚才的工具结果），目的是让模型扮演“质检员”，专门评估刚才那一步做得对不对。
//
// 在当前的 Go 代码实现中，主流程和反思流程并不是“共用”同一个物理长连接，而是“复用”了同一种连接构建模式。
// 由于 LLM 的 API（如 OpenAI 或 Anthropic 风格的接口）通常是无状态的请求-响应模式，每一轮对话或反思在底层其实都是两个独立的 HTTP 流式请求。
// 为了在逻辑上让用户感觉它们属于“同一个流”，是通过 EventBus 传递相同的 SessionID ，实现逻辑上流的绑定。

// streamReflectionToEventBus streams reflection process through EventBus
// Note: Reflection is now handled through the think tool in main loop
func (e *AgentEngine) streamReflectionToEventBus(
	ctx context.Context,
	toolCallID string,
	toolName string,
	result string,
	iteration int,
	sessionID string,
) (string, error) {
	// Simplified reflection without BuildReflectionPrompt
	reflectionPrompt := fmt.Sprintf(`请评估刚才调用工具 %s 的结果，并决定下一步行动。

工具返回: %s

思考:
1. 结果是否满足需求？
2. 下一步应该做什么？`, toolName, result)

	messages := []chat.Message{
		{Role: "user", Content: reflectionPrompt},
	}

	// Generate a single ID for this entire reflection stream
	reflectionID := generateEventID("reflection")

	fullReflection, _, err := e.streamLLMToEventBus(
		ctx,
		messages,                            // 专门为反思构造的提示词
		&chat.ChatOptions{Temperature: 0.5}, // 反思通常需要更稳定的输出
		func(chunk *types.StreamResponse, fullContent string) { // 处理反思过程中的碎块 (Chunks)
			if chunk.Content != "" {
				e.eventBus.Emit(ctx, event.Event{
					ID:        reflectionID,               // Same ID for all chunks in this stream
					Type:      event.EventAgentReflection, // 发送的是“反思类型”的事件
					SessionID: sessionID,
					Data: event.AgentReflectionData{
						ToolCallID: toolCallID,
						Content:    chunk.Content,
						Iteration:  iteration,
						Done:       chunk.Done,
					},
				})
			}
		},
	)
	if err != nil {
		logger.Warnf(ctx, "Reflection failed: %v", err)
		return "", err
	}

	return fullReflection, nil
}

// streamThinkingToEventBus 调用 LLM 执行推理阶段，并将思考过程与工具调用决策实时流式推送至事件总线。
// 该函数是 ReAct 循环中 "Think" 环节的核心实现，负责连接底层 LLM 流式接口与上层事件驱动架构。
//
// 主要功能：
// 1. 配置构建：根据 AgentConfig 动态设置温度 (Temperature)、可用工具列表 (Tools) 及思考模式 (Thinking)。
// 2. 流式监听：委托 `streamLLMToEventBus` 处理底层 SSE/Stream 连接，实时捕获文本块与工具调用信号。
// 3. 事件分发：
//    - 思考内容 (Thought)：将 LLM 生成的文本片段封装为 `EventAgentThought` 事件，使用统一的 ThinkingID 标识同一轮次的连续输出，支持前端打字机效果。
//    - 工具挂起 (ToolCall Pending)：在流式中解析出工具调用意图时，立即发射 `EventAgentToolCall` (Pending 状态)，通知前端即将执行的动作，减少等待感知。
// 4. 防重处理：内部维护 `pendingToolCalls` 映射，确保同一工具调用 ID 在流式过程中仅触发一次“开始”事件。
// 5. 结果聚合：收集完整的思考文本 (`fullContent`) 和结构化工具调用列表 (`toolCalls`)，组装为标准 `ChatResponse` 供后续逻辑使用。
//
// 异常处理：
// - 若底层 LLM 流式调用失败（网络中断、API 错误），直接返回 error，由上层 `executeLoop` 进行统一错误处理与重试决策。
//
// 参数说明:
// - ctx: 上下文，用于控制超时与链路追踪。
// - messages: 当前对话历史（含 System Prompt 与过往交互），作为 LLM 的输入上下文。
// - tools: 当前轮次可用的工具定义 schema，指导 LLM 进行 Function Calling。
// - iteration: 当前 ReAct 迭代轮次索引，用于事件数据标记与日志区分。
// - sessionID: 会话唯一标识，确保事件路由到正确的客户端连接。
//
// 返回:
// - *types.ChatResponse: 包含完整思考内容、解析后的工具调用列表及结束原因的标准响应对象。
// - error: 若 LLM 调用失败则返回错误，否则为 nil。

// 它将 LLM 生成的原始字节流解析为有意义的业务事件（思考片段或工具意图），并立即通过 EventBus 推送给前端。

// 1. 核心机制：回调处理 (Callback Design)
// 	e.streamLLMToEventBus 在接收 LLM 响应的过程中，每解析出一个“数据块” (Chunk)，就会调用传入的回调函数。
//  这个回调函数的作用是：
// 		在 LLM 生成内容的每一毫秒，实时拦截数据流，解析其中的意图（是纯文本思考还是工具调用），
//		并立即通过事件总线（EventBus）推送给前端。
//		目的:
//			将原始的 LLM 数据流转换为业务层面的事件流。
//		输入:
//			- chunk: 当前这一小片数据（可能包含几个字符的文本，或者一个工具调用的片段）。
//			- fullContent: 到目前为止累积的所有文本内容（用于最终返回）。
//		效果:
//			- 异步性：不需要等待 LLM 生成完整个长句子，用户就能看到逐字蹦出的效果。
//			- 解耦：底层负责网络流解析，回调函数负责业务逻辑分发。
//		这种设计实现了真正的“流式交互”，让用户在 LLM 完全生成完回答之前，就能看到思考过程和即将执行的动作。
//
// 2. 逻辑分支一：检测并处理工具调用 (Tool Call Detection)
//		触发条件:
//			当 chunk 的类型被标记为 ResponseTypeToolCall（说明 LLM 决定调用工具了）。
//		数据提取:
//			从 chunk.Data 中提取 tool_call_id（工具调用的唯一ID）和 tool_name（工具名称）。
//		防重机制 (De-duplication):
//			问题:
//				LLM 的流式输出中，同一个工具调用可能会分多个 Chunk 返回，
//				例如第一个 Chunk 说“我要调用A”，第二个 Chunk 又确认了一遍“调用A”。
//				如果不处理，前端会收到多次“开始调用”的事件，导致界面重复渲染加载动画。
//			解决:
//				使用 pendingToolCalls map 记录已经发射过事件的 toolCallID。
//				只有当 !pendingToolCalls[toolCallID] 为真时（即第一次见到这个 ID），
//				才发射事件并标记为 true。
//		事件发射:
//			Type:
//				event.EventAgentToolCall。
//			作用:
//				通知前端“Agent 决定执行 XX 工具”。
//				前端此时可以立即显示该工具的图标、名称和“执行中...”的状态，无需等待工具真正执行完毕。
//				这极大地提升了响应速度的感知。
//
//
// 3. 逻辑分支二：思维链流式展示 (EventAgentThought)
//		触发条件: 当 chunk.Content 不为空（说明 LLM 正在输出自然语言，即“思考过程”或“最终回答”）。
//		关键设计 - 统一 ID (thinkingID):
//			注意: 这里使用的 Event ID 是外层定义的 thinkingID，而不是 chunk 自带的 ID。
//			意义: 这意味着这一轮思考产生的所有文本碎片（Chunk 1, Chunk 2, ... Chunk N）都属于同一个逻辑事件。
//			前端收益: 前端接收到这些事件时，可以根据相同的 ID 将它们拼接起来，实现流畅的打字机效果 (Typewriter Effect)，而不是渲染成多条独立的消息气泡。
//		完成标记 (Done):
//			chunk.Done 指示这是否是最后一个包。
//			前端收到 Done: true 后，可以停止加载动画，光标停止闪烁，表示思考阶段结束。

// streamThinkingToEventBus 驱动 LLM 进行流式推理，实时捕获“思考文本”与“工具调用意图”并推送至事件总线。
// 该函数充当“流量调度器”，根据每个 Chunk 的类型，将其动态路由到不同的事件通道，实现前端界面的毫秒级即时反馈。
//
// 【工作流程示例】
// 假设 LLM 流式返回以下 5 个数据块 (Chunks)：
//  1. Chunk{Content: "我需要先"}
//  2. Chunk{Content: "查询北京的天气。"}
//  3. Chunk{Type: ToolCall, Data: {ID: "call_001", Name: "get_weather"}}
//  4. Chunk{Content: "查询完后我会发送邮件。"}
//  5. Chunk{Type: ToolCall, Data: {ID: "call_002", Name: "send_email"}}
//
// 【动态分流示例】
// 假设 LLM 按时间顺序返回以下 5 个独立的数据块 (Chunks)，代码将对每一块进行即时分流：
//
//  -> 接收 Chunk 1: {"content": "我需要先"}
//     [分流逻辑]: 命中 Content 分支 (Content != "")。
//     [动作]: 发射 EventAgentThought (Content="我需要先", Done=false)。
//     [前端]: 屏幕立即显示文字：“我需要先” (光标闪烁，等待后续)。
//
//  -> 接收 Chunk 2: {"content": "查询北京的天气。"}
//     [分流逻辑]: 再次命中 Content 分支。
//     [动作]: 发射 EventAgentThought (Content="查询北京的天气。", Done=false)。
//     [前端]: 文字无缝拼接，显示为：“我需要先查询北京的天气。”
//
//  -> 接收 Chunk 3 (工具信号): {"type": "tool_call", "data": {"id": "call_001", "name": "get_weather"}}
//     [分流逻辑]: 命中 ToolCall 分支 (跳过 Content 分支)。
//     [动作]: 检查 pendingToolCalls 防重 -> 发射 EventAgentToolCall (Pending)。
//     [前端]: 文字流下方立即弹出状态卡片：“🌤️ 准备调用 get_weather...” (此时工具尚未执行)。
//
//  -> 接收 Chunk 4 (纯文本): {"content": "查询完后我会发送邮件。"}
//     [分流逻辑]: 再次跳回 Content 分支。
//     [动作]: 继续发射 EventAgentThought。
//     [前端]: 文字继续滚动，紧接上一句显示：“...查询完后我会发送邮件。”
//
//  -> 接收 Chunk 5 (工具信号): {"type": "tool_call", "data": {"id": "call_002", "name": "send_email"}}
//     [分流逻辑]: 再次跳入 ToolCall 分支。
//     [动作]: 发射第二个工具的 Pending 事件。
//     [前端]: 界面追加第二个状态卡片：“📧 等待调用 send_email...”。
//
// 核心机制:
//  1. 互斥分流：每个 Chunk 仅被处理一次，文本归文本流，指令归指令流，互不阻塞。
//  2. 上下文连续：尽管处理逻辑在“跳跃”，但所有文本 Chunk 共用同一个 thinkingID，保证前端渲染出的思考内容是连贯的段落。
//  3. 即时感知：工具调用指令一旦在流中出现（哪怕在句子中间），立即触发前端 UI 变化，无需等待整段话结束。
//  4. 结果聚合：流结束后，返回拼接好的完整文本 (fullContent) 和结构化好的工具列表 (toolCalls) 供后续执行。
//
// 参数说明:
//  - ctx: 上下文控制。
//  - messages: 当前对话历史。
//  - tools: 可用工具定义列表。
//  - iteration: 当前 ReAct 循环轮次。
//  - sessionID: 会话 ID。
//
// 返回:
//  - *types.ChatResponse: 包含完整思考内容与解析后的工具调用列表。
//  - error: 流式连接或解析失败时返回错误。

// 工具不会在 streamThinkingToEventBus 函数执行期间（即接收 Chunk 时）真正运行。
// 真正的执行发生在上一级的主循环（executeLoop）中，拿到完整的工具列表后统一执行。
//
// 为什么要这样设计？（为什么不收到一个 Chunk 就执行一个？）
//	完整性校验:
//		LLM 生成的工具调用参数可能是分散在多个 Chunk 里的（例如 {"city": "Bei ... jing"}）。
//		如果在流中间就执行，参数可能还没收全，导致执行报错。
//		所以必须等流结束，拿到完整、合法的 JSON 参数后才能执行。
//	批量执行优化:
//		有时候 LLM 会一次性决定调用多个工具（如例子中的天气 + 邮件）。
//		系统可以收集完所有工具调用后，选择并行执行（Go 语言的 go routine 优势），
//		而不是串行地等一个做完再做下一个，这样速度更快。
//	事务一致性:
//		如果执行到一半发现参数错误，或者上下文变了，在流结束后统一处理更容易进行错误回滚或整体重试。
//
// 真正的执行时机：Act 阶段 ，每一轮（Round）的 Act 阶段都可能会调用工具。
//	在 executeLoop 函数中，执行流程：
//		Wait (等待流结束)：
//			response, err := e.streamThinkingToEventBus(...) 这一行会阻塞，直到 LLM 把最后一个字符传完、连接关闭。
//		Collect (汇总数据)：
//			此时，response.ToolCalls 已经拿到了完整的 JSON 参数（例如：{"city": "Beijing"}）。
//		Execute (真正调用)：
//			for _, tc := range response.ToolCalls {
//			   result, err := e.toolRegistry.ExecuteTool(ctx, tc.Function.Name, ...) // 这里才是真正的执行！
//			}

// streamThinkingToEventBus streams the thinking process through EventBus
func (e *AgentEngine) streamThinkingToEventBus(
	ctx context.Context,
	messages []chat.Message,
	tools []chat.Tool,
	iteration int,
	sessionID string,
) (*types.ChatResponse, error) {
	logger.Infof(ctx, "[Agent][Thinking][Iteration-%d] Starting thinking stream with temperature=%.2f, tools=%d, thinking=%v",
		iteration+1, e.config.Temperature, len(tools), e.config.Thinking)

	opts := &chat.ChatOptions{
		Temperature: e.config.Temperature,
		Tools:       tools,
		Thinking:    e.config.Thinking,
	}
	logger.Debug(context.Background(), "[Agent] streamLLM opts tool_choice=auto temperature=", e.config.Temperature)

	pendingToolCalls := make(map[string]bool)

	// Generate a single ID for this entire thinking stream
	thinkingID := generateEventID("thinking")
	logger.Debugf(ctx, "[Agent][Thinking][Iteration-%d] ThinkingID: %s", iteration+1, thinkingID)

	fullContent, toolCalls, err := e.streamLLMToEventBus(
		ctx,
		messages,
		opts,
		func(chunk *types.StreamResponse, fullContent string) {
			if chunk.ResponseType == types.ResponseTypeToolCall && chunk.Data != nil {
				toolCallID, _ := chunk.Data["tool_call_id"].(string)
				toolName, _ := chunk.Data["tool_name"].(string)

				if toolCallID != "" && toolName != "" && !pendingToolCalls[toolCallID] {
					pendingToolCalls[toolCallID] = true
					e.eventBus.Emit(ctx, event.Event{
						ID:        fmt.Sprintf("%s-tool-call-pending", toolCallID),
						Type:      event.EventAgentToolCall,
						SessionID: sessionID,
						Data: event.AgentToolCallData{
							ToolCallID: toolCallID,
							ToolName:   toolName,
							Iteration:  iteration,
						},
					})
				}
			}

			if chunk.Content != "" {
				// logger.Debugf(ctx, "[Agent][Thinking][Iteration-%d] Emitting thought chunk: %d chars",
				// 	iteration+1, len(chunk.Content))
				e.eventBus.Emit(ctx, event.Event{
					ID:        thinkingID, // Same ID for all chunks in this stream
					Type:      event.EventAgentThought,
					SessionID: sessionID,
					Data: event.AgentThoughtData{
						Content:   chunk.Content,
						Iteration: iteration,
						Done:      chunk.Done,
					},
				})
			}
		},
	)
	if err != nil {
		logger.Errorf(ctx, "[Agent][Thinking][Iteration-%d] Thinking stream failed: %v", iteration+1, err)
		return nil, err
	}

	logger.Infof(ctx, "[Agent][Thinking][Iteration-%d] Thinking completed: content=%d chars, tool_calls=%d",
		iteration+1, len(fullContent), len(toolCalls))

	// Build response
	return &types.ChatResponse{
		Content:      fullContent,
		ToolCalls:    toolCalls,
		FinishReason: "stop",
	}, nil
}

// streamFinalAnswerToEventBus streams the final answer generation through EventBus
func (e *AgentEngine) streamFinalAnswerToEventBus(
	ctx context.Context,
	query string,
	state *types.AgentState,
	sessionID string,
) error {
	logger.Infof(ctx, "[Agent][FinalAnswer] Starting final answer generation")
	totalToolCalls := countTotalToolCalls(state.RoundSteps)
	logger.Infof(ctx, "[Agent][FinalAnswer] Context: %d steps with total %d tool calls",
		len(state.RoundSteps), totalToolCalls)
	common.PipelineInfo(ctx, "Agent", "final_answer_start", map[string]interface{}{
		"session_id":   sessionID,
		"query":        query,
		"steps":        len(state.RoundSteps),
		"tool_results": totalToolCalls,
	})

	// Build messages with all context
	systemPrompt := BuildSystemPrompt(
		e.knowledgeBasesInfo,
		e.config.WebSearchEnabled,
		e.selectedDocs,
		e.systemPromptTemplate,
	)

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: query},
	}

	// Add all tool call results as context
	toolResultCount := 0
	for stepIdx, step := range state.RoundSteps {
		for toolIdx, toolCall := range step.ToolCalls {
			toolResultCount++
			messages = append(messages, chat.Message{
				Role:    "user",
				Content: fmt.Sprintf("工具 %s 返回: %s", toolCall.Name, toolCall.Result.Output),
			})
			logger.Debugf(ctx, "[Agent][FinalAnswer] Added tool result [Step-%d][Tool-%d]: %s (output: %d chars)",
				stepIdx+1, toolIdx+1, toolCall.Name, len(toolCall.Result.Output))
		}
	}

	logger.Infof(ctx, "[Agent][FinalAnswer] Total context messages: %d (including %d tool results)",
		len(messages), toolResultCount)

	// Add final answer prompt
	finalPrompt := fmt.Sprintf(`基于上述工具调用结果，请为用户问题生成完整答案。

用户问题: %s

要求:
1. 基于实际检索到的内容回答
2. 清晰标注信息来源 (chunk_id, 文档名)
3. 结构化组织答案
4. 如信息不足，诚实说明

现在请生成最终答案:`, query)

	messages = append(messages, chat.Message{
		Role:    "user",
		Content: finalPrompt,
	})

	// Generate a single ID for this entire final answer stream
	answerID := generateEventID("answer")
	logger.Debugf(ctx, "[Agent][FinalAnswer] AnswerID: %s", answerID)

	fullAnswer, _, err := e.streamLLMToEventBus(
		ctx,
		messages,
		&chat.ChatOptions{Temperature: e.config.Temperature, Thinking: e.config.Thinking},
		func(chunk *types.StreamResponse, fullContent string) {
			if chunk.Content != "" {
				logger.Debugf(ctx, "[Agent][FinalAnswer] Emitting answer chunk: %d chars", len(chunk.Content))
				e.eventBus.Emit(ctx, event.Event{
					ID:        answerID, // Same ID for all chunks in this stream
					Type:      event.EventAgentFinalAnswer,
					SessionID: sessionID,
					Data: event.AgentFinalAnswerData{
						Content: chunk.Content,
						Done:    chunk.Done,
					},
				})
			}
		},
	)
	if err != nil {
		logger.Errorf(ctx, "[Agent][FinalAnswer] Final answer generation failed: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_stream_failed", map[string]interface{}{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		return err
	}

	logger.Infof(ctx, "[Agent][FinalAnswer] Final answer generated: %d characters", len(fullAnswer))
	common.PipelineInfo(ctx, "Agent", "final_answer_done", map[string]interface{}{
		"session_id": sessionID,
		"answer_len": len(fullAnswer),
	})
	state.FinalAnswer = fullAnswer
	return nil
}

// countTotalToolCalls counts total tool calls across all steps
func countTotalToolCalls(steps []types.AgentStep) int {
	total := 0
	for _, step := range steps {
		total += len(step.ToolCalls)
	}
	return total
}

// buildMessagesWithLLMContext builds the message array with LLM context
func (e *AgentEngine) buildMessagesWithLLMContext(
	systemPrompt, currentQuery string,
	llmContext []chat.Message,
) []chat.Message {
	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}

	if len(llmContext) > 0 {
		for _, msg := range llmContext {
			if msg.Role == "system" {
				continue
			}
			if msg.Role == "user" || msg.Role == "assistant" || msg.Role == "tool" {
				messages = append(messages, msg)
			}
		}
		logger.Infof(context.Background(), "Added %d history messages to context", len(llmContext))
	}

	messages = append(messages, chat.Message{
		Role:    "user",
		Content: currentQuery,
	})

	return messages
}

// generateEventID generates a unique event ID with type suffix for better traceability
func generateEventID(suffix string) string {
	return fmt.Sprintf("%s-%s", uuid.New().String()[:8], suffix)
}

package chatpipline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
)

// PluginStreamFilter 实现聊天管道中的流式过滤功能
//
// 该插件用于拦截和处理模型生成的流式响应，支持基于前缀的过滤策略。
// 当检测到响应以特定前缀开头时，可以阻止该响应继续传递，并触发降级回复。

// PluginStreamFilter implements stream filtering functionality for chat pipeline
type PluginStreamFilter struct{}

// NewPluginStreamFilter creates a new stream filter plugin instance
func NewPluginStreamFilter(eventManager *EventManager) *PluginStreamFilter {
	res := &PluginStreamFilter{}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginStreamFilter) ActivationEvents() []types.EventType {
	return []types.EventType{types.STREAM_FILTER}
}

// OnEvent 用于拦截和校验模型生成的流式响应内容。
// 该函数是插件的入口点，负责检查是否需要启用前缀过滤，并协调过滤逻辑。
//
// 处理逻辑:
//  1. 检查EventBus是否存在，不存在则返回错误
//  2. 检查SummaryConfig.NoMatchPrefix是否配置，决定是否启用过滤
//  3. 若启用过滤，调用filterEventsWithPrefix进行拦截处理
//  4. 若未启用过滤，直接调用next()透传
//
// 配置说明:
//   - SummaryConfig.NoMatchPrefix: 不匹配前缀，当响应以此前缀开头时需要过滤
//   - EventBus: 事件总线，用于接收和发送流式事件

// OnEvent handles stream filtering events in the chat pipeline
func (p *PluginStreamFilter) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "StreamFilter", "input", map[string]interface{}{
		"session_id":      chatManage.SessionID,
		"has_event_bus":   chatManage.EventBus != nil,
		"no_match_prefix": chatManage.SummaryConfig.NoMatchPrefix,
	})

	// EventBus is required
	if chatManage.EventBus == nil {
		pipelineError(ctx, "StreamFilter", "eventbus_missing", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return ErrModelCall.WithError(errors.New("EventBus is required for stream filtering"))
	}
	eventBus := chatManage.EventBus

	// Check if no-match prefix filtering is needed
	matchNoMatchBuilderPrefix := chatManage.SummaryConfig.NoMatchPrefix != ""

	if matchNoMatchBuilderPrefix {
		pipelineInfo(ctx, "StreamFilter", "enable_prefix_filter", map[string]interface{}{
			"prefix": chatManage.SummaryConfig.NoMatchPrefix,
		})
		// Create an event interceptor for prefix filtering
		return p.filterEventsWithPrefix(ctx, chatManage, eventBus, next)
	}

	// No filtering needed, just pass through
	pipelineInfo(ctx, "StreamFilter", "passthrough", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})
	return next()
}

// filterEventsWithPrefix intercepts events, checks for NoMatchPrefix, and re-emits filtered events
func (p *PluginStreamFilter) filterEventsWithPrefix(
	ctx context.Context,
	chatManage *types.ChatManage,
	originalEventBus types.EventBusInterface,
	next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "StreamFilter", "setup_temp_bus", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})

	// Create a temporary EventBus to intercept events
	tempEventBus := event.NewEventBus()
	chatManage.EventBus = tempEventBus.AsEventBusInterface()

	responseBuilder := &strings.Builder{}
	matchFound := false

	// Subscribe to answer events from temp bus
	tempEventBus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}

		responseBuilder.WriteString(data.Content)

		// Check if content does NOT match the no-match prefix (meaning it's valid content)
		if !strings.HasPrefix(chatManage.SummaryConfig.NoMatchPrefix, responseBuilder.String()) {
			pipelineInfo(ctx, "StreamFilter", "emit_valid_chunk", map[string]interface{}{
				"chunk_len": len(responseBuilder.String()),
			})

			// Emit the accumulated content as valid answer
			originalEventBus.Emit(ctx, types.Event{
				ID:        evt.ID,
				Type:      types.EventType(event.EventAgentFinalAnswer),
				SessionID: chatManage.SessionID,
				Data: event.AgentFinalAnswerData{
					Content: responseBuilder.String(),
					Done:    data.Done,
				},
			})
			matchFound = true
		}

		return nil
	})

	// Call next to trigger pipeline stages that will emit to tempEventBus
	err := next()

	// After pipeline completes, check if we need fallback
	if !matchFound && responseBuilder.Len() > 0 {
		pipelineInfo(ctx, "StreamFilter", "emit_fallback", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		fallbackID := fmt.Sprintf("%s-fallback", uuid.New().String()[:8])
		originalEventBus.Emit(ctx, types.Event{
			ID:        fallbackID,
			Type:      types.EventType(event.EventAgentFinalAnswer),
			SessionID: chatManage.SessionID,
			Data: event.AgentFinalAnswerData{
				Content: chatManage.FallbackResponse,
				Done:    true,
			},
		})
	}

	// Restore original EventBus
	chatManage.EventBus = originalEventBus

	return err
}

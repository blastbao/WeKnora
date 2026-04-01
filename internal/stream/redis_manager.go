package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
)

// RedisStreamManager implements StreamManager using Redis Lists for append-only event streaming
type RedisStreamManager struct {
	client *redis.Client
	ttl    time.Duration // TTL for stream data in Redis
	prefix string        // Redis key prefix
}

// NewRedisStreamManager creates a new Redis-based stream manager
func NewRedisStreamManager(redisAddr, redisUsername, redisPassword string,
	redisDB int, prefix string, ttl time.Duration,
) (*RedisStreamManager, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Username: redisUsername,
		Password: redisPassword,
		DB:       redisDB,
	})

	// Verify connection
	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	if ttl == 0 {
		ttl = 24 * time.Hour // Default TTL: 24 hours
	}

	if prefix == "" {
		prefix = "stream:events" // Default prefix
	}

	return &RedisStreamManager{
		client: client,
		ttl:    ttl,
		prefix: prefix,
	}, nil
}

// buildKey builds the Redis key for event list
func (r *RedisStreamManager) buildKey(sessionID, messageID string) string {
	return fmt.Sprintf("%s:%s:%s", r.prefix, sessionID, messageID)
}

// AppendEvent 将单个流式事件追加到 Redis 列表中
// 使用 Redis RPush 命令实现 O(1) 时间复杂度的追加操作
//
// 该函数确保事件按写入顺序存储，并为每个 key 设置/刷新 TTL，
// 实现数据的自动过期清理
//
// 参数:
//   - ctx: 上下文，用于超时控制和取消操作
//   - sessionID: 会话 ID，用于构建 Redis key
//   - messageID: 消息 ID，用于构建 Redis key
//   - event: 需要存储的流式事件（如果事件中 Timestamp 为零值，会自动填充当前时间）
//
// 返回:
//   - error: 错误信息，如果序列化或 Redis 操作失败则返回

// AppendEvent appends a single event to the stream using Redis RPush
func (r *RedisStreamManager) AppendEvent(
	ctx context.Context,
	sessionID, messageID string,
	event interfaces.StreamEvent,
) error {
	key := r.buildKey(sessionID, messageID)

	// Set timestamp if not already set
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	// Serialize event to JSON
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	// Append to Redis list with RPush (O(1) operation)
	if err := r.client.RPush(ctx, key, eventJSON).Err(); err != nil {
		return fmt.Errorf("failed to append event to Redis: %w", err)
	}

	// Set/refresh TTL on the key
	if err := r.client.Expire(ctx, key, r.ttl).Err(); err != nil {
		return fmt.Errorf("failed to set TTL: %w", err)
	}

	return nil
}

// GetEvents 从指定的事件流中按偏移量增量获取事件。
//
// 该方法基于 Redis List 实现，使用 LRange 从 fromOffset 开始读取到末尾，
// 适用于客户端通过 offset 轮询或流式拉取事件的场景（如 SSE / WebSocket）。
// 这种机制支持客户端进行断点续传或轮询，避免重复拉取已处理的历史数据。
//
// 参数：
//   - ctx: 上下文，用于控制超时与取消
//   - sessionID: 会话 ID，用于隔离不同会话的数据
//   - messageID: 消息 ID，用于隔离同一会话内的不同消息流
//   - fromOffset: 起始偏移量（0-based），表示从第几个事件开始读取
//
// 返回值：
//   - []interfaces.StreamEvent: 本次读取到的事件列表
//   - int: 下一次读取时应使用的偏移量（nextOffset）
//   - error: 错误信息（仅发生在 Redis 异常时）
//
// 说明：
//   - 若 key 不存在或没有新事件，返回空切片和原 offset
//   - 返回的 nextOffset 可直接用于下一次 GetEvents 调用
//   - 单个事件反序列化失败时会被跳过，不影响其他事件

// GetEvents gets events starting from offset using Redis LRange
// Returns: events slice, next offset, error
func (r *RedisStreamManager) GetEvents(
	ctx context.Context,
	sessionID, messageID string,
	fromOffset int,
) ([]interfaces.StreamEvent, int, error) {

	// 构建 Redis key: {prefix}:{sessionID}:{messageID}
	key := r.buildKey(sessionID, messageID)

	// 使用 LRange 从指定偏移量获取到列表末尾的所有事件。
	// LRange 索引是包含性的（inclusive），-1 表示最后一个元素。
	results, err := r.client.LRange(ctx, key, int64(fromOffset), -1).Result()
	if err != nil {
		if err == redis.Nil {
			// Key doesn't exist - return empty slice
			return []interfaces.StreamEvent{}, fromOffset, nil
		}
		return nil, fromOffset, fmt.Errorf("failed to get events from Redis: %w", err)
	}

	// No new events
	if len(results) == 0 {
		return []interfaces.StreamEvent{}, fromOffset, nil
	}

	// Unmarshal events
	events := make([]interfaces.StreamEvent, 0, len(results))
	for _, result := range results {
		var event interfaces.StreamEvent
		if err := json.Unmarshal([]byte(result), &event); err != nil {
			// Log error but continue with other events
			continue
		}
		events = append(events, event)
	}

	// 计算下次应该使用的偏移量 = 当前偏移量 + 本次获取到的数据条数
	nextOffset := fromOffset + len(results)
	return events, nextOffset, nil
}

// Close closes the Redis connection
func (r *RedisStreamManager) Close() error {
	return r.client.Close()
}

// Ensure RedisStreamManager implements StreamManager interface
var _ interfaces.StreamManager = (*RedisStreamManager)(nil)

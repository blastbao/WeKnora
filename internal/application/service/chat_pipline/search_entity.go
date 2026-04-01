package chatpipline

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 核心任务：
//
//	根据用户问题中提取的“实体”（如人名、地名、专有名词），在知识图谱中进行查找，
// 	找到相关的节点和关系，并将这些关联到的原始文本片段补充到搜索结果中。
//
//	这通常用于 RAG（检索增强生成）系统的高级检索阶段，目的是利用知识图谱的关联能力，
//	找到传统向量检索可能遗漏的相关信息。

// PluginSearch implements search functionality for chat pipeline
type PluginSearchEntity struct {
	graphRepo     interfaces.RetrieveGraphRepository
	chunkRepo     interfaces.ChunkRepository
	knowledgeRepo interfaces.KnowledgeRepository
}

// NewPluginSearchEntity creates a new plugin search entity
func NewPluginSearchEntity(
	eventManager *EventManager,
	graphRepository interfaces.RetrieveGraphRepository,
	chunkRepository interfaces.ChunkRepository,
	knowledgeRepository interfaces.KnowledgeRepository,
) *PluginSearchEntity {
	res := &PluginSearchEntity{
		graphRepo:     graphRepository,
		chunkRepo:     chunkRepository,
		knowledgeRepo: knowledgeRepository,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the list of event types this plugin responds to
func (p *PluginSearchEntity) ActivationEvents() []types.EventType {
	return []types.EventType{types.ENTITY_SEARCH}
}

// OnEvent
//
// 核心流程概览
//	前置检查：确认是否有实体需要检索，以及是否有可用的知识库。
//	并发图谱检索：在多个知识库或特定文件中并行搜索该实体对应的图谱数据（节点和关系）。
//	结果去重与过滤：将图谱中找到的节点对应的文本块（Chunk）提取出来，并过滤掉已经在之前步骤中搜索过的旧数据。
//	数据补全与合并：查询这些文本块的详细信息（如所属文档标题），将其转换为标准的搜索结果格式，并合并到最终的搜索列表中。
//
// 执行过程：
//  1. 预校验：检查请求中是否存在实体（Entity）以及是否有启用了提取配置的知识库 ID。
//  2. 并发策略选择：
//     - 路径 A (特定文件搜索)：若 entityKnowledge 存在，按“文件级别”并发检索图谱。
//     - 路径 B (知识库搜索)：若上述不存在，则按“整个知识库级别”并发检索。
//  3. 图谱检索 (SearchNode)：
//     - 使用协程并行调用 graphRepo，在各自的 Namespace 下查找与实体匹配的节点（Node）和关系（Relation）。
//     - 通过互斥锁（Mutex）安全汇总所有并发结果到 GraphResult 中。
//  4. 原始分片溯源 (Chunk Backtracking)：
//     - 调用 filterSeenChunk 提取图中实体关联的、且当前尚未出现在搜索结果中的 Chunk ID。
//     - 批量从数据库加载这些 Chunks 及其对应的知识库元数据（Knowledge Metadata）。
//  5. 结果整合与去重：
//     - 将新发现的 Chunk 转换为 SearchResult 格式并追加到总结果集中。
//     - 调用 removeDuplicateResults 进行全局去重，确保最终结果精炼。
//  6. 流程推进：若无任何结果返回 ErrSearchNothing，否则调用 next() 进入下一插件环节。

// OnEvent processes triggered events
func (p *PluginSearchEntity) OnEvent(
	ctx context.Context,
	eventType types.EventType,
	chatManage *types.ChatManage,
	next func() *PluginError,
) *PluginError {
	entity := chatManage.Entity
	if len(entity) == 0 {
		logger.Infof(ctx, "No entity found")
		return next()
	}

	// Use EntityKBIDs (knowledge bases with ExtractConfig enabled)
	knowledgeBaseIDs := chatManage.EntityKBIDs
	// Use EntityKnowledge (KnowledgeID -> KnowledgeBaseID mapping for graph-enabled files)
	entityKnowledge := chatManage.EntityKnowledge

	if len(knowledgeBaseIDs) == 0 && len(entityKnowledge) == 0 {
		logger.Warnf(ctx, "No knowledge base IDs or knowledge IDs with ExtractConfig enabled for entity search")
		return next()
	}

	// Parallel search across multiple knowledge bases and individual files
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allNodes []*types.GraphNode
	var allRelations []*types.GraphRelation

	// If specific KnowledgeIDs are provided, search by individual files
	if len(entityKnowledge) > 0 {
		logger.Infof(ctx, "Searching entities across %d knowledge file(s)", len(entityKnowledge))
		for knowledgeID, kbID := range entityKnowledge {
			wg.Add(1)
			go func(knowledgeBaseID, knowledgeID string) {
				defer wg.Done()

				graph, err := p.graphRepo.SearchNode(ctx, types.NameSpace{
					KnowledgeBase: knowledgeBaseID,
					Knowledge:     knowledgeID,
				}, entity)
				if err != nil {
					logger.Errorf(ctx, "Failed to search entity in Knowledge %s: %v", knowledgeID, err)
					return
				}

				logger.Infof(
					ctx,
					"Knowledge %s entity search result count: %d nodes, %d relations",
					knowledgeID,
					len(graph.Node),
					len(graph.Relation),
				)

				mu.Lock()
				allNodes = append(allNodes, graph.Node...)
				allRelations = append(allRelations, graph.Relation...)
				mu.Unlock()
			}(kbID, knowledgeID)
		}
	} else {
		// Otherwise, search by knowledge base
		logger.Infof(ctx, "Searching entities across %d knowledge base(s): %v", len(knowledgeBaseIDs), knowledgeBaseIDs)
		for _, kbID := range knowledgeBaseIDs {
			wg.Add(1)
			go func(knowledgeBaseID string) {
				defer wg.Done()

				graph, err := p.graphRepo.SearchNode(ctx, types.NameSpace{KnowledgeBase: knowledgeBaseID}, entity)
				if err != nil {
					logger.Errorf(ctx, "Failed to search entity in KB %s: %v", knowledgeBaseID, err)
					return
				}

				logger.Infof(
					ctx,
					"KB %s entity search result count: %d nodes, %d relations",
					knowledgeBaseID,
					len(graph.Node),
					len(graph.Relation),
				)

				mu.Lock()
				allNodes = append(allNodes, graph.Node...)
				allRelations = append(allRelations, graph.Relation...)
				mu.Unlock()
			}(kbID)
		}
	}

	wg.Wait()

	// Merge graph data
	chatManage.GraphResult = &types.GraphData{
		Node:     allNodes,
		Relation: allRelations,
	}
	logger.Infof(ctx, "Total entity search result: %d nodes, %d relations", len(allNodes), len(allRelations))

	chunkIDs := filterSeenChunk(ctx, chatManage.GraphResult, chatManage.SearchResult)
	if len(chunkIDs) == 0 {
		logger.Infof(ctx, "No new chunk found")
		return next()
	}
	chunks, err := p.chunkRepo.ListChunksByID(ctx, ctx.Value(types.TenantIDContextKey).(uint64), chunkIDs)
	if err != nil {
		logger.Errorf(ctx, "Failed to list chunks, session_id: %s, error: %v", chatManage.SessionID, err)
		return next()
	}
	knowledgeIDs := []string{}
	for _, chunk := range chunks {
		knowledgeIDs = append(knowledgeIDs, chunk.KnowledgeID)
	}
	knowledges, err := p.knowledgeRepo.GetKnowledgeBatch(
		ctx,
		ctx.Value(types.TenantIDContextKey).(uint64),
		knowledgeIDs,
	)
	if err != nil {
		logger.Errorf(ctx, "Failed to list knowledge, session_id: %s, error: %v", chatManage.SessionID, err)
		return next()
	}

	knowledgeMap := map[string]*types.Knowledge{}
	for _, knowledge := range knowledges {
		knowledgeMap[knowledge.ID] = knowledge
	}
	for _, chunk := range chunks {
		searchResult := chunk2SearchResult(chunk, knowledgeMap[chunk.KnowledgeID])
		chatManage.SearchResult = append(chatManage.SearchResult, searchResult)
	}
	// remove duplicate results
	chatManage.SearchResult = removeDuplicateResults(chatManage.SearchResult)
	if len(chatManage.SearchResult) == 0 {
		logger.Infof(ctx, "No new search result, session_id: %s", chatManage.SessionID)
		return ErrSearchNothing
	}
	logger.Infof(
		ctx,
		"search entity result count: %d, session_id: %s",
		len(chatManage.SearchResult),
		chatManage.SessionID,
	)
	return next()
}

// filterSeenChunk filters seen chunks from the graph
func filterSeenChunk(ctx context.Context, graph *types.GraphData, searchResult []*types.SearchResult) []string {
	seen := map[string]bool{}
	for _, chunk := range searchResult {
		seen[chunk.ID] = true
	}
	logger.Infof(ctx, "filterSeenChunk: seen count: %d", len(seen))

	chunkIDs := []string{}
	for _, node := range graph.Node {
		for _, chunkID := range node.Chunks {
			if seen[chunkID] {
				continue
			}
			seen[chunkID] = true
			chunkIDs = append(chunkIDs, chunkID)
		}
	}
	logger.Infof(ctx, "filterSeenChunk: new chunkIDs count: %d", len(chunkIDs))
	return chunkIDs
}

// chunk2SearchResult converts a chunk to a search result
func chunk2SearchResult(chunk *types.Chunk, knowledge *types.Knowledge) *types.SearchResult {
	return &types.SearchResult{
		ID:                chunk.ID,
		Content:           chunk.Content,
		KnowledgeID:       chunk.KnowledgeID,
		ChunkIndex:        chunk.ChunkIndex,
		KnowledgeTitle:    knowledge.Title,
		StartAt:           chunk.StartAt,
		EndAt:             chunk.EndAt,
		Seq:               chunk.ChunkIndex,
		Score:             1.0,
		MatchType:         types.MatchTypeGraph,
		Metadata:          knowledge.GetMetadata(),
		ChunkType:         string(chunk.ChunkType),
		ParentChunkID:     chunk.ParentChunkID,
		ImageInfo:         chunk.ImageInfo,
		KnowledgeFilename: knowledge.FileName,
		KnowledgeSource:   knowledge.Source,
		ChunkMetadata:     chunk.Metadata,
	}
}

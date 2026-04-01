package chatpipline

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginSearch implements search functionality for chat pipeline
type PluginSearch struct {
	knowledgeBaseService  interfaces.KnowledgeBaseService
	knowledgeService      interfaces.KnowledgeService
	chunkService          interfaces.ChunkService
	config                *config.Config
	webSearchService      interfaces.WebSearchService
	tenantService         interfaces.TenantService
	sessionService        interfaces.SessionService
	webSearchStateService interfaces.WebSearchStateService
}

func NewPluginSearch(eventManager *EventManager,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	config *config.Config,
	webSearchService interfaces.WebSearchService,
	tenantService interfaces.TenantService,
	sessionService interfaces.SessionService,
	webSearchStateService interfaces.WebSearchStateService,
) *PluginSearch {
	res := &PluginSearch{
		knowledgeBaseService:  knowledgeBaseService,
		knowledgeService:      knowledgeService,
		chunkService:          chunkService,
		config:                config,
		webSearchService:      webSearchService,
		tenantService:         tenantService,
		sessionService:        sessionService,
		webSearchStateService: webSearchStateService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginSearch) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_SEARCH}
}

// OnEvent 是搜索插件的核心入口，负责编排整个检索增强生成（RAG）的检索阶段。
// 该函数采用并发模型，同时执行内部知识库检索和外部网络搜索，并根据召回情况动态触发查询扩展，最终聚合去重结果。
//
// 执行流程详解：
// 1. 前置校验：检查是否配置了有效的搜索目标（知识库/知识点）或开启了联网搜索，若无则直接报错。
// 2. 双路并发检索：
//    - 启动两个 Goroutine 并行执行 `searchByTargets`（内部库检索）和 `searchWebIfEnabled`（联网搜索）。
//    - 使用 `sync.WaitGroup` 等待两者完成，并将结果合并至 `allResults`。
// 3. 低召回率动态扩展（Fallback 机制）：
//    - 触发条件：若 `EnableQueryExpansion` 开启且首轮召回数量低于阈值（`EmbeddingTopK/2`）。
//    - 扩展策略：调用 `expandQueries` 生成关键词变体（去除停用词、疑问词等）。
//    - 二次检索：针对变体执行纯关键词搜索（`DisableVectorMatch: true`），并放宽关键词阈值，以获取更广泛的匹配。
//    - 并发控制：使用信号量限制扩展检索的最大并发数（16），防止数据库过载。
// 4. 上下文与去重：
//    - 历史回溯：调用 `getSearchResultFromHistory` 将上一轮对话引用的文档加入结果集，保持上下文连贯。
//    - 全局去重：调用 `removeDuplicateResults`，基于 ID 和内容指纹剔除重复项。
// 5. 流程流转：
//    - 记录最终结果的分数日志。
//    - 若有结果，调用 `next()` 进入后续环节（通常是 Rerank 重排序）；若无结果，返回 `ErrSearchNothing`。

// OnEvent handles search events in the chat pipeline
func (p *PluginSearch) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {

	// 检查是否有检索目标（私有知识库 ID、具体的知识点 ID 或联网搜索开关）。
	// 如果没有配置任何搜索源，直接报错返回，节省计算资源。

	// Check if we have search targets or web search enabled
	hasKBTargets := len(chatManage.SearchTargets) > 0 || len(chatManage.KnowledgeBaseIDs) > 0 || len(chatManage.KnowledgeIDs) > 0
	if !hasKBTargets && !chatManage.WebSearchEnabled {
		pipelineError(ctx, "Search", "kb_not_found", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return nil
	}

	pipelineInfo(ctx, "Search", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"rewrite_query":  chatManage.RewriteQuery,
		"search_targets": len(chatManage.SearchTargets),
		"tenant_id":      chatManage.TenantID,
		"web_enabled":    chatManage.WebSearchEnabled,
	})

	// 双路并行检索：同时开启私有库检索和互联网检索。
	// Run KB search and web search concurrently
	pipelineInfo(ctx, "Search", "plan", map[string]interface{}{
		"search_targets":    len(chatManage.SearchTargets),
		"embedding_top_k":   chatManage.EmbeddingTopK,
		"vector_threshold":  chatManage.VectorThreshold,
		"keyword_threshold": chatManage.KeywordThreshold,
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allResults := make([]*types.SearchResult, 0)

	wg.Add(2)

	// 协程 A：搜内部知识库
	// Goroutine 1: Knowledge base search using SearchTargets
	go func() {
		defer wg.Done()
		kbResults := p.searchByTargets(ctx, chatManage)
		if len(kbResults) > 0 {
			mu.Lock()
			allResults = append(allResults, kbResults...)
			mu.Unlock()
		}
	}()

	// 协程 B：搜互联网
	// Goroutine 2: Web search (if enabled)
	go func() {
		defer wg.Done()
		webResults := p.searchWebIfEnabled(ctx, chatManage)
		if len(webResults) > 0 {
			mu.Lock()
			allResults = append(allResults, webResults...)
			mu.Unlock()
		}
	}()

	wg.Wait()

	chatManage.SearchResult = allResults

	// 原始分数留底：记录原始分数，以便和后面“扩写搜索”或“去重后”的结果做对比，方便调试。
	// Log all search results with scores before any processing
	for i, r := range chatManage.SearchResult {
		pipelineInfo(ctx, "Search", "result_score_before_normalize", map[string]interface{}{
			"index":      i,
			"chunk_id":   r.ID,
			"score":      fmt.Sprintf("%.4f", r.Score),
			"match_type": r.MatchType,
		})
	}

	// If recall is low, attempt query expansion with keyword-focused search
	//
	// 触发条件：结果数 < EmbeddingTopK/2
	if chatManage.EnableQueryExpansion && len(chatManage.SearchResult) < max(1, chatManage.EmbeddingTopK/2) {
		pipelineInfo(ctx, "Search", "recall_low", map[string]interface{}{
			"current":   len(chatManage.SearchResult),
			"threshold": chatManage.EmbeddingTopK / 2,
		})

		// 生成查询变体（比如把“怎么修电脑”变成“电脑 维修 方法”）
		expansions := p.expandQueries(ctx, chatManage)
		if len(expansions) > 0 {
			// 二次检索：打开关键词搜索（DisableVectorMatch: true），因为关键词更泛化，并放宽阈值
			pipelineInfo(ctx, "Search", "expansion_start", map[string]interface{}{
				"variants": len(expansions),
			})
			expTopK := max(chatManage.EmbeddingTopK*2, chatManage.RerankTopK*2)
			expKwTh := chatManage.KeywordThreshold * 0.8
			// Concurrent expansion retrieval across queries and search targets
			expResults := make([]*types.SearchResult, 0, expTopK*len(expansions))
			var muExp sync.Mutex
			var wgExp sync.WaitGroup
			jobs := len(expansions) * len(chatManage.SearchTargets)
			capSem := 16 // 最大并发度
			if jobs < capSem {
				capSem = jobs
			}
			if capSem <= 0 {
				capSem = 1
			}
			sem := make(chan struct{}, capSem) // 信号量
			pipelineInfo(ctx, "Search", "expansion_concurrency", map[string]interface{}{
				"jobs": jobs,
				"cap":  capSem,
			})

			// 对每一个新关键词，去每一个目标库里搜，限制最大并发数 (16)，防止把数据库打挂
			for _, q := range expansions {
				for _, target := range chatManage.SearchTargets {
					wgExp.Add(1)
					go func(q string, t *types.SearchTarget) {
						// 执行混合搜索 HybridSearch
						defer wgExp.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						paramsExp := types.SearchParams{
							QueryText:            q,
							VectorThreshold:      chatManage.VectorThreshold,
							KeywordThreshold:     expKwTh,
							MatchCount:           expTopK,
							DisableVectorMatch:   true,
							DisableKeywordsMatch: false,
						}
						// Apply knowledge ID filter if this is a partial KB search
						if t.Type == types.SearchTargetTypeKnowledge {
							paramsExp.KnowledgeIDs = t.KnowledgeIDs
						}
						res, err := p.knowledgeBaseService.HybridSearch(ctx, t.KnowledgeBaseID, paramsExp)
						if err != nil {
							pipelineWarn(ctx, "Search", "expansion_error", map[string]interface{}{
								"kb_id": t.KnowledgeBaseID,
								"error": err.Error(),
							})
							return
						}
						if len(res) > 0 {
							pipelineInfo(ctx, "Search", "expansion_hits", map[string]interface{}{
								"kb_id": t.KnowledgeBaseID,
								"query": q,
								"hits":  len(res),
							})
							muExp.Lock()
							expResults = append(expResults, res...)
							muExp.Unlock()
						}
					}(q, target)
				}
			}
			wgExp.Wait()
			if len(expResults) > 0 {
				// Scores already normalized in HybridSearch
				pipelineInfo(ctx, "Search", "expansion_done", map[string]interface{}{
					"added": len(expResults),
				})
				// 扩展查询的结果加到总结果里
				chatManage.SearchResult = append(chatManage.SearchResult, expResults...)
			}
		}
	}

	// 加入历史引用：如果上一轮对话用到过某些文档，这次也强行加进来（保持上下文连贯）
	// Add relevant results from chat history
	historyResult := p.getSearchResultFromHistory(chatManage)
	if historyResult != nil {
		pipelineInfo(ctx, "Search", "history_hits", map[string]interface{}{
			"session_id":   chatManage.SessionID,
			"history_hits": len(historyResult),
		})
		chatManage.SearchResult = append(chatManage.SearchResult, historyResult...)
	}

	// 去重：把所有来源（第一轮、扩写轮、历史轮）的结果放在一起，剔除重复的（chunkid相同、内容指纹相同）
	// Remove duplicate results
	before := len(chatManage.SearchResult)
	chatManage.SearchResult = removeDuplicateResults(chatManage.SearchResult)
	pipelineInfo(ctx, "Search", "dedup_summary", map[string]interface{}{
		"before": before,
		"after":  len(chatManage.SearchResult),
	})

	// 记录最终结果的分数
	// Log final scores after all processing
	for i, r := range chatManage.SearchResult {
		pipelineInfo(ctx, "Search", "final_score", map[string]interface{}{
			"index":      i,
			"chunk_id":   r.ID,
			"score":      fmt.Sprintf("%.4f", r.Score),
			"match_type": r.MatchType,
		})
	}

	// 如果有结果，就进入下一步（通常是 Rerank 重排序）
	// Return if we have results
	if len(chatManage.SearchResult) != 0 {
		pipelineInfo(ctx, "Search", "output", map[string]interface{}{
			"session_id":   chatManage.SessionID,
			"result_count": len(chatManage.SearchResult),
		})
		return next()
	}

	// 如果没结果，返回特定错误
	pipelineWarn(ctx, "Search", "output", map[string]interface{}{
		"session_id":   chatManage.SessionID,
		"result_count": 0,
	})
	return ErrSearchNothing
}

// 当用户在多轮对话中追问时，系统不仅要看当前的问题，还要“回头看”上一轮回答里引用了哪些资料，
// getSearchResultFromHistory 函数把上一轮回答引用的资料，直接作为本轮搜索的结果。
//
// 执行流程：
// 	如果没有历史对话记录（比如是第一轮对话），就不用回溯了，直接返回 nil。
//
// 	从 History 数组的最后一个元素（即刚刚发生的那一轮对话）开始往回找，即按时间从近到远倒序追溯历史。
// 	因为用户当前的问题最可能跟上一轮提到的内容相关，而不是三轮之前的。
//
// 	检查那一轮对话中，AI 是否引用了任何知识库片段（KnowledgeReferences）。
// 	一旦发现这一轮有引用的对话，就锁定这些引用。
//
// 	将这些引用的匹配类型（MatchType）修改为 MatchTypeHistory（历史匹配）。
// 	告诉后续的精排（Rerank）和生成（LLM）阶段：“这些不是我刚刚搜出来的，而是上一轮就在用的老资料”。
//
// 业务场景举例：
//
//	假设用户和 AI 有如下对话：
//
//	第一轮：
//		用户：“帮我查一下 华为 Mate 60 的参数。”
//		搜索：系统从库里搜到了“Mate 60 规格表”。
//		AI：“Mate 60 采用麒麟芯片，屏幕 6.69 英寸...”
//	第二轮：
//		用户：“那 Pro 版本 呢？”
//		尴尬点：如果第二轮搜索因为关键词太短（只搜“Pro版本”）没搜到东西，AI 就会失去上下文。
//
//	本函数的作用：
//		它发现第二轮没搜到好东西，于是去翻第一轮。
//		发现第一轮引用了“Mate 60 规格表”。
//		它把这个规格表再次作为参考资料传给 AI。
//	最终效果：
//		AI 结合第一轮的资料，聪明地回答：“Pro 版本相比刚才提到的标准版，屏幕更大，且支持卫星通话……”

// getSearchResultFromHistory retrieves relevant knowledge references from chat history
func (p *PluginSearch) getSearchResultFromHistory(chatManage *types.ChatManage) []*types.SearchResult {
	if len(chatManage.History) == 0 {
		return nil
	}
	// Search history in reverse chronological order
	for i := len(chatManage.History) - 1; i >= 0; i-- {
		if len(chatManage.History[i].KnowledgeReferences) > 0 {
			// Mark all references as history matches
			for _, reference := range chatManage.History[i].KnowledgeReferences {
				reference.MatchType = types.MatchTypeHistory
			}
			return chatManage.History[i].KnowledgeReferences
		}
	}
	return nil
}

// 去重机制
//	唯一 ID 去重 (r.ID)：
//		最基本的去重，防止同一个数据块被多次添加。
//	父节点去重 (r.ParentChunkID)：
//		如果多个搜索结果指向同一个父级（例如长文档切片），只保留第一个出现的切片。
//		这能有效防止搜索结果被同一个文档的内容霸屏。
//	内容指纹去重 (buildContentSignature)：
//		即使 ID 不同，如果内容完全一致（比如不同文档里有完全相同的段落），通过计算内容的 Hash 指纹也能识别并剔除。

// removeDuplicateResults 对搜索结果进行去重处理，防止重复的上下文信息干扰模型回答。
//
// 去重逻辑说明：
// 1. ID 去重：
//    - 使用 seen 集合记录已处理的 ID。
//    - 不仅检查当前块的 ID，还检查 ParentChunkID（父块 ID）。
//    - 如果一个块与已存在的块共享 ID 或父块 ID，则视为重复并跳过。
// 2. 内容指纹去重：
//    - 调用 buildContentSignature 为每个块生成内容签名（通常是内容的哈希或摘要）。
//    - 使用 contentSig 映射记录“签名 -> 首个出现的块 ID”。
//    - 如果当前块的签名已存在，说明内容完全一致（可能是不同知识库中的重复文档），直接跳过。
// 3. 日志记录：
//    - 对跳过的块记录详细的 Debug 日志，说明是因为 ID 冲突还是内容重复被移除。

func removeDuplicateResults(results []*types.SearchResult) []*types.SearchResult {
	seen := make(map[string]bool)
	contentSig := make(map[string]string) // sig -> first chunk ID
	var uniqueResults []*types.SearchResult
	for _, r := range results {
		keys := []string{r.ID}
		if r.ParentChunkID != "" {
			keys = append(keys, "parent:"+r.ParentChunkID)
		}
		dup := false
		dupKey := ""
		for _, k := range keys {
			if seen[k] {
				dup = true
				dupKey = k
				break
			}
		}
		if dup {
			logger.Debugf(context.Background(), "Dedup: chunk %s removed due to key: %s", r.ID, dupKey)
			continue
		}
		sig := buildContentSignature(r.Content)
		if sig != "" {
			if firstChunk, exists := contentSig[sig]; exists {
				logger.Debugf(context.Background(), "Dedup: chunk %s removed due to content signature (dup of %s, sig prefix: %.50s...)", r.ID, firstChunk, sig)
				continue
			}
			contentSig[sig] = r.ID
		}
		for _, k := range keys {
			seen[k] = true
		}
		uniqueResults = append(uniqueResults, r)
	}
	return uniqueResults
}

func buildContentSignature(content string) string {
	return searchutil.BuildContentSignature(content)
}

// searchByTargets 基于预计算的 SearchTargets 执行知识库搜索，这是利用统一搜索目标进行检索的核心方法。
// 该函数采用并发模型，针对每个目标独立执行搜索，并智能处理“小文件直接加载”与“大文件向量检索”的降级逻辑。
//
// 执行流程详解：
// 1. 并发调度：
//    - 遍历 chatManage.SearchTargets，为每个目标启动一个 Goroutine 并行处理。
//    - 使用 sync.WaitGroup 等待所有搜索任务完成，使用 sync.Mutex 保护共享结果切片的并发写入。
// 2. 智能加载策略（针对知识库类型目标）：
//    - 优先尝试直接加载：调用 tryDirectChunkLoading 尝试直接读取文件块。适用于体积小、无需复杂检索的文件。
//    - 结果分流：
//      - 成功直接加载的部分：直接加入结果集，赋予最高相关性。
//      - 因体积过大被跳过的部分（skippedIDs）：将这些 ID 加入后续的向量搜索队列。
//    - 短路逻辑：如果所有文件都被成功直接加载（skippedIDs 为空），则跳过后续的向量搜索步骤。
// 3. 混合检索执行：
//    - 对于未被直接加载的 ID（或集合/其他类型的目标），构建 SearchParams。
//    - 调用 knowledgeBaseService.HybridSearch 执行向量+关键词的混合检索。
// 4. 结果聚合：
//    - 将直接加载的结果和混合检索的结果合并。
//    - 记录详细的 Pipeline 日志，包括直接加载数量、跳过数量及最终检索命中的总数。

// searchByTargets performs KB searches using pre-computed SearchTargets
// This is the main search method that uses the unified search targets
func (p *PluginSearch) searchByTargets(
	ctx context.Context,
	chatManage *types.ChatManage,
) []*types.SearchResult {
	if len(chatManage.SearchTargets) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []*types.SearchResult

	// Search each target concurrently
	for _, target := range chatManage.SearchTargets {
		wg.Add(1)
		go func(t *types.SearchTarget) {
			defer wg.Done()

			// List of knowledge IDs to perform vector search on
			// Default to all IDs in the target
			searchKnowledgeIDs := t.KnowledgeIDs

			// Try direct loading for specific knowledge targets
			if t.Type == types.SearchTargetTypeKnowledge {
				directResults, skippedIDs := p.tryDirectChunkLoading(ctx, chatManage.TenantID, t.KnowledgeIDs)

				if len(directResults) > 0 {
					pipelineInfo(ctx, "Search", "direct_load", map[string]interface{}{
						"kb_id":        t.KnowledgeBaseID,
						"loaded_count": len(directResults),
						"skipped_ids":  len(skippedIDs),
					})
					mu.Lock()
					results = append(results, directResults...)
					mu.Unlock()
				}

				// If all files were loaded directly, we don't need to search anything
				if len(skippedIDs) == 0 && len(t.KnowledgeIDs) > 0 {
					return
				}

				// Otherwise, only search the files that were skipped (too large)
				searchKnowledgeIDs = skippedIDs
			}

			// If no IDs left to search (and we are in Knowledge mode), we are done
			if t.Type == types.SearchTargetTypeKnowledge && len(searchKnowledgeIDs) == 0 {
				return
			}

			// Build params for rewrite query
			params := types.SearchParams{
				QueryText:        strings.TrimSpace(chatManage.RewriteQuery),
				VectorThreshold:  chatManage.VectorThreshold,
				KeywordThreshold: chatManage.KeywordThreshold,
				MatchCount:       chatManage.EmbeddingTopK,
			}
			// Apply knowledge ID filter if this is a partial KB search
			if t.Type == types.SearchTargetTypeKnowledge {
				params.KnowledgeIDs = searchKnowledgeIDs
			}
			res, err := p.knowledgeBaseService.HybridSearch(ctx, t.KnowledgeBaseID, params)
			if err != nil {
				pipelineWarn(ctx, "Search", "kb_search_error", map[string]interface{}{
					"kb_id":       t.KnowledgeBaseID,
					"target_type": t.Type,
					"query":       params.QueryText,
					"error":       err.Error(),
				})
				return
			}
			pipelineInfo(ctx, "Search", "kb_result", map[string]interface{}{
				"kb_id":       t.KnowledgeBaseID,
				"target_type": t.Type,
				"hit_count":   len(res),
			})
			mu.Lock()
			results = append(results, res...)
			mu.Unlock()
		}(target)
	}

	wg.Wait()

	pipelineInfo(ctx, "Search", "kb_result_summary", map[string]interface{}{
		"total_hits": len(results),
	})
	return results
}

// tryDirectChunkLoading 尝试直接加载指定知识库 ID 的文件块数据，绕过常规的向量检索流程。
// 该函数主要用于处理小文件或精确命中的场景，以极低的延迟返回完整内容。
//
// 返回值说明：
// - []*types.SearchResult: 成功直接加载的文件块转换后的搜索结果列表。
// - []string: 因超出限制而被跳过的知识库 ID 列表（调用方可针对这些 ID 执行后续的混合搜索）。
//
// 执行过程详解：
// 1. 容量预检与防溢出：设定了 maxTotalChunks (50个块) 的全局阈值，防止因一次性加载过多数据导致内存溢出 (OOM) 或上下文过大。
// 2. 分批加载与熔断：
//    - 遍历输入的 knowledgeIDs。
//    - 调用 ListChunksByKnowledgeID 获取文件块。
//    - 在累加过程中实时检查总数，一旦当前已加载数量加上新获取的数量超过阈值，立即停止后续加载，并将剩余的 ID 标记为“跳过”。
// 3. 元数据批量获取：
//    - 收集所有成功加载的 Knowledge ID。
//    - 调用 GetKnowledgeBatchWithSharedAccess 批量获取这些文件的元数据（如标题、文件名、来源等），以 enrich 搜索结果。
// 4. 结果构建：
//    - 将 Chunk 数据转换为 SearchResult 结构。
//    - 设置 Score 为 1.0（最高分），MatchType 为 DirectLoad，表明这是直接加载的精确结果。
//    - 合并文件元数据到结果中。

// tryDirectChunkLoading attempts to load chunks for given knowledge IDs directly
// Returns loaded results and a list of knowledge IDs that were skipped (e.g. due to size limits)
func (p *PluginSearch) tryDirectChunkLoading(ctx context.Context, tenantID uint64, knowledgeIDs []string) ([]*types.SearchResult, []string) {
	if len(knowledgeIDs) == 0 {
		return nil, nil
	}

	// Limit direct loading to avoid OOM or context overflow
	// 50 chunks * ~500 chars/chunk ~= 25k chars
	const maxTotalChunks = 50

	var allChunks []*types.Chunk
	var skippedIDs []string
	loadedKnowledgeIDs := make(map[string]bool)

	for _, kid := range knowledgeIDs {
		// Optimization: Check chunk count first if possible?
		chunks, err := p.chunkService.ListChunksByKnowledgeID(ctx, kid)
		if err != nil {
			logger.Warnf(ctx, "DirectLoad: Failed to list chunks for knowledge %s: %v", kid, err)
			skippedIDs = append(skippedIDs, kid)
			continue
		}

		if len(allChunks)+len(chunks) > maxTotalChunks {
			logger.Infof(ctx, "DirectLoad: Skipped knowledge %s due to size limit (%d + %d > %d)",
				kid, len(allChunks), len(chunks), maxTotalChunks)
			skippedIDs = append(skippedIDs, kid)
			continue
		}
		allChunks = append(allChunks, chunks...)
		loadedKnowledgeIDs[kid] = true
	}

	if len(allChunks) == 0 {
		return nil, skippedIDs
	}

	// Fetch Knowledge metadata
	var uniqueKIDs []string
	for kid := range loadedKnowledgeIDs {
		uniqueKIDs = append(uniqueKIDs, kid)
	}

	knowledgeMap := make(map[string]*types.Knowledge)
	if len(uniqueKIDs) > 0 {
		knowledges, err := p.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, uniqueKIDs)
		if err != nil {
			logger.Warnf(ctx, "DirectLoad: Failed to fetch knowledge batch: %v", err)
			// Continue without metadata
		} else {
			for _, k := range knowledges {
				knowledgeMap[k.ID] = k
			}
		}
	}

	var results []*types.SearchResult
	for _, chunk := range allChunks {
		res := &types.SearchResult{
			ID:            chunk.ID,
			Content:       chunk.Content,
			Score:         1.0, // Maximum score for direct matches
			KnowledgeID:   chunk.KnowledgeID,
			ChunkIndex:    chunk.ChunkIndex,
			MatchType:     types.MatchTypeDirectLoad,
			ChunkType:     string(chunk.ChunkType),
			ParentChunkID: chunk.ParentChunkID,
			ImageInfo:     chunk.ImageInfo,
			ChunkMetadata: chunk.Metadata,
			StartAt:       chunk.StartAt,
			EndAt:         chunk.EndAt,
		}

		if k, ok := knowledgeMap[chunk.KnowledgeID]; ok {
			res.KnowledgeTitle = k.Title
			res.KnowledgeFilename = k.FileName
			res.KnowledgeSource = k.Source
			res.Metadata = k.GetMetadata()
		}

		results = append(results, res)
	}

	return results, skippedIDs
}

// searchWebIfEnabled 在配置允许且服务就绪时，执行外部网络搜索并返回标准化的搜索结果。
// 该函数集成了 RAG 压缩机制，利用 Redis 维护会话级别的临时知识库状态，以优化搜索结果的冗余度。
//
// 执行流程说明：
// 1. 前置校验：
//    - 检查 chatManage 中是否开启了 Web 搜索开关。
//    - 检查依赖服务（webSearchService, tenantService）是否已初始化。
//    - 从上下文中获取租户信息，验证其是否配置了有效的搜索提供商（Provider）。
// 2. 执行搜索：调用 webSearchService.Search 使用重写后的查询词（RewriteQuery）获取原始网络搜索结果。
// 3. RAG 压缩与去重（核心优化）：
//    - 加载状态：从 Redis 中获取当前会话（SessionID）的临时知识库 ID 及已见过的 URL 列表。
//    - 执行压缩：调用 CompressWithRAG，结合历史状态对新的搜索结果进行过滤和压缩，剔除重复或低价值信息。
//    - 保存状态：将更新后的临时知识库 ID 和已见 URL 列表持久化回 Redis，供下一轮对话使用。
// 4. 结果转换：将处理后的结果通过 ConvertWebSearchResults 转换为统一的 SearchResult 格式并返回。
// 5. 监控：全过程包含详细的 Pipeline 日志，记录配置缺失、搜索错误及最终命中的数量。

// searchWebIfEnabled executes web search when enabled and returns converted results
func (p *PluginSearch) searchWebIfEnabled(ctx context.Context, chatManage *types.ChatManage) []*types.SearchResult {
	if !chatManage.WebSearchEnabled || p.webSearchService == nil || p.tenantService == nil {
		return nil
	}
	tenant := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant == nil || tenant.WebSearchConfig == nil || tenant.WebSearchConfig.Provider == "" {
		pipelineWarn(ctx, "Search", "web_config_missing", map[string]interface{}{
			"tenant_id": chatManage.TenantID,
		})
		return nil
	}

	pipelineInfo(ctx, "Search", "web_request", map[string]interface{}{
		"tenant_id": chatManage.TenantID,
		"provider":  tenant.WebSearchConfig.Provider,
	})
	webResults, err := p.webSearchService.Search(ctx, tenant.WebSearchConfig, chatManage.RewriteQuery)
	if err != nil {
		pipelineWarn(ctx, "Search", "web_search_error", map[string]interface{}{
			"tenant_id": chatManage.TenantID,
			"error":     err.Error(),
		})
		return nil
	}
	// Build questions using RewriteQuery only
	questions := []string{strings.TrimSpace(chatManage.RewriteQuery)}
	// Load session-scoped temp KB state from Redis using WebSearchStateRepository
	tempKBID, seen, ids := p.webSearchStateService.GetWebSearchTempKBState(ctx, chatManage.SessionID)
	compressed, kbID, newSeen, newIDs, err := p.webSearchService.CompressWithRAG(
		ctx, chatManage.SessionID, tempKBID, questions, webResults, tenant.WebSearchConfig,
		p.knowledgeBaseService, p.knowledgeService, seen, ids,
	)
	if err != nil {
		pipelineWarn(ctx, "Search", "web_compress_error", map[string]interface{}{
			"error": err.Error(),
		})
	} else {
		webResults = compressed
		// Persist temp KB state back into Redis using WebSearchStateRepository
		p.webSearchStateService.SaveWebSearchTempKBState(ctx, chatManage.SessionID, kbID, newSeen, newIDs)
	}
	res := searchutil.ConvertWebSearchResults(webResults)
	pipelineInfo(ctx, "Search", "web_hits", map[string]interface{}{
		"hit_count": len(res),
	})
	return res
}

// expandQueries 在本地生成查询变体，不依赖 LLM，旨在通过多样化的关键词组合提升检索召回率。
// 该函数采用轻量级规则处理，包括：去除停用词、提取关键短语、分割长句以及去除疑问词。
//
// 执行流程说明：
// 1. 去重初始化：
//    - 建立 seen 集合，预先存入原始查询词（RewriteQuery）和原始用户输入（Query）的小写形式，防止生成重复或无效的变体。
//    - 定义 addIfNew 辅助函数，用于校验字符串长度（>3字符）和唯一性，通过后才加入结果集。
// 2. 关键词提取：
//    - 调用 extractKeywords 去除常见停用词（如“的”、“是”、“在”等），仅保留核心实词。
//    - 如果核心词数量大于等于2，则将其组合成一个新的查询串。
// 3. 短语与片段提取：
//    - 引用短语：调用 extractPhrases 提取引号内的内容或关键片段。
//    - 分隔符切分：调用 splitByDelimiters 按标点符号切分，选取长度大于5的片段作为独立查询。
// 4. 疑问词清洗：
//    - 调用 removeQuestionWords 去除“什么”、“如何”、“怎么”、“为什么”等口语化疑问词，使查询更接近搜索引擎的关键词习惯。
// 5. 截断与返回：
//    - 限制最终生成的变体数量最多为 5 个，防止后续搜索请求过多。
//    - 记录生成的变体数量日志并返回切片。

// expandQueries generates query variants locally without LLM to improve keyword recall
// Uses simple techniques: word reordering, stopword removal, key phrase extraction
func (p *PluginSearch) expandQueries(ctx context.Context, chatManage *types.ChatManage) []string {
	query := strings.TrimSpace(chatManage.RewriteQuery)
	if query == "" {
		return nil
	}

	expansions := make([]string, 0, 5)
	seen := make(map[string]struct{})
	seen[strings.ToLower(query)] = struct{}{}
	if q := strings.ToLower(chatManage.Query); q != "" {
		seen[q] = struct{}{}
	}

	addIfNew := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(s) < 3 {
			return
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		expansions = append(expansions, s)
	}

	// 1. Remove common stopwords and create keyword-only variant
	keywords := extractKeywords(query)
	if len(keywords) >= 2 {
		addIfNew(strings.Join(keywords, " "))
	}

	// 2. Extract quoted phrases or key segments
	phrases := extractPhrases(query)
	for _, phrase := range phrases {
		addIfNew(phrase)
	}

	// 3. Split by common delimiters and use longest segment
	segments := splitByDelimiters(query)
	for _, seg := range segments {
		if len(seg) > 5 {
			addIfNew(seg)
		}
	}

	// 4. Remove question words (什么/如何/怎么/为什么/哪个 etc.)
	cleaned := removeQuestionWords(query)
	if cleaned != query {
		addIfNew(cleaned)
	}

	// Limit to 5 expansions
	if len(expansions) > 5 {
		expansions = expansions[:5]
	}

	pipelineInfo(ctx, "Search", "local_expansion_result", map[string]interface{}{
		"variants": len(expansions),
	})
	return expansions
}

// Common Chinese and English stopwords
var stopwords = map[string]struct{}{
	"的": {}, "是": {}, "在": {}, "了": {}, "和": {}, "与": {}, "或": {},
	"a": {}, "an": {}, "the": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "being": {}, "have": {}, "has": {}, "had": {},
	"do": {}, "does": {}, "did": {}, "will": {}, "would": {}, "could": {},
	"should": {}, "may": {}, "might": {}, "must": {}, "can": {},
	"to": {}, "of": {}, "in": {}, "for": {}, "on": {}, "with": {}, "at": {},
	"by": {}, "from": {}, "as": {}, "into": {}, "through": {}, "about": {},
	"what": {}, "how": {}, "why": {}, "when": {}, "where": {}, "which": {},
	"who": {}, "whom": {}, "whose": {},
}

// extractKeywords 从输入文本中提取核心关键词，过滤掉常见的中英文停用词。
// 该函数旨在去除对检索贡献较低的词汇（如助词、介词、代词等），仅保留具有实际语义的实词。
//
// 过滤规则说明：
// 1. 停用词表：
//   - 中文停用词：包括“的”、“是”、“在”、“了”、“和”、“与”等常见功能词。
//   - 英文停用词：包括冠词（a, an, the）、be动词（is, are, was）、助动词（do, have, will）、介词（in, on, at, for）以及疑问词（what, how, why）。
//
// 2. 处理逻辑：
//   - 首先调用 tokenize 对文本进行分词。
//   - 将分词转换为小写后与停用词表比对。
//   - 长度校验：同时过滤掉长度为 1 的单词。
//
// 3. 应用场景：
//   - 输入："如何安装 Python 环境"
//   - 输出：["安装", "Python", "环境"] （去除了 "如何"）
//   - 输入："What is the best way to learn Go"
//   - 输出：["best", "way", "learn", "Go"] （去除了 "What", "is", "the", "to"）
func extractKeywords(text string) []string {
	words := tokenize(text)
	keywords := make([]string, 0, len(words))
	for _, w := range words {
		lower := strings.ToLower(w)
		if _, isStop := stopwords[lower]; !isStop && len(w) > 1 {
			keywords = append(keywords, w)
		}
	}
	return keywords
}

// 该函数旨在识别用户显式强调的内容（如专有名词、特定引用），以便在搜索时给予更高优先级。
//
// 提取规则说明：
// 1. 支持的引号类型：
//   - 英文双引号 (")
//   - 英文单引号 (')
//   - 中文直角引号（「 」）
//   - 中文双直角引号（『 』）
//
// 2. 过滤逻辑：
//   - 使用正则表达式 `["'"'「」『』]([^"'"'「」『』]+)["'"'「」『』]` 匹配引号内的非空内容。
//   - 长度校验：仅保留长度大于 2 的短语，以过滤掉无意义的单字或短词（如 "a", "的"）。
//
// 3. 应用场景：
//   - 输入：请帮我搜索 "Deep Learning" 相关的资料
//   - 输出：["Deep Learning"]
//   - 输入：查找 『三体』 这本书
//   - 输出：["三体"]
func extractPhrases(text string) []string {
	// Extract quoted content
	var phrases []string
	re := regexp.MustCompile(`["'"'「」『』]([^"'"'「」『』]+)["'"'「」『』]`)
	matches := re.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) > 1 && len(m[1]) > 2 {
			phrases = append(phrases, m[1])
		}
	}
	return phrases
}

// splitByDelimiters 根据常见的中英文标点符号及空白字符将文本分割为独立的片段。
// 该函数主要用于查询预处理阶段，将复杂的长句拆解为更短的语义片段，以提升检索的灵活性。
//
// 分割规则说明：
// 1. 分隔符定义：使用正则表达式 `[,，;；、。！？!?\s]+` 匹配分隔符。
//   - 涵盖中文标点：逗号（,，）、分号（;；）、顿号（、）、句号（。）、感叹号（！？）。
//   - 涵盖英文标点：逗号（,）、分号（;）、感叹号（!）、问号（?）。
//   - 涵盖空白字符：空格、制表符、换行符等（\s）。
//
// 2. 处理逻辑：
//   - 利用 regexp.Split 将文本切分。
//   - 遍历切分后的片段，去除首尾空白字符（strings.TrimSpace）。
//   - 过滤掉因连续分隔符产生的空字符串，仅保留有效内容。
//
// 3. 应用场景：
//   - 输入："你好，世界！这是一个测试。"
//   - 输出：["你好", "世界", "这是一个测试"]
func splitByDelimiters(text string) []string {
	// Split by common delimiters
	re := regexp.MustCompile(`[,，;；、。！？!?\s]+`)
	parts := re.Split(text, -1)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// Question words in Chinese
var questionWords = regexp.MustCompile(`^(什么是|什么|如何|怎么|怎样|为什么|为何|哪个|哪些|谁|何时|何地|请问|请告诉我|帮我|我想知道|我想了解)`)

func removeQuestionWords(text string) string {
	return strings.TrimSpace(questionWords.ReplaceAllString(text, ""))
}

// tokenize 对输入文本进行简易分词，专门针对中英文混合场景进行了优化。
// 该函数不依赖外部 NLP 库，而是基于 Unicode 字符属性进行流式处理。
//
// 分词规则说明：
// 1. 英文与数字：连续的字母（`unicode.IsLetter`）或数字（`unicode.IsDigit`）会被聚合为一个完整的 Token（例如："Search123" -> ["Search123"]）。
// 2. 中文字：每个汉字（`unicode.Is(unicode.Han, r)`）会被视为独立的 Token 。
//    - 注意：这会导致中文词语被拆分为单字（例如："搜索" -> ["搜", "索"]），适用于简单的字符级检索。
// 3. 分隔符处理：空格、标点符号或其他非字母数字字符被视为分隔符，会触发当前英文单词的截断与输出，但自身不会被保留在结果中。
// 4. 混合场景：
//    - "AI搜索" -> ["AI", "搜", "索"]
//    - "Hello World" -> ["Hello", "World"]
//    - "Go语言" -> ["Go", "语", "言"]
//
// 参数:
//   - text: 待分词的原始字符串，支持中英文混合
//
// 返回值:
//   - []string: 分词后的 token 列表，按原文顺序排列
//
// 使用场景:
//   - 搜索系统的查询词解析
//   - 倒排索引的文档预处理
//   - 简单文本分析的关键词提取
//
// 注意事项:
//   - 该分词器为轻量级实现，不进行词典匹配或语义分析
//   - 中文未实现词语级切分（如"人工智能"不会切为"人工"+"智能"）
//   - 对于专业术语或新词，建议配合 N-gram 或外部分词库使用
//   - 全角标点与半角标点统一作为分隔符处理

func tokenize(text string) []string {
	var tokens []string
	var current strings.Builder

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else if unicode.Is(unicode.Han, r) {
			// Flush current token
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			// Chinese character as single token
			tokens = append(tokens, string(r))
		} else {
			// Delimiter
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

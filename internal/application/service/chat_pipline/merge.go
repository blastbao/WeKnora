package chatpipline

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// 1. 核心流程：三阶段处理
//该插件对检索结果进行了三个层面的加工：
//
//	第一阶段：空间维度的物理合并 (OnEvent 主逻辑)
//		分组：按 KnowledgeID（文档 ID）和 ChunkType（类型）将片段归类。
//		排序与拼接：在同一个文档内，按片段在原书中的位置（StartAt）排序。如果两个片段重叠或相邻，就将它们合并成一个大片段。
//		图片元数据合并：调用 mergeImageInfo，确保合并后的文本段落依然关联着原来所有小片段里的图片（并自动去重）。
//	第二阶段：业务维度的内容充实 (populateFAQAnswers)
//		针对 FAQ 类型：检索回来的可能只是问题的 Embedding，不包含完整的答案。
//		动作：根据 Chunk ID 去数据库（chunkRepo）拉取完整的标准问答对，并格式化为 Q: ... Answer: ... 的形式。
//	第三阶段：语义维度的上下文扩展 (expandShortContextWithNeighbors)
//		触发条件：如果一个文本片段太短（少于 350 字），LLM 可能无法理解其深意。
//		动作：根据片段记录的 PreChunkID 和 NextChunkID，自动向前后“借”内容，直到长度达标（350-850 字之间）。
//		智能拼接：使用 concatNoOverlap 算法，自动识别并消除段落衔接处的重复文字。

// 核心功能详解
//
//	1. 碎片合并 (Chunk Merging)
//
//		在向量检索中，由于切片（Chunking）通常带有重叠区，搜索结果里经常会出现内容高度相似且位置相邻的片段。
//
//		逻辑：根据 KnowledgeID（文档ID）分组，并按 StartAt（起始位置）排序。
//		动作：如果两个片段在原文档中是连续或重叠的，插件会将它们整合成一个长片段。
//		处理：
//			如果两个片段不重叠，就保留为两个独立片段。
//			如果两个片段重叠，代码会把它们“缝合”起来，缝合文字的同时，调用 mergeImageInfo 将两个片段关联的图片信息也合并在一起并去重，确保图片描述不丢失。
//		结果：
//			原本可能召回了 10 个乱糟糟的重叠碎片，经过这里处理后，变成了 3-4 个逻辑连贯的完整段落。
//
//	2. FAQ 答案补全 (FAQ Enrichment)
//		向量数据库有时只存储了 FAQ 的问题（以此提高检索匹配率），但是没有答案或者答案不详细。
//		在向量数据库检索到了一个 ID 为 faq_101 的片段，但知识库里存的可能只是个标题，或者不完整的摘要。
//
//		逻辑：识别类型为 ChunkTypeFAQ 的片段。
//		动作：调用 populateFAQAnswers 根据 ChunkID 去数据库查出完整的标准答案。
//		逻辑：
//			插件发现当前片段是 FAQ 类型。
//			它会拿着这个 ID 去数据库（chunkRepo）里查一遍真正的详细数据。
//			把数据库里完整的“标准问题”和“标准答案”拼成一个字符串（格式：Q: ... \n A: ...）。
//		结果：
//			把检索结果里的占位符，替换成了大模型可以直接阅读的标准问答内容。
//			利用 buildFAQAnswerContent 将其格式化为清晰的 Q: ... \n Answer: - ... 结构，方便 LLM 阅读。
//
//	3. 短文本扩充 (Context Expansion)
//		有些检索到的片段太短，缺乏足够的语境，导致 LLM 看不懂。
//
//		场景：
//			检索到了一句话：“光合作用释放氧气。”
//			这句话太短，大模型不知道上下文（是讲植物？还是讲化学方程式？），容易胡说八道。
//		触发：
//			片段长度小于 minLen (350字) 且类型为普通文本。
//		逻辑：
//			检测：如果片段字数少于 350 个字符（minLen）。
//			寻找邻居：去数据库里查这个片段的“上一段”（PreChunkID）和“下一段”（NextChunkID）。
//			拼接：把前文、当前文、后文拼在一起，直到长度接近 850 个字符（maxLen）或者没有邻居为止。
//			去重：拼接时会检查是否有重叠的字（比如前一段以“是”结尾，后一段以“是”开头），避免出现“释放是是氧气”的错误。
//			结果：把一句孤立的话，变成了一个包含背景信息的完整段落，极大提升了大模型回答的准确性。
//		意义：
//			通过补全前后文，解决了“语义断层”问题，极大提升了模型生成的准确度。
//
//	4. 智能去重拼接 (concatNoOverlap)
//		在合并或扩充时，最怕出现“复读机”现象（即 A 的结尾和 B 的开头重复了）。
//
//		实现：concatNoOverlap 函数会计算两个字符串的最大交集。
//		效果：如果 A 是 “我是中国...”，B 是 “中国心”，它会聪明地拼成 “我是中国心”，而不是 “我是中国中国心”。

// PluginMerge handles merging of search result chunks
type PluginMerge struct {
	chunkRepo interfaces.ChunkRepository
}

// NewPluginMerge creates and registers a new PluginMerge instance
func NewPluginMerge(eventManager *EventManager, chunkRepo interfaces.ChunkRepository) *PluginMerge {
	res := &PluginMerge{
		chunkRepo: chunkRepo,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginMerge) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_MERGE}
}

// OnEvent 处理 CHUNK_MERGE 事件，执行知识片段的物理合并与逻辑增强。
//
// 核心逻辑：
// 1. 物理合并：在同一文档内，根据偏移量 (StartAt/EndAt) 将相邻或重叠的片段合并，减少冗余。
// 2. 图像整合：合并过程中同步去重并保留所有关联的图片元数据。
// 3. FAQ 填充：将 FAQ 类型的片段补全为完整的“问-答”结构。
// 4. 上下文扩展：对过短的文本片段进行邻居节点动态扩展，确保 LLM 获得足够的上下文信息。

// OnEvent processes the CHUNK_MERGE event to merge search result chunks
func (p *PluginMerge) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "Merge", "input", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"candidate_cnt": len(chatManage.RerankResult),
	})

	// Use rerank results if available, fallback to search results
	searchResult := chatManage.RerankResult
	if len(searchResult) == 0 {
		pipelineWarn(ctx, "Merge", "fallback", map[string]interface{}{
			"reason": "empty_rerank_result",
		})
		searchResult = chatManage.SearchResult
	}

	pipelineInfo(ctx, "Merge", "candidate_ready", map[string]interface{}{
		"chunk_cnt": len(searchResult),
	})

	// If there are no search results, return early
	if len(searchResult) == 0 {
		pipelineWarn(ctx, "Merge", "output", map[string]interface{}{
			"chunk_cnt": 0,
			"reason":    "no_candidates",
		})
		return next()
	}

	// Group chunks by their knowledge source ID
	knowledgeGroup := make(map[string]map[string][]*types.SearchResult)
	for _, chunk := range searchResult {
		if _, ok := knowledgeGroup[chunk.KnowledgeID]; !ok {
			knowledgeGroup[chunk.KnowledgeID] = make(map[string][]*types.SearchResult)
		}
		knowledgeGroup[chunk.KnowledgeID][chunk.ChunkType] = append(knowledgeGroup[chunk.KnowledgeID][chunk.ChunkType], chunk)
	}

	pipelineInfo(ctx, "Merge", "group_summary", map[string]interface{}{
		"knowledge_cnt": len(knowledgeGroup),
	})

	// 按 KnowledgeID（文档 ID）和 ChunkType（片段类型）对搜索结果进行双重分组。
	// 确保只有属于同一份文件且类型相同（如都是文本或都是 FAQ）的片段才会被尝试合并。
	//
	// 根据片段在原文档中的起始字符位置 (StartAt) 进行升序排列。
	//
	// 如果当前片段的开始位置在最后一个片段的结束位置之后（中间有空隙），说明它们不相邻，直接将其作为独立片段存入结果集。
	// 如果两个片段存在重叠或刚好相连，执行缝合操作，包括去重（把当前片段中“多出来的部分”粘贴到上一个片段的末尾）、更新 EndAt 指针、合并图片信息。
	//
	// 合并后的长片段分值取所有子片段中的最高分，保证即便合并了低分片段，这个“超级片段”在后续排序中依然能凭借其最核心的部分保留高优先级。
	//
	// 在每个组内部合并完后，按分值从高到低重新排序，确保发给 LLM 的资料是按相关性排序的，最相关的长片段排在最前面。

	mergedChunks := []*types.SearchResult{}
	// Process each knowledge source separately
	for knowledgeID, chunkGroup := range knowledgeGroup {
		for _, chunks := range chunkGroup {
			pipelineInfo(ctx, "Merge", "group_process", map[string]interface{}{
				"knowledge_id": knowledgeID,
				"chunk_cnt":    len(chunks),
			})

			// Sort chunks by their start position in the original document
			sort.Slice(chunks, func(i, j int) bool {
				if chunks[i].StartAt == chunks[j].StartAt {
					return chunks[i].EndAt < chunks[j].EndAt
				}
				return chunks[i].StartAt < chunks[j].StartAt
			})

			// Merge overlapping or adjacent chunks
			knowledgeMergedChunks := []*types.SearchResult{chunks[0]}
			for i := 1; i < len(chunks); i++ {
				lastChunk := knowledgeMergedChunks[len(knowledgeMergedChunks)-1]
				// If the current chunk starts after the last chunk ends, add it to the merged chunks
				if chunks[i].StartAt > lastChunk.EndAt {
					knowledgeMergedChunks = append(knowledgeMergedChunks, chunks[i])
					continue
				}
				// Merge overlapping chunks
				if chunks[i].EndAt > lastChunk.EndAt {
					contentRunes := []rune(chunks[i].Content)
					offset := len(contentRunes) - (chunks[i].EndAt - lastChunk.EndAt)
					lastChunk.Content = lastChunk.Content + string(contentRunes[offset:])
					lastChunk.EndAt = chunks[i].EndAt
					lastChunk.SubChunkID = append(lastChunk.SubChunkID, chunks[i].ID)

					// 合并 ImageInfo
					if err := mergeImageInfo(ctx, lastChunk, chunks[i]); err != nil {
						pipelineWarn(ctx, "Merge", "image_merge", map[string]interface{}{
							"knowledge_id": knowledgeID,
							"error":        err.Error(),
						})
					}
				}
				if chunks[i].Score > lastChunk.Score {
					lastChunk.Score = chunks[i].Score
				}
			}

			pipelineInfo(ctx, "Merge", "group_output", map[string]interface{}{
				"knowledge_id":  knowledgeID,
				"merged_chunks": len(knowledgeMergedChunks),
			})

			// Sort merged chunks by their score (highest first)
			sort.Slice(knowledgeMergedChunks, func(i, j int) bool {
				return knowledgeMergedChunks[i].Score > knowledgeMergedChunks[j].Score
			})

			mergedChunks = append(mergedChunks, knowledgeMergedChunks...)
		}
	}

	pipelineInfo(ctx, "Merge", "output", map[string]interface{}{
		"merged_total": len(mergedChunks),
	})

	mergedChunks = p.populateFAQAnswers(ctx, chatManage, mergedChunks)
	mergedChunks = p.expandShortContextWithNeighbors(ctx, chatManage, mergedChunks)

	chatManage.MergeResult = mergedChunks
	return next()
}

// mergeImageInfo 合并两个chunk的ImageInfo
func mergeImageInfo(ctx context.Context, target *types.SearchResult, source *types.SearchResult) error {
	// 如果source没有ImageInfo，不需要合并
	if source.ImageInfo == "" {
		return nil
	}

	var sourceImageInfos []types.ImageInfo
	if err := json.Unmarshal([]byte(source.ImageInfo), &sourceImageInfos); err != nil {
		pipelineWarn(ctx, "Merge", "image_unmarshal_source", map[string]interface{}{
			"error": err.Error(),
		})
		return err
	}

	// 如果source的ImageInfo为空，不需要合并
	if len(sourceImageInfos) == 0 {
		return nil
	}

	// 处理target的ImageInfo
	var targetImageInfos []types.ImageInfo
	if target.ImageInfo != "" {
		if err := json.Unmarshal([]byte(target.ImageInfo), &targetImageInfos); err != nil {
			pipelineWarn(ctx, "Merge", "image_unmarshal_target", map[string]interface{}{
				"error": err.Error(),
			})
			// 如果目标解析失败，直接使用源数据
			target.ImageInfo = source.ImageInfo
			return nil
		}
	}

	// 合并ImageInfo
	targetImageInfos = append(targetImageInfos, sourceImageInfos...)

	// 去重
	uniqueMap := make(map[string]bool)
	uniqueImageInfos := make([]types.ImageInfo, 0, len(targetImageInfos))

	for _, imgInfo := range targetImageInfos {
		// 使用URL作为唯一标识
		if imgInfo.URL != "" && !uniqueMap[imgInfo.URL] {
			uniqueMap[imgInfo.URL] = true
			uniqueImageInfos = append(uniqueImageInfos, imgInfo)
		}
	}

	// 序列化合并后的ImageInfo
	mergedImageInfoJSON, err := json.Marshal(uniqueImageInfos)
	if err != nil {
		pipelineWarn(ctx, "Merge", "image_marshal", map[string]interface{}{
			"error": err.Error(),
		})
		return err
	}

	// 更新目标chunk的ImageInfo
	target.ImageInfo = string(mergedImageInfoJSON)
	pipelineInfo(ctx, "Merge", "image_merged", map[string]interface{}{
		"image_refs": len(uniqueImageInfos),
	})
	return nil
}

// populateFAQAnswers populates FAQ answers for the search results
func (p *PluginMerge) populateFAQAnswers(
	ctx context.Context,
	chatManage *types.ChatManage,
	results []*types.SearchResult,
) []*types.SearchResult {
	if len(results) == 0 || p.chunkRepo == nil {
		return results
	}

	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	if tenantID == 0 && chatManage != nil {
		tenantID = chatManage.TenantID
	}
	if tenantID == 0 {
		pipelineWarn(ctx, "Merge", "faq_enrich_skip", map[string]interface{}{
			"reason": "missing_tenant",
		})
		return results
	}

	chunkResultMap := make(map[string][]*types.SearchResult)
	chunkIDSet := make(map[string]struct{})
	for _, r := range results {
		if r == nil || r.ID == "" {
			continue
		}
		if r.ChunkType != string(types.ChunkTypeFAQ) {
			continue
		}
		chunkResultMap[r.ID] = append(chunkResultMap[r.ID], r)
		if _, exists := chunkIDSet[r.ID]; !exists {
			chunkIDSet[r.ID] = struct{}{}
		}
	}

	if len(chunkIDSet) == 0 {
		return results
	}

	chunkIDs := make([]string, 0, len(chunkIDSet))
	for id := range chunkIDSet {
		chunkIDs = append(chunkIDs, id)
	}

	chunks, err := p.chunkRepo.ListChunksByID(ctx, tenantID, chunkIDs)
	if err != nil {
		pipelineWarn(ctx, "Merge", "faq_chunk_fetch_failed", map[string]interface{}{
			"error": err.Error(),
		})
		return results
	}

	updated := 0
	for _, chunk := range chunks {
		if chunk == nil {
			continue
		}
		meta, err := chunk.FAQMetadata()
		if err != nil || meta == nil {
			if err != nil {
				pipelineWarn(ctx, "Merge", "faq_metadata_parse_failed", map[string]interface{}{
					"chunk_id": chunk.ID,
					"error":    err.Error(),
				})
			}
			continue
		}
		content := buildFAQAnswerContent(meta)
		if content == "" {
			continue
		}
		for _, r := range chunkResultMap[chunk.ID] {
			if r == nil {
				continue
			}
			r.Content = content
			updated++
		}
	}

	if updated > 0 {
		pipelineInfo(ctx, "Merge", "faq_content_enriched", map[string]interface{}{
			"chunk_cnt": updated,
		})
	}
	return results
}

// buildFAQAnswerContent builds the content of a FAQ answer
func buildFAQAnswerContent(meta *types.FAQChunkMetadata) string {
	if meta == nil {
		return ""
	}

	question := strings.TrimSpace(meta.StandardQuestion)
	answers := make([]string, 0, len(meta.Answers))
	for _, ans := range meta.Answers {
		if trimmed := strings.TrimSpace(ans); trimmed != "" {
			answers = append(answers, trimmed)
		}
	}

	if question == "" && len(answers) == 0 {
		return ""
	}

	var builder strings.Builder
	if question != "" {
		builder.WriteString("Q: ")
		builder.WriteString(question)
		builder.WriteString("\n")
	}

	if len(answers) > 0 {
		builder.WriteString("Answer:\n")
		for _, ans := range answers {
			builder.WriteString("- ")
			builder.WriteString(ans)
			builder.WriteString("\n")
		}
	}

	return strings.TrimSpace(builder.String())
}

// expandShortContextWithNeighbors 实现了一种“滑动窗口”式的动态扩展机制。
// 它通过双向链表结构 (Pre/Next ID) 递归拉取相邻 Chunk，直至片段长度满足配置要求，
// 极大地缓解了 RAG 系统中常见的“断章取义”问题。

// expandShortContextWithNeighbors expands the short context with neighbors
func (p *PluginMerge) expandShortContextWithNeighbors(
	ctx context.Context,
	chatManage *types.ChatManage,
	results []*types.SearchResult,
) []*types.SearchResult {
	const (
		minLen = 350
		maxLen = 850
	)

	if len(results) == 0 || p.chunkRepo == nil {
		return results
	}

	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	if tenantID == 0 && chatManage != nil {
		tenantID = chatManage.TenantID
	}
	if tenantID == 0 {
		pipelineWarn(ctx, "Merge", "expand_skip", map[string]interface{}{
			"reason": "missing_tenant",
		})
		return results
	}

	type targetInfo struct {
		result *types.SearchResult
	}

	targets := make([]targetInfo, 0)
	baseIDsSet := make(map[string]struct{})

	for _, r := range results {
		if r == nil || r.ID == "" || r.Content == "" {
			continue
		}
		if r.ChunkType != string(types.ChunkTypeText) {
			continue
		}
		if runeLen(r.Content) >= minLen {
			continue
		}
		targets = append(targets, targetInfo{result: r})
		baseIDsSet[r.ID] = struct{}{}
		pipelineInfo(ctx, "Merge", "need_expand", map[string]interface{}{
			"chunk_id":   r.ID,
			"content":    r.Content,
			"chunk_type": r.ChunkType,
			"len":        runeLen(r.Content),
		})
	}

	if len(targets) == 0 {
		return results
	}

	baseIDs := make([]string, 0, len(baseIDsSet))
	for id := range baseIDsSet {
		baseIDs = append(baseIDs, id)
	}

	chunkMap := make(map[string]*types.Chunk, len(baseIDs))
	chunks, err := p.chunkRepo.ListChunksByID(ctx, tenantID, baseIDs)
	if err != nil {
		pipelineWarn(ctx, "Merge", "expand_list_base_failed", map[string]interface{}{
			"error": err.Error(),
		})
		return results
	}
	for _, chunk := range chunks {
		chunkMap[chunk.ID] = chunk
	}

	neighborIDsSet := make(map[string]struct{})
	for _, chunk := range chunkMap {
		if chunk == nil {
			continue
		}
		if chunk.PreChunkID != "" {
			if _, exists := chunkMap[chunk.PreChunkID]; !exists {
				neighborIDsSet[chunk.PreChunkID] = struct{}{}
			}
		}
		if chunk.NextChunkID != "" {
			if _, exists := chunkMap[chunk.NextChunkID]; !exists {
				neighborIDsSet[chunk.NextChunkID] = struct{}{}
			}
		}
	}

	if len(neighborIDsSet) > 0 {
		neighborIDs := make([]string, 0, len(neighborIDsSet))
		for id := range neighborIDsSet {
			neighborIDs = append(neighborIDs, id)
		}
		neighbors, err := p.chunkRepo.ListChunksByID(ctx, tenantID, neighborIDs)
		if err != nil {
			pipelineWarn(ctx, "Merge", "expand_list_neighbor_failed", map[string]interface{}{
				"error": err.Error(),
			})
		} else {
			for _, chunk := range neighbors {
				chunkMap[chunk.ID] = chunk
				pipelineInfo(ctx, "Merge", "expand_list_neighbor_success", map[string]interface{}{
					"neighbor_chunk_id":   chunk.ID,
					"neighbor_content":    chunk.Content,
					"neighbor_chunk_type": chunk.ChunkType,
					"neighbor_len":        runeLen(chunk.Content),
				})
			}
		}
	}

	for _, target := range targets {
		res := target.result
		p.fetchChunksIfMissing(ctx, tenantID, chunkMap, res.ID)
		baseChunk := chunkMap[res.ID]
		if baseChunk == nil || baseChunk.Content == "" || baseChunk.ChunkType != types.ChunkTypeText {
			continue
		}

		prevContent := ""
		nextContent := ""
		prevIDs := []string{}
		nextIDs := []string{}

		prevCursor := baseChunk.PreChunkID
		nextCursor := baseChunk.NextChunkID

		p.fetchChunksIfMissing(ctx, tenantID, chunkMap, prevCursor, nextCursor)

		if prevCursor != "" {
			if prevChunk := chunkMap[prevCursor]; prevChunk != nil && prevChunk.KnowledgeID == baseChunk.KnowledgeID {
				prevContent = prevChunk.Content
				prevIDs = append(prevIDs, prevChunk.ID)
				prevCursor = prevChunk.PreChunkID
			} else {
				prevCursor = ""
			}
		}

		if nextCursor != "" {
			if nextChunk := chunkMap[nextCursor]; nextChunk != nil && nextChunk.KnowledgeID == baseChunk.KnowledgeID {
				nextContent = nextChunk.Content
				nextIDs = append(nextIDs, nextChunk.ID)
				nextCursor = nextChunk.NextChunkID
			} else {
				nextCursor = ""
			}
		}

		var merged string
		for {
			merged = mergeOrderedContent(prevContent, baseChunk.Content, nextContent, maxLen)
			if merged == "" {
				break
			}
			if runeLen(merged) >= minLen {
				break
			}
			if prevCursor == "" && nextCursor == "" {
				break
			}

			expanded := false
			if prevCursor != "" {
				p.fetchChunksIfMissing(ctx, tenantID, chunkMap, prevCursor)
				if prevChunk := chunkMap[prevCursor]; prevChunk != nil &&
					prevChunk.KnowledgeID == baseChunk.KnowledgeID {
					prevContent = concatNoOverlap(prevChunk.Content, prevContent)
					prevIDs = append([]string{prevChunk.ID}, prevIDs...)
					prevCursor = prevChunk.PreChunkID
					expanded = true
				} else {
					prevCursor = ""
				}
			}

			merged = mergeOrderedContent(prevContent, baseChunk.Content, nextContent, maxLen)
			if runeLen(merged) >= minLen {
				break
			}

			if nextCursor != "" {
				p.fetchChunksIfMissing(ctx, tenantID, chunkMap, nextCursor)
				if nextChunk := chunkMap[nextCursor]; nextChunk != nil &&
					nextChunk.KnowledgeID == baseChunk.KnowledgeID {
					nextContent = concatNoOverlap(nextContent, nextChunk.Content)
					nextIDs = append(nextIDs, nextChunk.ID)
					nextCursor = nextChunk.NextChunkID
					expanded = true
				} else {
					nextCursor = ""
				}
			}

			if !expanded {
				break
			}
		}

		if merged == "" {
			continue
		}

		beforeLen := runeLen(res.Content)
		res.Content = merged

		for _, id := range prevIDs {
			if id != "" && !containsID(res.SubChunkID, id) {
				res.SubChunkID = append(res.SubChunkID, id)
			}
		}
		for _, id := range nextIDs {
			if id != "" && !containsID(res.SubChunkID, id) {
				res.SubChunkID = append(res.SubChunkID, id)
			}
		}

		if prevContent != "" {
			res.StartAt = baseChunk.StartAt - runeLen(prevContent)
			if res.StartAt < 0 {
				res.StartAt = 0
			}
		}
		res.EndAt = res.StartAt + runeLen(res.Content)

		pipelineInfo(ctx, "Merge", "expand_short_chunk", map[string]interface{}{
			"chunk_id":       res.ID,
			"prev_ids":       prevIDs,
			"next_ids":       nextIDs,
			"before_len":     beforeLen,
			"after_len":      runeLen(res.Content),
			"base_content":   baseChunk.Content,
			"after_content":  res.Content,
			"chunk_type":     res.ChunkType,
			"remaining_prev": prevCursor,
			"remaining_next": nextCursor,
		})
	}

	return results
}

// runeLen returns the length of a string in runes
func runeLen(s string) int {
	return len([]rune(s))
}

// mergeOrderedContent merges ordered content
func mergeOrderedContent(prev, base, next string, maxLen int) string {
	content := base
	if prev != "" {
		content = concatNoOverlap(prev, content)
	}
	if next != "" {
		content = concatNoOverlap(content, next)
	}
	runes := []rune(content)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return content
}

// concatNoOverlap concatenates two strings, removing potential overlapping prefix/suffix
func concatNoOverlap(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}

	ar := []rune(a)
	br := []rune(b)
	maxOverlap := minInt(len(ar), len(br))
	for k := maxOverlap; k > 0; k-- {
		if string(ar[len(ar)-k:]) == string(br[:k]) {
			return string(ar) + string(br[k:])
		}
	}
	return string(ar) + string(br)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func (p *PluginMerge) fetchChunksIfMissing(
	ctx context.Context,
	tenantID uint64,
	chunkMap map[string]*types.Chunk,
	chunkIDs ...string,
) {
	missing := make([]string, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		if id == "" {
			continue
		}
		if _, exists := chunkMap[id]; !exists {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}

	chunks, err := p.chunkRepo.ListChunksByID(ctx, tenantID, missing)
	if err != nil {
		pipelineWarn(ctx, "Merge", "expand_fetch_missing_failed", map[string]interface{}{
			"missing_cnt": len(missing),
			"error":       err.Error(),
		})
	}

	found := make(map[string]struct{}, len(chunks))
	for _, chunk := range chunks {
		chunkMap[chunk.ID] = chunk
		found[chunk.ID] = struct{}{}
	}

	for _, id := range missing {
		if _, ok := found[id]; !ok {
			chunkMap[id] = nil
		}
	}
}

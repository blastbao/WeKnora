package chatpipline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginRerank implements reranking functionality for chat pipeline
type PluginRerank struct {
	modelService interfaces.ModelService // Service to access rerank models
}

// NewPluginRerank creates a new rerank plugin instance
func NewPluginRerank(eventManager *EventManager, modelService interfaces.ModelService) *PluginRerank {
	res := &PluginRerank{
		modelService: modelService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginRerank) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_RERANK}
}

// OnEvent 处理聊天流水线中的重排序事件
//
// 该函数作为重排序插件的事件入口，负责协调完整的重排序流程，包括：
// 候选结果筛选、富文本构建、模型调用、阈值降级策略、复合分数计算、FAQ分数提升、直接加载结果处理及 MMR 多样性重排序。
//
// 执行过程:
//  1. 输入校验与日志记录：检查搜索结果及重排序模型配置
//  2. 获取重排序模型实例
//  3. 候选结果预处理：
//     - 提取出直接加载结果（MatchTypeDirectLoad），此类结果跳过重排序模型
//     - 对其余结果调用 getEnrichedPassage 构建富文本片段，补充图片描述、OCR文本及潜在问题信息
//  4. 重排序模型调用：
//     - 使用改写后的查询（RewriteQuery）进行首次重排序
//	   - 若初次调用无结果且阈值较高（>0.3），自动将阈值降低 30% 进行二次重试，防止因门槛过高导致“零结果”的尴尬情况。
//  5. 分数计算与增强：
//     - 对重排序结果计算复合分数（compositeScore）
//     - 若启用 FAQ 优先策略，对 FAQ 类型片段应用分数提升
//     - 对直接加载结果赋予满分模型分（1.0）并计算复合分数
//  6. 多样性重排序：对合并后的结果应用 MMR 算法，筛选 TopK
//  7. 结果输出：更新 chatManage.RerankResult，记录处理摘要
//
// 参数:
//   - ctx: 请求上下文，用于日志追踪
//   - eventType: 事件类型标识
//   - chatManage: 聊天管理对象，包含搜索结果、模型配置、阈值等参数
//   - next: 流水线下一节点回调函数
//
// 返回:
//   - *PluginError: 处理成功返回 next() 结果；失败返回对应错误码

// 补充说明
//
//
// 在 RAG 流水线中，重排（Rerank）不仅仅是“排个序”，它最重要的职责其实是“过滤（Filtering）”和“截断（Truncation）”。
// 如果第一次调用重排模型，因门槛太高导致一个结果都没选出来，系统会自动把门槛降低（打7折），再试一次。
// 目的是为了保证大模型至少能拿到一点参考资料，避免出现‘无话可说’的局面。
//
//
// 在 RAG 系统的实际业务中，并不是所有资料都需要靠 AI 模型来猜“相不相关”。有些资料是业务规则硬性规定必须给 AI 看的。
// MatchTypeDirectLoad 通常代表“强制加载”或“直接指定”的数据。
// 这类数据不是通过关键词匹配或语义相似度“算”出来的，而是通过确定性逻辑找到的。
//
// 常见场景包括：
//	FAQ（常见问题解答）精准命中：
//		用户问：“怎么重置密码？”
//		系统不是去知识库里“猜”哪篇文章像，而是直接在 FAQ 表里查到了“重置密码”这个标准问题。
//		这种匹配是100% 准确的，不需要模型再去判断“像不像”。
//	人工置顶/运营干预：
//		运营人员在后台配置了：“只要用户提到‘双11活动’，必须把这篇《活动规则》放在第一条。”
//		这种是业务规则强制要求的，模型打分可能会因为语义偏差把它排在后面，所以必须“直接加载”。
//	上下文引用/明确指代：
//		用户说：“帮我总结一下刚才发的那份文档。”
//		系统直接锁定那个文档 ID，不需要再去计算它和相关性的分数。

// OnEvent handles reranking events in the chat pipeline
func (p *PluginRerank) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "Rerank", "input", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"candidate_cnt": len(chatManage.SearchResult),
		"rerank_model":  chatManage.RerankModelID,
		"rerank_thresh": chatManage.RerankThreshold,
		"rewrite_query": chatManage.RewriteQuery,
	})
	if len(chatManage.SearchResult) == 0 {
		pipelineInfo(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_search_result",
		})
		return next()
	}
	if chatManage.RerankModelID == "" {
		pipelineWarn(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_model_id",
		})
		return next()
	}

	// Get rerank model from service
	rerankModel, err := p.modelService.GetRerankModel(ctx, chatManage.RerankModelID)
	if err != nil {
		pipelineError(ctx, "Rerank", "get_model", map[string]interface{}{
			"model_id": chatManage.RerankModelID,
			"error":    err.Error(),
		})
		return ErrGetRerankModel.WithError(err)
	}

	// Prepare passages for reranking (excluding DirectLoad results)
	var passages []string
	var candidatesToRerank []*types.SearchResult
	var directLoadResults []*types.SearchResult

	for _, result := range chatManage.SearchResult {
		if result.MatchType == types.MatchTypeDirectLoad {
			directLoadResults = append(directLoadResults, result)
			pipelineInfo(ctx, "Rerank", "direct_load_skip", map[string]interface{}{
				"chunk_id": result.ID,
			})
			continue
		}
		// 合并Content和ImageInfo的文本内容
		passage := getEnrichedPassage(ctx, result)
		passages = append(passages, passage)
		candidatesToRerank = append(candidatesToRerank, result)
	}

	pipelineInfo(ctx, "Rerank", "build_passages", map[string]interface{}{
		"total_cnt":     len(chatManage.SearchResult),
		"candidate_cnt": len(candidatesToRerank),
		"direct_cnt":    len(directLoadResults),
	})

	var rerankResp []rerank.RankResult

	// Only call rerank model if there are candidates
	if len(candidatesToRerank) > 0 {
		// Single rerank call with RewriteQuery, use threshold degradation if no results
		originalThreshold := chatManage.RerankThreshold
		rerankResp = p.rerank(ctx, chatManage, rerankModel, chatManage.RewriteQuery, passages, candidatesToRerank)

		// If no results and threshold is high enough, try with lower threshold
		if len(rerankResp) == 0 && originalThreshold > 0.3 {
			degradedThreshold := originalThreshold * 0.7
			if degradedThreshold < 0.3 {
				degradedThreshold = 0.3
			}
			pipelineInfo(ctx, "Rerank", "threshold_degrade", map[string]interface{}{
				"original": originalThreshold,
				"degraded": degradedThreshold,
			})
			chatManage.RerankThreshold = degradedThreshold
			rerankResp = p.rerank(ctx, chatManage, rerankModel, chatManage.RewriteQuery, passages, candidatesToRerank)
			// Restore original threshold
			chatManage.RerankThreshold = originalThreshold
		}
	}

	pipelineInfo(ctx, "Rerank", "model_response", map[string]interface{}{
		"result_cnt": len(rerankResp),
	})

	// Log input scores before reranking for debugging
	for i, sr := range chatManage.SearchResult {
		pipelineInfo(ctx, "Rerank", "input_score", map[string]interface{}{
			"index":      i,
			"chunk_id":   sr.ID,
			"score":      fmt.Sprintf("%.4f", sr.Score),
			"match_type": sr.MatchType,
		})
	}

	for i := range chatManage.SearchResult {
		chatManage.SearchResult[i].Metadata = ensureMetadata(chatManage.SearchResult[i].Metadata)
	}
	reranked := make([]*types.SearchResult, 0, len(rerankResp)+len(directLoadResults))

	// Process reranked results
	for _, rr := range rerankResp {
		// 安全检查：索引越界跳过
		if rr.Index >= len(candidatesToRerank) {
			continue
		}
		sr := candidatesToRerank[rr.Index] // 取出对应候选结果

		// 记录基础分数（原始检索分数）
		base := sr.Score
		sr.Metadata["base_score"] = fmt.Sprintf("%.4f", base)
		modelScore := rr.RelevanceScore

		// 计算复合分数：模型分 + 原始分 + 来源权重 + 位置权重
		sr.Score = compositeScore(sr, modelScore, base)

		// FAQ 战略提权：针对 FAQ 类型的知识点进行动态分值增益 (Boost)，优先满足标准问答的召回需求。
		// 如果开启了 FAQ 优先，且当前片段是 FAQ 类型，分数直接乘以一个系数（比如 1.2 倍），最高不超过 1.0
		//
		// Apply FAQ score boost if enabled
		if chatManage.FAQPriorityEnabled && chatManage.FAQScoreBoost > 1.0 &&
			sr.ChunkType == string(types.ChunkTypeFAQ) {
			originalScore := sr.Score
			sr.Score = math.Min(sr.Score*chatManage.FAQScoreBoost, 1.0)
			sr.Metadata["faq_boosted"] = "true"
			sr.Metadata["faq_original_score"] = fmt.Sprintf("%.4f", originalScore)
			pipelineInfo(ctx, "Rerank", "faq_boost", map[string]interface{}{
				"chunk_id":       sr.ID,
				"original_score": fmt.Sprintf("%.4f", originalScore),
				"boosted_score":  fmt.Sprintf("%.4f", sr.Score),
				"boost_factor":   chatManage.FAQScoreBoost,
			})
		}

		pipelineInfo(ctx, "Rerank", "composite_calc", map[string]interface{}{
			"chunk_id":    sr.ID,
			"base_score":  fmt.Sprintf("%.4f", base),
			"model_score": fmt.Sprintf("%.4f", modelScore),
			"final_score": fmt.Sprintf("%.4f", sr.Score),
			"match_type":  sr.MatchType,
		})

		// 把处理好的结果加入临时榜单
		reranked = append(reranked, sr)
	}

	// DirectLoad（保送）片段赋予 1.0 的模型满分，确保其在结果集中占据高位。
	//
	// Process direct load results (bypass rerank model, assume high relevance)
	for _, sr := range directLoadResults {
		base := sr.Score
		sr.Metadata["base_score"] = fmt.Sprintf("%.4f", base)
		// Assign high model score for direct load items
		modelScore := 1.0
		sr.Score = compositeScore(sr, modelScore, base)
		pipelineInfo(ctx, "Rerank", "composite_calc_direct", map[string]interface{}{
			"chunk_id":    sr.ID,
			"base_score":  fmt.Sprintf("%.4f", base),
			"model_score": fmt.Sprintf("%.4f", modelScore),
			"final_score": fmt.Sprintf("%.4f", sr.Score),
			"match_type":  sr.MatchType,
		})
		reranked = append(reranked, sr)
	}

	// 即使某些资料分数很高，如果它们内容太像了，MMR 会把重复的踢掉。
	final := applyMMR(ctx, reranked, chatManage, min(len(reranked), max(1, chatManage.RerankTopK)), 0.7)
	chatManage.RerankResult = final

	// Log composite top scores and MMR selection summary
	topN := min(3, len(reranked))
	for i := 0; i < topN; i++ {
		pipelineInfo(ctx, "Rerank", "composite_top", map[string]interface{}{
			"rank":        i + 1,
			"chunk_id":    reranked[i].ID,
			"base_score":  reranked[i].Metadata["base_score"],
			"final_score": fmt.Sprintf("%.4f", reranked[i].Score),
		})
	}

	if len(chatManage.RerankResult) == 0 {
		pipelineWarn(ctx, "Rerank", "output", map[string]interface{}{
			"filtered_cnt": 0,
		})
		return ErrSearchNothing
	}

	pipelineInfo(ctx, "Rerank", "output", map[string]interface{}{
		"filtered_cnt": len(chatManage.RerankResult),
	})
	return next()
}

// rerank 执行实际的重排操作，并根据阈值过滤结果
//
// 该函数负责调用底层重排模型计算查询与候选片段的相关性得分，并包含针对特定类型（如历史记录）的阈值动态调整逻辑。
//
// 执行过程 (Execution Flow):
// 1. 模型调用: 将查询词（Query）与经过富化的文本片段（Passages）发送给重排模型进行打分。
// 2. 错误处理: 若模型调用失败，记录错误日志并返回 nil。
// 3. 调试日志: 记录重排模型返回的前 5 名结果（包括排名、分数、分片ID、匹配类型等），便于在线上环境追踪模型表现。
// 4. 结果过滤 (Filtering):
//		如果候选片段是 "历史记录" 类型 (MatchTypeHistory)，则将判定阈值 (th) 适当降低 (th - 0.1)，但最低不低于 0.5，以放宽历史记录的准入条件。
//
//	 	历史对话是用户之前聊过的内容，即使相关性稍低，也很可能是有用的上下文，而重排模型往往“看不起”聊天记录，给出较低的分数。
// 		降低过滤门槛可以让更多历史信息进入后续处理，避免"说过的话被过滤掉"的体验问题，因为用户可能正在围绕这份文档进行连续提问。
//  6. 返回过滤后的重排序结果列表
//
// 参数:
//   - ctx: 上下文环境，用于日志记录
//   - chatManage: 聊天管理对象，包含会话配置（如重排过滤阈值）
//   - rerankModel: 重排模型接口，用于计算相关性分数
//   - query: 查询字符串（通常为 RewriteQuery）
//   - passages: 待重排的候选文档列表（文本内容，包含 OCR/描述等增强信息）
//   - candidates: 候选搜索结果列表，与 passages 一一对应，用于在过滤阶段校验候选文档的类型（如 MatchType）
//
// 返回值:
//   - []rerank.RankResult: 过滤后的重排序结果，按相关性分数降序排列；
//   - 若模型调用失败，返回 nil

// rerank performs the actual reranking operation with given query and passages
func (p *PluginRerank) rerank(ctx context.Context,
	chatManage *types.ChatManage,
	rerankModel rerank.Reranker,
	query string,
	passages []string,
	candidates []*types.SearchResult,
) []rerank.RankResult {
	pipelineInfo(ctx, "Rerank", "model_call", map[string]interface{}{
		"query_variant": query,
		"passages":      len(passages),
	})
	rerankResp, err := rerankModel.Rerank(ctx, query, passages)
	if err != nil {
		pipelineError(ctx, "Rerank", "model_call", map[string]interface{}{
			"query_variant": query,
			"error":         err.Error(),
		})
		return nil
	}

	// Log top scores for debugging
	pipelineInfo(ctx, "Rerank", "threshold", map[string]interface{}{
		"threshold": chatManage.RerankThreshold,
	})
	for i := range min(5, len(rerankResp)) {
		if rerankResp[i].Index < len(candidates) {
			pipelineInfo(ctx, "Rerank", "top_score", map[string]interface{}{
				"rank":       i + 1,
				"score":      rerankResp[i].RelevanceScore,
				"chunk_id":   candidates[rerankResp[i].Index].ID,
				"match_type": candidates[rerankResp[i].Index].MatchType,
				"chunk_type": candidates[rerankResp[i].Index].ChunkType,
				"content":    candidates[rerankResp[i].Index].Content,
			})
		}
	}

	// Filter results based on threshold with special handling for history matches
	rankFilter := []rerank.RankResult{}
	for _, result := range rerankResp {
		if result.Index >= len(candidates) {
			continue
		}
		th := chatManage.RerankThreshold
		matchType := candidates[result.Index].MatchType
		if matchType == types.MatchTypeHistory {
			th = math.Max(th-0.1, 0.5) // Lower threshold for history matches
		}
		if result.RelevanceScore > th {
			rankFilter = append(rankFilter, result)
		}
	}
	return rankFilter
}

// ensureMetadata ensures the metadata is not nil
func ensureMetadata(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

// compositeScore 计算搜索结果的综合加权得分
//
// 多因子加权算法评估文档片段的相关性，旨在平衡模型语义判断、原始检索质量、来源权威性及文档结构特征。
//
// 1. 来源可信度惩罚 (Source Trust Penalty):
//    - 针对 'web_search' (网络搜索) 来源施加 5% 的分数惩罚 (权重 0.95)。
//    - 目的：在同等相关性下，优先保留准确性更高的私有知识库内容，降低外部噪声干扰。
//
// 2. 位置先验 (Position Prior):
//    根据片段在原文档中的起始位置（StartAt）计算位置系数。
//    - 逻辑：文档开头的内容通常包含更多核心信息，因此给予轻微的正向增益（最高 +5%）。
//    - 计算：positionPrior = 1.0 + Clamp(1.0 - StartAt / (EndAt + 1), -0.05, 0.05)
//
// 3. 核心加权 (Core Weighting):
//    - 模型得分 (60%): 重排模型计算的语义相关性得分，占据主导地位。
//    - 基础得分 (30%): 初始检索阶段的原始相关性得分，保留向量/关键词检索的先验信息。
//    - 来源权重 (10%): 基于知识来源的可信度系数（例如：网络搜索降权至 0.95，内部知识库保持 1.0）。
//    - 公式: core = 0.6*modelScore + 0.3*baseScore + 0.1*sourceWeight
//
// 4. 边界归一化:
//    - 强制将结果截断在 [0, 1] 区间内，确保分数的有效性和一致性。
//
// 参数:
//   - sr: 搜索结果对象，包含来源类型（KnowledgeSource）和位置信息（StartAt/EndAt）。
//   - modelScore: 重排模型输出的语义相关性得分（0-1）。
//   - baseScore: 初始检索阶段的原始得分（0-1）。
//
// 返回值:
//   - float64: 经过多维度修正后的最终综合得分（0-1）。

// compositeScore calculates the composite score for a search result
func compositeScore(sr *types.SearchResult, modelScore, baseScore float64) float64 {
	sourceWeight := 1.0
	switch strings.ToLower(sr.KnowledgeSource) {
	case "web_search":
		sourceWeight = 0.95
	default:
		sourceWeight = 1.0
	}
	positionPrior := 1.0
	if sr.StartAt >= 0 {
		positionPrior += searchutil.ClampFloat(1.0-float64(sr.StartAt)/float64(sr.EndAt+1), -0.05, 0.05)
	}
	composite := 0.6*modelScore + 0.3*baseScore + 0.1*sourceWeight
	composite *= positionPrior
	if composite < 0 {
		composite = 0
	}
	if composite > 1 {
		composite = 1
	}
	return composite
}

// applyMMR 对搜索结果应用 MMR 算法进行多样性重排序
//
// 该算法不仅仅关注文档与查询的相关性，还通过惩罚与已选文档高度相似的候选文档，确保最终结果集在保持高相关性的同时，覆盖尽可能多的不同信息维度。
// 它在结果相关性与多样性之间取得平衡，通过预计算词元集合优化冗余度计算性能，最终筛选出指定数量的高质量低冗余结果子集。
//
// 执行过程:
//  1. 参数校验：若 k <= 0 或结果集为空，直接返回 nil
//  2. 预计算优化：对所有结果提取富文本片段并进行简单分词，生成词元集合缓存
//  3. 迭代选择：循环执行至多 k 轮，每轮遍历未选结果计算 MMR 分数
//     - 相关性：直接使用结果原始评分 r.Score
//     - 冗余度：计算当前结果与所有已选结果的 Jaccard 相似度最大值
//     - MMR 分数：lambda*relevance - (1-lambda)*redundancy
//     - 选取 MMR 分数最高的结果加入选中集合
//  4. 终止条件：选满 k 个结果或遍历完所有候选
//  5. 统计输出：计算已选结果间的平均冗余度，记录算法执行摘要
//
// 核心算法逻辑 (MMR Formula):
//
// 对于每一个候选文档，计算其 MMR 得分：
//    MMR = λ * Relevance - (1-λ) * Redundancy
// 其中：
// - Relevance (相关性): 文档的原始得分 (r.Score)，代表与查询的匹配程度。
// - Redundancy (冗余度): 该文档与当前已选文档集中最相似文档的 Jaccard 相似度。
// - Lambda (λ): 平衡因子。
//    - λ 越接近 1：侧重于相关性，忽略重复（类似普通排序）。
//    - λ 越接近 0：侧重于多样性，极力避免重复（可能牺牲相关性）。
//
// 执行流程 (Execution Flow):
// 1. 预处理 (Pre-computation):
//    - 对所有候选结果进行分词并构建 Token 集合。
//    - 优化：预先计算所有集合避免了在循环中重复分词，显著提升性能。
//
// 2. 贪心选择 (Greedy Selection):
//    - 初始化一个空的“已选集合”。
//    - 循环执行，直到选满 k 个结果或没有更多候选者：
//      a. 遍历所有未选文档。
//      b. 计算当前文档与“已选集合”中所有文档的最大 Jaccard 相似度（即最大冗余度）。
//      c. 根据 MMR 公式计算得分。
//      d. 选出 MMR 得分最高的文档加入“已选集合”。
//
// 3. 监控与统计 (Monitoring):
//    - 计算最终选中结果集内部的平均冗余度，用于评估去重效果。
//
// 参数:
//   - ctx: 上下文环境，用于日志记录。
//   - results: 待处理的候选搜索结果列表，按相关性降序排列。
//   - chatManage: 聊天管理对象，包含会话级配置。
//   - k: 期望返回的结果条数 (Top-K)。
//   - lambda: 相关性权重系数，范围 [0.0, 1.0]；越接近 1 越侧重相关性，越接近 0 越侧重多样性。
//
// 返回值:
//   - []*types.SearchResult: MMR 算法选出的结果子集，长度不超过 k；这些结果在保持相关性的同时，彼此之间的内容重复度最低。若参数非法则返回 nil

// applyMMR applies the MMR algorithm to the search results with pre-computed token sets
func applyMMR(
	ctx context.Context,
	results []*types.SearchResult,
	chatManage *types.ChatManage,
	k int,
	lambda float64,
) []*types.SearchResult {
	if k <= 0 || len(results) == 0 {
		return nil
	}
	pipelineInfo(ctx, "Rerank", "mmr_start", map[string]interface{}{
		"lambda":     lambda,
		"k":          k,
		"candidates": len(results),
	})

	// Pre-compute all token sets upfront (optimization)
	allTokenSets := make([]map[string]struct{}, len(results))
	for i, r := range results {
		allTokenSets[i] = searchutil.TokenizeSimple(getEnrichedPassage(ctx, r))
	}

	selected := make([]*types.SearchResult, 0, k)
	selectedTokenSets := make([]map[string]struct{}, 0, k)
	selectedIndices := make(map[int]struct{})

	for len(selected) < k && len(selectedIndices) < len(results) {
		bestIdx := -1
		bestScore := -1.0

		for i, r := range results {
			if _, isSelected := selectedIndices[i]; isSelected {
				continue
			}

			relevance := r.Score
			redundancy := 0.0

			// Use pre-computed token sets for redundancy calculation
			for _, selTokens := range selectedTokenSets {
				sim := searchutil.Jaccard(allTokenSets[i], selTokens)
				if sim > redundancy {
					redundancy = sim
				}
			}

			mmr := lambda*relevance - (1.0-lambda)*redundancy
			if mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			break
		}

		selected = append(selected, results[bestIdx])
		selectedTokenSets = append(selectedTokenSets, allTokenSets[bestIdx])
		selectedIndices[bestIdx] = struct{}{}
	}

	// Compute average redundancy among selected using pre-computed token sets
	avgRed := 0.0
	if len(selected) > 1 {
		pairs := 0
		for i := 0; i < len(selectedTokenSets); i++ {
			for j := i + 1; j < len(selectedTokenSets); j++ {
				avgRed += searchutil.Jaccard(selectedTokenSets[i], selectedTokenSets[j])
				pairs++
			}
		}
		if pairs > 0 {
			avgRed /= float64(pairs)
		}
	}
	pipelineInfo(ctx, "Rerank", "mmr_done", map[string]interface{}{
		"selected":       len(selected),
		"avg_redundancy": fmt.Sprintf("%.4f", avgRed),
	})
	return selected
}

// getEnrichedPassage 构建包含多模态信息与元数据的增强型文本片段。
//
// 该函数对搜索结果进行多源信息融合，将原始文本内容与图片描述、OCR文本、生成问题等辅助信息整合为单一富文本，用于提升重排序模型的语义理解能力。
// 它不仅返回原始文本内容，还会深度解析并提取隐藏在 JSON 字段中的关键信息（如图片描述、OCR 识别文本、潜在相关问题），
// 将其拼接为统一的文本流，以确保模型在计算相关性时不会遗漏非文本或元数据特征，为重排模型提供更全面的上下文信息。
//
// 执行过程:
//  1. 初始化：以原始 Content 作为基础文本
//  2. 解析 ImageInfo 字段：
//     - 反序列化 JSON 图片信息数组
//     - 提取每张图片的 Caption（描述）和 OCRText（OCR文本）
//     - 分别按固定格式追加至增强信息列表
//     - 解析失败时记录警告日志，不影响后续处理
//  3. 解析 ChunkMetadata 字段：
//     - 反序列化为文档片段元数据结构
//     - 调用 GetQuestionStrings 提取生成问题列表
//     - 按固定格式追加至增强信息列表
//     - 解析失败时记录警告日志，不影响后续处理
//  4. 拼接：若存在增强信息，以双换行符分隔追加至基础文本
//  5. 返回：记录富文本生成摘要日志，返回合并后的完整文本
//
// 参数:
//   - ctx: 上下文环境，用于日志记录。
//   - result: 原始搜索结果对象，包含 Content, ImageInfo 和 ChunkMetadata 字段。
//
// 返回值:
//   - string: 融合后的富文本内容，可直接作为重排模型的输入 Passage ；若无需增强则返回原始 Content

// getEnrichedPassage 合并Content、ImageInfo和GeneratedQuestions的文本内容
func getEnrichedPassage(ctx context.Context, result *types.SearchResult) string {
	combinedText := result.Content
	var enrichments []string

	// 解析ImageInfo
	if result.ImageInfo != "" {
		var imageInfos []types.ImageInfo
		err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos)
		if err != nil {
			pipelineWarn(ctx, "Rerank", "image_info_parse", map[string]interface{}{
				"error": err.Error(),
			})
		} else {
			// 提取所有图片的描述和OCR文本
			for _, img := range imageInfos {
				if img.Caption != "" {
					enrichments = append(enrichments, fmt.Sprintf("图片描述: %s", img.Caption))
				}
				if img.OCRText != "" {
					enrichments = append(enrichments, fmt.Sprintf("图片文本: %s", img.OCRText))
				}
			}
		}
	}

	// 解析ChunkMetadata中的GeneratedQuestions
	if len(result.ChunkMetadata) > 0 {
		var docMeta types.DocumentChunkMetadata
		err := json.Unmarshal(result.ChunkMetadata, &docMeta)
		if err != nil {
			pipelineWarn(ctx, "Rerank", "chunk_metadata_parse", map[string]interface{}{
				"error": err.Error(),
			})
		} else if questionStrings := docMeta.GetQuestionStrings(); len(questionStrings) > 0 {
			enrichments = append(enrichments, fmt.Sprintf("相关问题: %s", strings.Join(questionStrings, "; ")))
		}
	}

	if len(enrichments) == 0 {
		return combinedText
	}

	// 组合内容和增强信息
	if combinedText != "" {
		combinedText += "\n\n"
	}
	combinedText += strings.Join(enrichments, "\n")

	pipelineInfo(ctx, "Rerank", "passage_enrich", map[string]interface{}{
		"content_len":    len(result.Content),
		"enrichment":     strings.Join(enrichments, "\n"),
		"enrichment_len": len(strings.Join(enrichments, "\n")),
	})

	return combinedText
}

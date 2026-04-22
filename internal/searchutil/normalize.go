package searchutil

import "sort"

// KeywordScoreCallbacks allows callers to hook into normalization telemetry.
type KeywordScoreCallbacks struct {
	OnNoVariance func(count int, score float64)
	OnNormalized func(count int, rawMin, rawMax, normalizeMin, normalizeMax float64)
}

// NormalizeKeywordScores 使用稳健的百分位数边界算法，对关键词匹配结果进行原地（In-place）分数归一化。
//
// 该函数的核心逻辑如下：
// 1. 筛选目标：根据 `isKeyword` 过滤出需要处理的关键词结果。
// 2. 边界计算：
//     - 如果结果数量少于 10 个，直接使用原始数据的最大值和最小值作为边界。
//     - 如果结果数量达到或超过 10 个，则采用 P5（5%分位数）和 P95（95%分位数） 作为归一化的最小和最大边界。
//    	 这能有效剔除极低分和极高分的离群值影响，避免分数被极端值压缩。
// 3. 线性映射：将分数映射到 [0, 1] 区间。超出边界的分数会被截断（Clamp）到边界值。
// 4. 特殊情况处理：如果所有分数都相同（方差为 0）或归一化范围无效，则统一将分数设为 1.0。
//
// 参数：
//   results     - 结果切片（会被原地修改）
//   isKeyword   - 判断元素是否为关键字匹配的函数
//   getScore    - 从元素中获取原始分数的函数
//   setScore    - 将归一化后的分数设置回元素的函数
//   callbacks   - 回调函数集合（可以为空，不传则忽略回调），用于接收监控数据（如无方差或归一化边界信息）。
//
// 归一化策略：
//   - 0个关键字：直接返回，不做任何处理
//   - 1个关键字：直接设置为 1.0（唯一结果，最高相关性）
//   - 2-9个关键字：使用最小/最大值作为边界进行线性归一化
//   - 10个及以上：使用第5百分位数和第95百分位数作为边界
//   - 无方差（所有分数相同）：所有结果设置为 1.0
//   - 边界折叠（百分位数相等）：回退到所有结果设置为 1.0
//
//
// 线性归一化公式：
//   normalized = (clamped(score, min, max) - min) / (max - min)
//   结果范围：[0, 1]
//
// 百分位数边界的好处：
//   - 排除极端异常值的影响（如某些文档分数异常高或低）
//   - 提高归一化的稳定性
//   - 避免大多数结果被压缩到接近0或1的极端值
//
// 注意事项：
//   - 函数会原地修改 results 中的分数，不创建新切片
//   - 只对 isKeyword 返回 true 的元素进行归一化，非关键字匹配的元素分数保持不变
//   - 归一化后的分数范围始终在 [0, 1] 之间
//   - 使用百分位数时需要至少10个关键字结果
//   - 百分位数计算采用下取整方式（p5Idx = len * 5 / 100）
//
// 示例：
//   type SearchResult struct {
//       Title   string
//       IsMatch bool
//       Score   float64
//   }
//
//   results := []SearchResult{
//       {Title: "doc1", IsMatch: true, Score: 100},
//       {Title: "doc2", IsMatch: true, Score: 80},
//       {Title: "doc3", IsMatch: true, Score: 30},
//       {Title: "doc4", IsMatch: false, Score: 0},
//   }
//
//   NormalizeKeywordScores(
//       results,
//       func(r SearchResult) bool { return r.IsMatch },
//       func(r SearchResult) float64 { return r.Score },
//       func(r SearchResult, score float64) { r.Score = score },
//       KeywordScoreCallbacks{
//           OnNormalized: func(count int, rawMin, rawMax, normMin, normMax float64) {
//               log.Printf("归一化完成: 原始范围[%.2f,%.2f] -> 归一化范围[%.2f,%.2f]",
//                   rawMin, rawMax, normMin, normMax)
//           },
//       },
//   )
//
//   // 归一化后:
//   // doc1.Score = 1.0   (100 -> (100-30)/(100-30) = 1.0)
//   // doc2.Score = 0.71  (80 -> (80-30)/(100-30) ≈ 0.714)
//   // doc3.Score = 0.0   (30 -> (30-30)/(100-30) = 0.0)
//   // doc4.Score = 0.0   (非关键字，保持不变)

// NormalizeKeywordScores normalizes keyword match scores in-place using robust percentile bounds.
func NormalizeKeywordScores[T any](
	results []T,
	isKeyword func(T) bool,
	getScore func(T) float64,
	setScore func(T, float64),
	callbacks KeywordScoreCallbacks,
) {
	keywordResults := make([]T, 0, len(results))
	for _, result := range results {
		if isKeyword(result) {
			keywordResults = append(keywordResults, result)
		}
	}

	if len(keywordResults) == 0 {
		return
	}

	if len(keywordResults) == 1 {
		setScore(keywordResults[0], 1.0)
		return
	}

	minS := getScore(keywordResults[0])
	maxS := minS
	for _, r := range keywordResults[1:] {
		score := getScore(r)
		if score < minS {
			minS = score
		}
		if score > maxS {
			maxS = score
		}
	}

	if maxS <= minS {
		for _, r := range keywordResults {
			setScore(r, 1.0)
		}
		if callbacks.OnNoVariance != nil {
			callbacks.OnNoVariance(len(keywordResults), minS)
		}
		return
	}

	normalizeMin := minS
	normalizeMax := maxS

	if len(keywordResults) >= 10 {
		scores := make([]float64, len(keywordResults))
		for i, r := range keywordResults {
			scores[i] = getScore(r)
		}
		sort.Float64s(scores)
		p5Idx := len(scores) * 5 / 100
		p95Idx := len(scores) * 95 / 100
		if p5Idx < len(scores) {
			normalizeMin = scores[p5Idx]
		}
		if p95Idx < len(scores) {
			normalizeMax = scores[p95Idx]
		}
	}

	rangeSize := normalizeMax - normalizeMin
	if rangeSize > 0 {
		for _, r := range keywordResults {
			clamped := getScore(r)
			if clamped < normalizeMin {
				clamped = normalizeMin
			} else if clamped > normalizeMax {
				clamped = normalizeMax
			}
			ns := (clamped - normalizeMin) / rangeSize
			if ns < 0 {
				ns = 0
			} else if ns > 1 {
				ns = 1
			}
			setScore(r, ns)
		}
		if callbacks.OnNormalized != nil {
			callbacks.OnNormalized(
				len(keywordResults),
				minS,
				maxS,
				normalizeMin,
				normalizeMax,
			)
		}
		return
	}

	// Fallback when percentile filtering collapses the range.
	for _, r := range keywordResults {
		setScore(r, 1.0)
	}
}

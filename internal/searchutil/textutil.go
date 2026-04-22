package searchutil

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
)

// BuildContentSignature 创建内容的标准化 MD5 签名，用于检测重复内容
//
// 功能说明：
//   1. 去除字符串首尾空白字符
//   2. 将所有字符转换为小写
//   3. 压缩连续的空白字符为单个空格
//   4. 计算 MD5 哈希值
//   5. 返回十六进制编码的哈希字符串
//
// 归一化处理：
//   - "  Hello   World  " → "hello world"
//   - "HELLO   WORLD" → "hello world"
//   - "hello\nworld" → "hello world"
//
// 参数：
//   content - 待生成签名的原始内容
//
// 返回值：
//   string - MD5 签名的十六进制字符串
//            - 如果内容为空字符串或只包含空白字符，返回空字符串
//            - 正常内容返回 32 位十六进制字符串
//
// 注意事项：
//   - MD5 算法存在理论上的哈希碰撞可能，但不影响去重场景使用
//   - 签名基于归一化后的内容，格式差异不会影响去重效果
//   - 对于极大量文档（千万级以上），建议使用 SHA-256 降低碰撞风险
//
// 使用场景：
//   - 文档去重系统
//   - 缓存键生成
//   - 数据库唯一性约束
//   - 相似内容快速匹配
//
// 示例：
//   sig1 := BuildContentSignature("  Hello   World  ")
//   sig2 := BuildContentSignature("hello world")
//   // sig1 == sig2，两个不同格式的文本生成相同签名

// BuildContentSignature creates a normalized MD5 signature for content to detect duplicates.
// It normalizes the content by lowercasing, trimming whitespace, and collapsing multiple spaces.
func BuildContentSignature(content string) string {
	c := strings.ToLower(strings.TrimSpace(content))
	if c == "" {
		return ""
	}
	// Normalize whitespace
	c = strings.Join(strings.Fields(c), " ")
	// Use MD5 hash of full content
	hash := md5.Sum([]byte(c))
	return hex.EncodeToString(hash[:])
}

// TokenizeSimple 将文本拆分为单词集合（基于空白字符分词）。
//
// 功能说明：
//   1. 将文本转换为小写
//   2. 按空白字符（空格、制表符、换行符等）分割文本
//   3. 忽略长度小于等于 1 的字符（如单字母、标点符号，如 "a", "b" 等），以减少噪音
//   4. 返回一个以单词为键的 map 集合，实现自动去重，方便后续进行高效的交集与并集运算。
//
// 分词规则：
//   - 仅支持空白符分割，不支持中文等无空格语言
//   - 自动去除重复单词
//   - 标点符号会附着在单词上（如 "hello," 会作为整体）
//
// 参数：
//   text - 待分词的原始文本
//
// 返回值：
//   map[string]struct{} - 单词集合，键为小写单词，值为空结构体
//                         - 空文本返回空集合
//                         - 只包含单字符词的文本返回空集合
//
// 注意事项：
//   - 不过滤停用词（如 "the", "and", "of" 等）
//   - 不支持词干提取（"run" 和 "running" 视为不同单词）
//   - 标点符号不会被自动移除
//   - 适合英文文本，中文文本建议使用专业分词器（如 jieba）
//
// 使用场景：
//   - 文档相似度计算
//   - 关键词提取
//   - 文本分类的特征生成
//   - 搜索引擎的索引构建
//
// 示例：
//   text := "The quick brown fox jumps over the lazy dog"
//   tokens := TokenizeSimple(text)
//   // 结果包含: "the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog"
//   // 注意: "the" 虽然出现两次，但集合中只保留一个
//
//   text2 := "a b c hello world"
//   tokens2 := TokenizeSimple(text2)
//   // 结果只包含: "hello", "world"（单字符词 "a", "b", "c" 被过滤）

// TokenizeSimple tokenizes text into a set of words (simple whitespace-based).
// Returns a map where keys are lowercase tokens with length > 1.
func TokenizeSimple(text string) map[string]struct{} {
	text = strings.ToLower(text)
	fields := strings.Fields(text)
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if len(f) > 1 {
			set[f] = struct{}{}
		}
	}
	return set
}

// Jaccard 计算两个 token 集合之间的 Jaccard 相似度系数
//
// 数学定义：
//   J(A,B) = |A ∩ B| / |A ∪ B|
//
// 其中：
//   - |A ∩ B| 表示两个集合的交集大小
//   - |A ∪ B| 表示两个集合的并集大小
//
// 返回值范围：
//   0.0 - 两个集合完全不同（无共同元素）
//   1.0 - 两个集合完全相同（所有元素相同）
//   0-1 之间的值 - 部分相似，值越大相似度越高
//
// 特殊处理：
//   - 如果两个集合都为空，返回 0（避免除零错误）
//   - 自动选择较小的集合作为遍历对象，优化性能
//   - 使用递归调用确保第一个参数始终是较小的集合
//
// 参数：
//   a - 第一个 token 集合（map[string]struct{} 类型）
//   b - 第二个 token 集合（map[string]struct{} 类型）
//
// 返回值：
//   float64 - Jaccard 相似度系数，范围 [0.0, 1.0]
//
// 性能：
//   时间复杂度 O(min(|A|, |B|))，空间复杂度 O(1)
//   通过选择较小集合遍历，显著提升计算效率
//
// 注意事项：
//   - 输入参数必须是 map[string]struct{} 类型（由 TokenizeSimple 生成）
//   - 不会修改原始集合，纯函数实现
//   - 结果可能因浮点数精度问题略有偏差
//
// 使用场景：
//   - 文档相似度比较
//   - 推荐系统的物品相似度
//   - 文本聚类算法
//   - 搜索引擎的结果排序
//   - 抄袭检测系统
//
// 示例：
//   set1 := TokenizeSimple("the cat sleeps")
//   set2 := TokenizeSimple("the dog sleeps")
//   // set1: {"the", "cat", "sleeps"}
//   // set2: {"the", "dog", "sleeps"}
//   // 交集: {"the", "sleeps"} → 2
//   // 并集: {"the", "cat", "sleeps", "dog"} → 4
//   // 相似度 = 2/4 = 0.5
//   similarity := Jaccard(set1, set2) // 0.5
//
//   set3 := TokenizeSimple("hello world")
//   set4 := TokenizeSimple("hello world")
//   similarity = Jaccard(set3, set4) // 1.0
//
//   set5 := TokenizeSimple("apple banana")
//   set6 := TokenizeSimple("orange grape")
//   similarity = Jaccard(set5, set6) // 0.0

// Jaccard calculates Jaccard similarity between two token sets.
// Returns a value between 0 and 1, where 1 means identical sets.
func Jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}

	// small set drives large set
	if len(a) > len(b) {
		return Jaccard(b, a)
	}

	// Calculate intersection
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}

	// Calculate union
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}

	return float64(inter) / float64(union)
}

// ClampFloat 将浮点数限制在指定的数值范围内
//
// 功能说明：
//   1. 如果数值小于最小值，返回最小值
//   2. 如果数值大于最大值，返回最大值
//   3. 否则返回原数值
//
// 参数：
//   v    - 待限制的浮点数
//   minV - 允许的最小值（下限）
//   maxV - 允许的最大值（上限）
//
// 边界处理：
//   - 如果 minV > maxV，函数仍会执行比较逻辑
//   - 建议调用前确保 minV <= maxV，否则结果可能不符合预期
//
// 示例：
//   // 限制相似度到有效范围
//   similarity := 1.5
//   clamped := ClampFloat(similarity, 0, 1) // 返回 1.0
//
//   // 限制百分比
//   percent := -10.5
//   clamped = ClampFloat(percent, 0, 100) // 返回 0
//
//   // 正常范围内的数值
//   value := 42.5
//   clamped = ClampFloat(value, 0, 100) // 返回 42.5
//
//   // 与 Jaccard 配合使用
//   similarity := Jaccard(set1, set2)
//   safeSimilarity := ClampFloat(similarity, 0, 1)

// ClampFloat clamps a float value to the specified range [minV, maxV].
func ClampFloat(v, minV, maxV float64) float64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 应用程序总配置
type Config struct {
	Conversation    *ConversationConfig    `yaml:"conversation"     json:"conversation"`
	Server          *ServerConfig          `yaml:"server"           json:"server"`
	KnowledgeBase   *KnowledgeBaseConfig   `yaml:"knowledge_base"   json:"knowledge_base"`
	Tenant          *TenantConfig          `yaml:"tenant"           json:"tenant"`
	Models          []ModelConfig          `yaml:"models"           json:"models"`
	VectorDatabase  *VectorDatabaseConfig  `yaml:"vector_database"  json:"vector_database"`
	DocReader       *DocReaderConfig       `yaml:"docreader"        json:"docreader"`
	StreamManager   *StreamManagerConfig   `yaml:"stream_manager"   json:"stream_manager"`
	ExtractManager  *ExtractManagerConfig  `yaml:"extract"          json:"extract"`
	WebSearch       *WebSearchConfig       `yaml:"web_search"       json:"web_search"`
	PromptTemplates *PromptTemplatesConfig `yaml:"prompt_templates" json:"prompt_templates"`
}

type DocReaderConfig struct {
	Addr string `yaml:"addr" json:"addr"`
}

type VectorDatabaseConfig struct {
	Driver string `yaml:"driver" json:"driver"`
}

// ConversationConfig 对话服务配置
//
// 检索精度控制：决定了系统从知识库中“捞取”资料的范围和门槛。
//   - MaxRounds: 允许 AI 记住多少轮历史对话，防止上下文过长导致模型分心。
//   - KeywordThreshold: 关键词搜索（BM25）的及格线，低于此分的文档片段不采用。
//   - EmbeddingTopK: 第一轮向量检索找回的片段数量（粗排）。
//   - VectorThreshold: 向量检索的相似度及格线（通常在 0.5-0.8 之间）。
//   - RerankTopK: 经过 BERT 等模型重新打分后，最终喂给 AI 的片段数量（精排）。
//   - RerankThreshold: 精排的及格线，只有真正高度相关的资料才会传给大模型。
//   - 示例：如果 RerankTopK 是 3，AI 即使找到了 10 条相关信息，也只会挑最靠谱的 3 条来组织答案。
//
// 失败处理机制：数据库里查不到相关资料时，系统该怎么办。
//   - FallbackStrategy: 策略选择（如 "model" 表示让模型用自带知识回答，"response" 表示回复固定内容）。
//   - FallbackResponse: 固定回复语。如：“抱歉，在知识库中未找到相关信息。”
//   - FallbackPrompt: 当进入兜底模式时，引导 AI 说话的指令。
//   - 示例：当用户问一个知识库没有的问题时，系统根据 FallbackResponse 直接回一句客套话，而不是让 AI 乱编。
//
// 问题优化模块：在搜索之前，先通过 LLM 对用户的问题进行“预加工”。
//   - EnableRewrite / RewritePrompt ... : 指代消解。
//     -- 用户问： “iPhone 16 多少钱？” -> “它好用吗？”
//     -- 改写后： “iPhone 16 好用吗？”（防止数据库搜“它”搜不到东西）。
//   - EnableQueryExpansion: 语义扩展。
//     -- 用户问： “感冒怎么办？”
//     -- 扩展后： “感冒症状、流感治疗、发烧怎么退烧”。
//   - SimplifyQueryPrompt...: 长句精简。
//     -- 用户问： “那个，麻烦问一下浦东机场附近稍微便宜点环境还行的酒店推荐下谢谢。”
//     -- 精简后： “浦东机场 经济型酒店 评价好”。
//
// 辅助生成功能：用于丰富用户体验和管理后台数据。
//   - Summary: 最终生成回答时的模型参数（如随机度、最大字数）。
//   - GenerateSessionTitlePrompt: 为当前的对话生成一个标题。
//   - GenerateSummaryPrompt: 对长篇对话或文档生成摘要。
//   - GenerateQuestionsPrompt: 反向提问。在文档存入数据库之前，让 AI 先读一遍文档，并根据内容 “反向” 出几道题，用来提升检索召回率。
//
// 知识结构化提取：将乱糟糟的对话变成有规律的数据。
//   - ExtractEntitiesPrompt: 提取实体（人名、地名、产品名）。
//   - ExtractRelationshipsPrompt: 提取关系（如：张三 入职 某公司）。
type ConversationConfig struct {
	MaxRounds                  int            `yaml:"max_rounds"                    json:"max_rounds"`
	KeywordThreshold           float64        `yaml:"keyword_threshold"             json:"keyword_threshold"`
	EmbeddingTopK              int            `yaml:"embedding_top_k"               json:"embedding_top_k"`
	VectorThreshold            float64        `yaml:"vector_threshold"              json:"vector_threshold"`
	RerankTopK                 int            `yaml:"rerank_top_k"                  json:"rerank_top_k"`
	RerankThreshold            float64        `yaml:"rerank_threshold"              json:"rerank_threshold"`
	FallbackStrategy           string         `yaml:"fallback_strategy"             json:"fallback_strategy"`
	FallbackResponse           string         `yaml:"fallback_response"             json:"fallback_response"`
	FallbackPrompt             string         `yaml:"fallback_prompt"               json:"fallback_prompt"`
	EnableRewrite              bool           `yaml:"enable_rewrite"                json:"enable_rewrite"`
	EnableQueryExpansion       bool           `yaml:"enable_query_expansion"        json:"enable_query_expansion"`
	EnableRerank               bool           `yaml:"enable_rerank"                 json:"enable_rerank"`
	Summary                    *SummaryConfig `yaml:"summary"                       json:"summary"`
	GenerateSessionTitlePrompt string         `yaml:"generate_session_title_prompt" json:"generate_session_title_prompt"`
	GenerateSummaryPrompt      string         `yaml:"generate_summary_prompt"       json:"generate_summary_prompt"`
	RewritePromptSystem        string         `yaml:"rewrite_prompt_system"         json:"rewrite_prompt_system"`
	RewritePromptUser          string         `yaml:"rewrite_prompt_user"           json:"rewrite_prompt_user"`
	SimplifyQueryPrompt        string         `yaml:"simplify_query_prompt"         json:"simplify_query_prompt"`
	SimplifyQueryPromptUser    string         `yaml:"simplify_query_prompt_user"    json:"simplify_query_prompt_user"`
	ExtractEntitiesPrompt      string         `yaml:"extract_entities_prompt"       json:"extract_entities_prompt"`
	ExtractRelationshipsPrompt string         `yaml:"extract_relationships_prompt"  json:"extract_relationships_prompt"`
	// GenerateQuestionsPrompt is used to generate questions for document chunks to improve recall
	GenerateQuestionsPrompt string `yaml:"generate_questions_prompt" json:"generate_questions_prompt"`
}

// SummaryConfig 摘要配置
//
// 1. 采样与随机性控制（决定 AI 的“性格”）
//
//	Temperature (温度)
//	- 解释：控制输出的随机性。
//	- 数值：0 到 2 之间。
//	 -- 低温度（如 0.2）：让回答更严谨、确定，适合知识库问答；
//	 -- 高温度（如 0.8）：让回答更有创意，但容易胡编乱造。
//
//	TopP (核采样)
//	- 解释：在累积概率达到 P 的词池中采样。
//	- 作用：设置为 0.9 表示只在占总概率 90% 的词中选。它比 Temperature 更柔和地保证了输出质量。
//
//	TopK
//	- 解释：在概率最高的前 K 个词中采样。
//	- 作用：限制模型只看最可能的几个词，防止跑题。
//
//	Seed (种子)
//	- 解释：随机数种子。
//	- 作用：固定种子可以让模型在输入相同的情况下，输出结果尽量一致，方便调试（Debug）

// 2. 惩罚机制（防止“复读机”）
//	RepeatPenalty (重复惩罚)
//	- 解释：对已经出现过的词进行整体惩罚。
//	- 作用：防止模型陷入无限循环（比如一直重复一句话）。通常设为 1.1 左右。
//
//	FrequencyPenalty (频率惩罚)
//	- 解释：根据词语出现的次数累积惩罚。
//	- 作用：降低高频词出现的概率，让语言更丰富。
//
//	PresencePenalty (存在惩罚)
//	- 解释：只要词语出现过就惩罚，不管次数。
//	- 作用：鼓励模型谈论新话题，增加输出的信息量。
//
// 3. 长度与指令控制（决定 AI 的“尺度”）
//	MaxTokens
//	- 解释：输入 + 输出的总 Token 限制。
//	- 作用：防止上下文过长导致模型崩溃或成本过高。
//
//	MaxCompletionTokens
//	- 解释：只针对输出内容的最大长度限制。
//	- 作用：防止 AI 变成“话痨”，强制它在规定长度内说完。
//
//	Prompt (系统指令)
//	- 解释：这是给 AI 的总纲。
//	- 作用：定义角色（如：“你是个资深医生”）和行为准则（如：“必须严格依据事实”）。
//
//	ContextTemplate (上下文模板)
//	- 解释：定义如何组装检索到的资料。
//	- 示例："已知信息：{{context}}。用户问题：{{query}}。请回答："。
//
// 4. RAG 业务高级逻辑（决定 AI 的“策略”）
//	NoMatchPrefix (未命中前缀)
//	- 解释：当 AI 发现检索到的资料里没答案时，强制要求的开头语。
//	- 作用：比如设为 [NO_MATCH]。这样程序后端一看到这两个字，就知道“检索失败”了，可以触发发短信或转人工的逻辑。
//
//	Thinking (思维链开关)
//	- 解释：是否启用推理过程（类似 DeepSeek 的推理模式）。
//	- 作用：开启后，模型会先输出一段 <think>...</think> 内容，理清思路后再给答案，这能显著提高复杂问题的准确率。

type SummaryConfig struct {
	MaxTokens           int     `yaml:"max_tokens"            json:"max_tokens"`
	RepeatPenalty       float64 `yaml:"repeat_penalty"        json:"repeat_penalty"`
	TopK                int     `yaml:"top_k"                 json:"top_k"`
	TopP                float64 `yaml:"top_p"                 json:"top_p"`
	FrequencyPenalty    float64 `yaml:"frequency_penalty"     json:"frequency_penalty"`
	PresencePenalty     float64 `yaml:"presence_penalty"      json:"presence_penalty"`
	Prompt              string  `yaml:"prompt"                json:"prompt"`
	ContextTemplate     string  `yaml:"context_template"      json:"context_template"`
	Temperature         float64 `yaml:"temperature"           json:"temperature"`
	Seed                int     `yaml:"seed"                  json:"seed"`
	MaxCompletionTokens int     `yaml:"max_completion_tokens" json:"max_completion_tokens"`
	NoMatchPrefix       string  `yaml:"no_match_prefix"       json:"no_match_prefix"`
	Thinking            *bool   `yaml:"thinking"              json:"thinking"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port            int           `yaml:"port"             json:"port"`
	Host            string        `yaml:"host"             json:"host"`
	LogPath         string        `yaml:"log_path"         json:"log_path"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout" default:"30s"`
}

// KnowledgeBaseConfig 知识库配置
//
// 用于将长文档（如 PDF、Markdown、TXT）切分成适合大模型处理的小片段，以便存入向量数据库。
//
// 1. 文档切片参数
// 	ChunkSize (int)
//	- 含义：每个文本片段（Chunk）的最大长度。
//	- 单位：通常是字符数（characters）或 Token 数（取决于具体的切片器实现）。
//	- 作用：控制单个片段的大小。
//		-- 设置过小：可能导致上下文断裂，模型无法理解完整语义。
//		-- 设置过大：可能超出模型的上下文窗口限制，或者包含过多无关噪音，影响检索精度。
//		-- 常见值：512, 1024, 2048 等。
//
//	ChunkOverlap (int)
//	- 含义：相邻两个片段之间重叠的长度。
//	- 作用：保持上下文的连贯性。
//	- 如果不重叠（为 0），切分点处的语义可能会丢失（例如一个句子被切断），通过重叠，确保切分点附近的信息在两个片段中都存在，提高检索召回率。
//	- 常见值：通常是 ChunkSize 的 10% - 20%，例如 ChunkSize 为 1000 时，Overlap 设为 100 或 200。
//
//	SplitMarkers ([]string)
//	- 含义：用于分割文本的分隔符列表。
//	- 作用：定义切片的优先级边界。系统通常会尝试按顺序使用这些分隔符进行切分，以确保切片尽量在自然的语义边界（如段落、句子）处断开，而不是强行截断单词或句子。
//	- 示例：["\n\n", "\n", "。", "！", "？", " "]（优先按双换行符切分，其次是单换行，最后是标点符号）。
//
//	KeepSeparator (bool)
//	- 含义：是否在切分后的片段中保留分隔符。
//	- 作用：
//	   -- true：保留分隔符（如换行符 \n 或 Markdown 标题标记 #）。这有助于模型识别文档结构（如知道哪里是新段落或新标题）。
//     -- false：丢弃分隔符，只保留纯文本内容。这可以减少 Token 消耗，但可能丢失部分结构信息。
//
// 2. 多媒体处理配置
//	ImageProcessing (*ImageProcessingConfig)
//	- 含义：指向另一个结构体 ImageProcessingConfig 的指针。
//	- 作用：当知识库中包含图片（或在 PDF/Markdown 中嵌入图片）时，定义如何处理这些图片。
//	- 可能包含的子配置（推测）：
//	  -- 是否启用 OCR（光学字符识别）提取图片中的文字。
//	  -- 是否使用多模态模型（Vision Model）生成图片的描述（Caption）。
//	  -- 图片的压缩或分辨率调整策略。
//	  -- 如果为 nil，则表示不处理图片或跳过包含图片的文档。

type KnowledgeBaseConfig struct {
	ChunkSize       int                    `yaml:"chunk_size"       json:"chunk_size"`
	ChunkOverlap    int                    `yaml:"chunk_overlap"    json:"chunk_overlap"`
	SplitMarkers    []string               `yaml:"split_markers"    json:"split_markers"`
	KeepSeparator   bool                   `yaml:"keep_separator"   json:"keep_separator"`
	ImageProcessing *ImageProcessingConfig `yaml:"image_processing" json:"image_processing"`
}

// ImageProcessingConfig 图像处理配置
type ImageProcessingConfig struct {
	EnableMultimodal bool `yaml:"enable_multimodal" json:"enable_multimodal"`
}

// TenantConfig 租户配置
//
// 用于配置多租户系统（Multi-tenant System）中关于会话（Session）默认行为和跨租户访问权限的策略。
// 在多租户架构中，不同的组织（租户）拥有隔离的数据和资源，此配置允许每个租户自定义其用户的初始体验和安全边界。

type TenantConfig struct {
	DefaultSessionName        string `yaml:"default_session_name"        json:"default_session_name"`
	DefaultSessionTitle       string `yaml:"default_session_title"       json:"default_session_title"`
	DefaultSessionDescription string `yaml:"default_session_description" json:"default_session_description"`
	// EnableCrossTenantAccess enables cross-tenant access for users with permission
	EnableCrossTenantAccess bool `yaml:"enable_cross_tenant_access" json:"enable_cross_tenant_access"`
}

// PromptTemplate 提示词模板
//
//
// HasKnowledgeBase (bool)
//	含义：标记该模板是否需要使用私有知识库。
//	逻辑：
//	 - true：系统在发送请求给大模型前，会先根据用户问题在向量数据库中检索相关文档片段，并将这些片段注入到 Content 的上下文中。适用于回答公司内部政策、产品文档等特定领域问题。
//	 - false：仅依赖模型自身的训练知识。
//
// HasWebSearch (bool)
//	含义：标记该模板是否需要启用联网搜索能力。
//	逻辑：
//	 - true：系统会调用搜索引擎 API 获取最新的互联网信息，将搜索结果作为上下文补充给模型。适用于查询新闻、股价、最新技术动态等实时性问题。
//   - false：不进行联网搜索。

type PromptTemplate struct {
	ID               string `yaml:"id"                 json:"id"`
	Name             string `yaml:"name"               json:"name"`
	Description      string `yaml:"description"        json:"description"`
	Content          string `yaml:"content"            json:"content"`
	HasKnowledgeBase bool   `yaml:"has_knowledge_base" json:"has_knowledge_base,omitempty"`
	HasWebSearch     bool   `yaml:"has_web_search"     json:"has_web_search,omitempty"`
}

// PromptTemplatesConfig 提示词模板配置
//
// SystemPrompt
//
//	用途: 定义系统的角色设定或全局指令。
//	场景: 这是发送给 LLM 的第一条消息，用于确立 AI 的行为准则、语气、专业领域或限制条件（例如：“你是一个专业的Python助手”或“请只回答与医疗相关的问题”）。
//	特点: 通常在整个会话期间保持不变，或者在会话开始时注入。
//
// ContextTemplate
//
//	用途: 定义如何构建上下文信息。
//	场景:  在多轮对话或检索增强生成（RAG）场景中，需要将历史对话记录、检索到的文档片段或外部知识插入到提示词中。
//		  这个模板定义了这些动态内容如何被格式化并拼接到 Prompt 中。
//	示例: 可能包含 {history} 或 {retrieved_docs} 等占位符。
//
// RewriteSystem 	重写系统的指令
// RewriteUser 		重写用户的模版 	定义具体的重写逻辑（如：“请将以下口语化的描述转为专业的搜索关键词”）。
//
//	用途: 当需要 AI 重写用户问题时，给重写引擎下达的系统级指令，用于查询重写或提示词优化阶段的系统指令和用户输入模板。
//	场景: 在将用户问题发送给主模型之前，先通过一个轻量级模型或专门的流程对问题进行优化。
//	 - RewriteSystem: 指导重写模型如何优化问题（例如：“请将模糊的问题改写为具体的搜索查询”）。
//	 - RewriteUser: 包含原始用户输入，供重写模型处理。
//	目的: 提高后续主任务（如搜索、回答）的准确率，解决指代不清、信息缺失等问题。
//
// Fallback
//
//	用途: 定义降级策略或备用提示词，当主要模板失效、报错或无法匹配意图时，使用此模板兜底。
//	场景:
//	 - 当主提示词模板执行失败（如触发安全过滤、模型报错、输出格式错误）时。
//	 - 当特定类型的请求无法由常规模板处理时。
//	 - 用于提供一个更保守、更通用或专门用于报错处理的提示词，确保系统不会直接崩溃或返回无意义的结果。
//
// 示例：
//
//	system_prompt:
//		- content: "你是一个友善的电商客服助手，擅长处理退换货问题。"
//		  variables: {}
//		- content: "你是一个严肃的技术支持专家，只回答技术故障排查。"
//		  variables: {}
//
//	context_template:
//		- content: "以下是用户的历史对话记录：\n{{history}}\n当前问题：{{query}}"
//
//	rewrite_system:
//		- content: "请分析以下用户输入，提取关键实体并消除歧义，输出改写后的查询语句。"
//
//	fallback:
//		- content: "抱歉，我暂时无法理解您的问题。能否请您换一种方式描述？"
type PromptTemplatesConfig struct {
	SystemPrompt    []PromptTemplate `yaml:"system_prompt"    json:"system_prompt"`
	ContextTemplate []PromptTemplate `yaml:"context_template" json:"context_template"`
	RewriteSystem   []PromptTemplate `yaml:"rewrite_system"   json:"rewrite_system"`
	RewriteUser     []PromptTemplate `yaml:"rewrite_user"     json:"rewrite_user"`
	Fallback        []PromptTemplate `yaml:"fallback"         json:"fallback"`
}

// ModelConfig 模型配置
//
// Type (type)
//
//	含义: 模型类型
//	用途: 用于区分不同种类的模型，以便调用不同的处理流程或适配器（Adapter）。
//	示例值:
//		- "chat": 对话型模型（如 GPT-4, Claude）。
//		- "embedding": 向量化模型（用于 RAG 检索）。
//		- "completion": 文本补全模型（旧式 API）。
//		- "rerank": 重排序模型。
//		- "image": 文生图模型。
//
// Source (source)
//
//	含义: 模型来源
//	用途: 告诉系统去哪里获取这个模型，或者使用哪个 SDK/客户端来连接。
//	示例值:
//		- "openai": 调用 OpenAI API。
//		- "anthropic": 调用 Anthropic API。
//		- "local" / "ollama": 本地部署的模型。
//		- "azure": Azure OpenAI Service。
//		- "huggingface": Hugging Face Inference API。
//		- "vertex_ai": Google Cloud Vertex AI。
//
// ModelName (model_name)
//
//	含义: 模型名
//	用途: 指定要加载或调用的具体版本。这是传递给 API 的最关键参数。
//	示例值:
//		- "gpt-4o"
//		- "claude-3-5-sonnet-20241022"
//		- "llama-3-70b-instruct"
//		- "text-embedding-3-large"
//
// Parameters (parameters)
//
//	含义: 模型参数
//	类型: map[string]interface{}
//	用途: 不同模型的参数名称和类型不同（有的需要 float，有的需要 int，有的需要 bool），使用 interface{} 可以兼容所有情况。
//	常见参数示例:
//		- temperature: 控制输出的随机性 (0.0 - 1.0)。
//		- max_tokens: 限制生成的最大 token 数。
//		- top_p: 核采样概率。
//		- frequency_penalty: 频率惩罚。
//		- stop_sequences: 停止生成的特定字符串。
//		- seed: 随机种子（用于复现结果）。
//
// 举例 1 ：
//
//	type: "chat"
//	source: "openai"
//	model_name: "gpt-4o"
//	parameters:
//		temperature: 0.7
//		max_tokens: 2048
//		top_p: 0.9
//		stop_sequences: ["END"]
//
// 举例 2 ：
//
//	type: "embedding"
//	source: "azure"
//	model_name: "text-embedding-3-large"
//	parameters:
//		dimensions: 1536  # 特定于某些 API 的维度设置
type ModelConfig struct {
	Type       string                 `yaml:"type"       json:"type"`
	Source     string                 `yaml:"source"     json:"source"`
	ModelName  string                 `yaml:"model_name" json:"model_name"`
	Parameters map[string]interface{} `yaml:"parameters" json:"parameters"`
}

// StreamManagerConfig 流管理器配置
//
// 在 LLM 应用中，当模型以“打字机”效果逐字输出内容时，后端需要一种机制来暂存这些碎片化的数据块（Chunks），直到它们被完整组装并发送给前端。
//
// Type (type)
//
//	含义: 指定流状态存储的后端实现类型。
//	用途: 决定系统使用哪种策略来管理流数据。
//	常见值:
//		- "memory": 内存模式。
//			优点: 速度极快，无网络开销。
//			缺点: 数据易失（重启丢失），无法在多实例（分布式）间共享。适合单机部署或开发环境。
//		- "redis": Redis 模式。
//			优点: 支持分布式共享（多个后端实例可以处理同一个用户的流请求），数据持久化（取决于 Redis 配置），适合生产环境和集群部署。
//			缺点: 有网络延迟，依赖外部中间件。
//
// Redis (redis)
//
//	含义: 当 Type 设置为 "redis" 时，具体的 Redis 连接配置。
//	类型: RedisConfig (这是一个自定义的结构体，虽然代码中未展示，但通常包含地址、端口、密码、数据库索引、TLS 设置等)。
//	用途: 提供连接 Redis 所需的所有凭证和参数。如果 Type 是 "memory"，此字段通常会被忽略。
//
// CleanupTimeout (cleanup_timeout)
//
//	含义: 清理超时时间。
//	类型: time.Duration (Go 语言的时间跨度类型)。
//	用途: 防止内存泄漏或 Redis 键堆积。
//		- 如果一个流会话在多长时间（例如 30 秒）内没有活动或被标记为完成，系统将强制清理该会话占用的资源（删除 Redis Key 或释放内存缓冲）。
type StreamManagerConfig struct {
	Type           string        `yaml:"type"            json:"type"`            // 类型: "memory" 或 "redis"
	Redis          RedisConfig   `yaml:"redis"           json:"redis"`           // Redis配置
	CleanupTimeout time.Duration `yaml:"cleanup_timeout" json:"cleanup_timeout"` // 清理超时，单位秒
}

// RedisConfig Redis配置
type RedisConfig struct {
	Address  string        `yaml:"address"  json:"address"`  // Redis地址
	Username string        `yaml:"username" json:"username"` // Redis用户名
	Password string        `yaml:"password" json:"password"` // Redis密码
	DB       int           `yaml:"db"       json:"db"`       // Redis数据库
	Prefix   string        `yaml:"prefix"   json:"prefix"`   // 键前缀
	TTL      time.Duration `yaml:"ttl"      json:"ttl"`      // 过期时间(小时)
}

// ExtractManagerConfig 抽取管理器配置
//
// 定义如何从非结构化的文本（如文章、对话、报告、网页））中提取出结构化数据（如知识图谱的三元组、命名实体、特定字段等）。
type ExtractManagerConfig struct {
	ExtractGraph  *types.PromptTemplateStructured `yaml:"extract_graph"  json:"extract_graph"`
	ExtractEntity *types.PromptTemplateStructured `yaml:"extract_entity" json:"extract_entity"`
	FabriText     *FebriText                      `yaml:"fabri_text"     json:"fabri_text"`
}

type FebriText struct {
	WithTag   string `yaml:"with_tag"    json:"with_tag"`
	WithNoTag string `yaml:"with_no_tag" json:"with_no_tag"`
}

// LoadConfig 从配置文件加载配置
func LoadConfig() (*Config, error) {
	// 设置配置文件名和路径
	viper.SetConfigName("config")         // 配置文件名称(不带扩展名)
	viper.SetConfigType("yaml")           // 配置文件类型
	viper.AddConfigPath(".")              // 当前目录
	viper.AddConfigPath("./config")       // config子目录
	viper.AddConfigPath("$HOME/.appname") // 用户目录
	viper.AddConfigPath("/etc/appname/")  // etc目录

	// 启用环境变量替换
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	// 替换配置中的环境变量引用
	configFileContent, err := os.ReadFile(viper.ConfigFileUsed())
	if err != nil {
		return nil, fmt.Errorf("error reading config file content: %w", err)
	}

	// 替换${ENV_VAR}格式的环境变量引用
	re := regexp.MustCompile(`\${([^}]+)}`)
	result := re.ReplaceAllStringFunc(string(configFileContent), func(match string) string {
		// 提取环境变量名称（去掉${}部分）
		envVar := match[2 : len(match)-1]
		// 获取环境变量值，如果不存在则保持原样
		if value := os.Getenv(envVar); value != "" {
			return value
		}
		return match
	})

	// 使用处理后的配置内容
	viper.ReadConfig(strings.NewReader(result))

	// 解析配置到结构体
	var cfg Config
	if err := viper.Unmarshal(&cfg, func(dc *mapstructure.DecoderConfig) {
		dc.TagName = "yaml"
	}); err != nil {
		return nil, fmt.Errorf("unable to decode config into struct: %w", err)
	}
	fmt.Printf("Using configuration file: %s\n", viper.ConfigFileUsed())

	// 加载提示词模板（从目录或配置文件）
	configDir := filepath.Dir(viper.ConfigFileUsed())
	promptTemplates, err := loadPromptTemplates(configDir)
	if err != nil {
		fmt.Printf("Warning: failed to load prompt templates from directory: %v\n", err)
		// 如果目录加载失败，使用配置文件中的模板（如果有）
	} else if promptTemplates != nil {
		cfg.PromptTemplates = promptTemplates
	}

	return &cfg, nil
}

// promptTemplateFile 用于解析模板文件
type promptTemplateFile struct {
	Templates []PromptTemplate `yaml:"templates"`
}

// loadPromptTemplates 从目录加载提示词模板
func loadPromptTemplates(configDir string) (*PromptTemplatesConfig, error) {
	templatesDir := filepath.Join(configDir, "prompt_templates")

	// 检查目录是否存在
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		return nil, nil // 目录不存在，返回nil让调用者使用配置文件中的模板
	}

	config := &PromptTemplatesConfig{}

	// 定义模板文件映射
	templateFiles := map[string]*[]PromptTemplate{
		"system_prompt.yaml":    &config.SystemPrompt,
		"context_template.yaml": &config.ContextTemplate,
		"rewrite_system.yaml":   &config.RewriteSystem,
		"rewrite_user.yaml":     &config.RewriteUser,
		"fallback.yaml":         &config.Fallback,
	}

	// 加载每个模板文件
	for filename, target := range templateFiles {
		filePath := filepath.Join(templatesDir, filename)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			continue // 文件不存在，跳过
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", filename, err)
		}

		var file promptTemplateFile
		if err := yaml.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", filename, err)
		}

		*target = file.Templates
	}

	return config, nil
}

// WebSearchConfig represents the web search configuration
type WebSearchConfig struct {
	Timeout int `yaml:"timeout" json:"timeout"` // 超时时间（秒）
}

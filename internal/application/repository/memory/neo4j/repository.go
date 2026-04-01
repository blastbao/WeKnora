package neo4j

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// MemoryRepository(memory/neo4j) 和 Neo4jRepository(retriever/neo4j) 的区别
//
// 这两个对象都是操作 Neo4j 图数据库，且都涉及“节点”和“关系”，但它们的设计目标、数据模型、应用场景以及技术实现深度有着本质的区别。
// 简单来说：
//	- 代码 A (MemoryRepository) 是 “人类记忆系统”：侧重于时间线、对话上下文、用户个性化。
//		它模拟人脑的“情景记忆”和“语义记忆”，用于 AI 助手记住用户说过什么、何时说的。
//	- 代码 B (Neo4jRepository) 是 “通用知识图谱引擎”：侧重于大规模数据导入、知识库管理、结构化检索。
//		它像一个图书管理员，负责把海量的文档知识清洗、去重、入库，并提供模糊搜索。
//
// MemoryRepository 采用的是 Episode - Entity - Relation 三层范式，AI长期记忆的标准范式。
//	- Episode (情景节点)：记录具体的对话事件，作为记忆的“时间锚点”。
//	- Entity (实体节点)：从对话中抽取的核心概念，是知识的“沉淀池”。
//	- Relation (关系)：建立实体间的结构化关联，用于逻辑推理。
//
// Neo4jRepository 采用的是传统的 知识图谱 (Graph) 存储范式。
//	- 节点 (Node)：通常代表一个知识实体，如“马云”、“阿里巴巴”，属性包含 name, chunks (来源), attributes。
//	- 关系 (Relationship)：连接节点，如 [:WORKS_FOR]，并带有属性。
//
// MemoryRepository 负责 “短期对话”向“长期记忆”的转化。
// 它记录的是“何时、何事”，是动态的、与用户强相关的记忆。它回答了“我们是怎么知道这件事的？”（时间溯源）。
//
// Neo4jRepository 负责 “长期记忆”向“结构化知识”的沉淀。
// 它存储的是“是什么、与什么相关”，是静态的、可复用的知识。它回答了“我们知道什么？”（知识沉淀）。

// 场景举例
//	场景 1: 用户问 “我上次说想去哪里旅游来着？”
//	使用 memory/neo4j :
//		系统提取关键词“旅游”。
//		调用 FindRelatedEpisodes(user_id, ["旅游"])。
//		Cypher 查找该用户最近提到的包含“旅游”实体的 Episode。
//		返回: “您在上周二（2026-03-18）的对话中提到想去日本和冰岛。”
//		核心价值: 唤起用户的个人历史记忆。
//
//	场景 2: 用户问 “介绍一下特斯拉公司的上下游关系。”
//	使用 retriever/neo4j :
//		系统提取关键词“特斯拉”。
//		调用 SearchNode(namespace, ["特斯拉"])。
//		Cypher 找到“特斯拉”节点，并返回与其相连的“供应商”、“子公司”、“竞争对手”等节点和关系。
//		返回: 一个结构化的图谱数据，展示特斯拉 -> 供应 -> 松下电池，特斯拉 -> 竞争 -> 比亚迪等。
//		核心价值: 提供客观的结构化知识。
//
//
// 在 AI 应用中，通常这两个仓库是共存的。
//	用户对话时，调用 Code A 保存记忆，让用户觉得 AI“记得住事”。
//	后台定时任务读取文档，调用 Code B 构建行业知识图谱，让 AI“懂得多”。
//	在检索阶段 (RAG)，系统可以同时进行：
//	 - 查 Code A 获取用户偏好和历史上下文。
//	 - 查 Code B 获取专业领域知识。
//	 - 将两者合并作为 Prompt 上下文发给 LLM。

type MemoryRepository struct {
	driver neo4j.Driver
}

func NewMemoryRepository(driver neo4j.Driver) interfaces.MemoryRepository {
	return &MemoryRepository{driver: driver}
}

func (r *MemoryRepository) IsAvailable(ctx context.Context) bool {
	return r.driver != nil
}

// Episode - Entity - Relation 三层结构，是2026年AI长期记忆的标准范式。
//	- Episode 解决了“记不住细节”和“时间溯源”的问题。
//	- Entity 解决了“知识碎片化”和“遗忘干扰”的问题。
//	- Relation 解决了“无法推理”和“逻辑断层”的问题。

// SaveEpisode 原子性地保存对话片段及其提取的知识图谱，构建具备“长期记忆”能力的存储结构。
//
// 该函数模仿人类大脑记忆机制，将数据分层存储以解决传统向量检索的遗忘与推理难题：
// 1. 情景记忆 (Episode): 记录具体的对话事件（时间、上下文），作为记忆的“时间锚点”，保留完整历史且互不干扰。
// 2. 语义记忆 (Entity): 抽取并聚合核心概念（如人物、地点），将碎片化信息融合为唯一的知识节点，避免新数据稀释旧事实。
// 3. 逻辑关联 (Relation): 建立实体间的结构化关系，支持跨时间、跨概念的多跳推理（Multi-hop Query），赋予记忆逻辑性。
//
// 通过此结构，系统能从离散的对话中动态演化出连贯的用户模型与世界模型，实现可溯源、抗干扰且具备推理能力的长期记忆。

// 场景举例：
//	用户说：“我昨天去了北京，参观了故宫，那里人很多。对了，故宫是明朝建立的。”
// 系统处理流程：
//	创建 Episode (记录流水账)
//		创建一个节点：(Episode: {id: "uuid-123", summary: "用户去了北京参观故宫...", time: "昨天"})
//		意义：保留了“昨天”这个时间信息和“人很多”这个具体感受。
//	提取 Entity (建立词条)
//		检查是否有 北京 实体？没有则创建 (Entity: 北京, type: 城市)。
//		检查是否有 故宫 实体？没有则创建 (Entity: 故宫, type: 景点)。
//		检查是否有 明朝 实体？没有则创建 (Entity: 明朝, type: 朝代)。
//	关键连接：建立 Episode -> MENTIONS -> Entity 的关系。
//	意义：现在系统知道“昨天的那次对话”提到了这三个概念。以后搜“北京”或“故宫”都能找到这篇日记。
// 构建 Relationship (编织知识网)
//	分析语义，发现“故宫”和“明朝”有关系。
//	建立关系：(Entity: 故宫)-[:FOUNDED_IN {desc: "建立于"}]->(Entity: 明朝)。
//	意义：即使下次用户只问“故宫是哪个朝代建的？”，哪怕没提昨天的对话，系统也能通过实体关系直接回答，因为它已经变成了通用知识。

// SaveEpisode 将对话片段（Episode）、实体（Entities）及实体间关系（Relations）原子性地保存到 Neo4j 图数据库中。
//
// 该函数在一个写事务中按顺序执行以下操作以确保数据一致性：
// 1. 创建或更新 Episode 节点：使用 episode.ID 作为唯一标识，合并节点并更新用户ID、会话ID、摘要及创建时间。
// 2. 处理实体节点：遍历 entities 切片，对每个实体执行 MERGE 操作（若不存在则创建），更新其类型和描述，
//    并建立从当前 Episode 指向该实体的 [:MENTIONS] 关系。
// 3. 处理实体关系：遍历 relations 切片，查找对应的源实体和目标实体，并在它们之间建立或更新 [:RELATED_TO] 关系，
//    同时设置关系的描述和权重。
//
// 如果在任何步骤中发生错误，整个事务将回滚，不会留下部分写入的数据。
// 若保存失败，函数将记录错误日志并返回具体的 error。

func (r *MemoryRepository) SaveEpisode(ctx context.Context, episode *types.Episode, entities []*types.Entity, relations []*types.Relationship) error {
	session := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		// 1. Create Episode Node
		createEpisodeQuery := `
			MERGE (e:Episode {id: $id})
			SET e.user_id = $user_id,
				e.session_id = $session_id,
				e.summary = $summary,
				e.created_at = $created_at
		`
		_, err := tx.Run(ctx, createEpisodeQuery, map[string]interface{}{
			"id":         episode.ID,
			"user_id":    episode.UserID,
			"session_id": episode.SessionID,
			"summary":    episode.Summary,
			"created_at": episode.CreatedAt.Format(time.RFC3339),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create episode: %v", err)
		}

		// 2. Create Entity Nodes and MENTIONS relationships
		for _, entity := range entities {

			// 1. 在图中查找一个标签为 Entity 且 name 属性等于输入值 $name 的节点
			// 2. 无论节点是新建的还是复用的，都强制更新其类型和描述。
			// 3. 将上一步处理好的节点 n 传递到下一行代码。
			// 4. 精确查找当前正在处理的那个 Episode 节点（通过唯一的 $episode_id）
			// 5. 在 Episode (e) 和 Entity (n) 之间建立一条方向为 e -> n 的 MENTIONS 关系。
			createEntityQuery := `
				MERGE (n:Entity {name: $name})
				SET n.type = $type, n.description = $description
				WITH n
				MATCH (e:Episode {id: $episode_id})
				MERGE (e)-[:MENTIONS]->(n)
			`
			_, err := tx.Run(ctx, createEntityQuery, map[string]interface{}{
				"name":        entity.Title,
				"type":        entity.Type,
				"description": entity.Description,
				"episode_id":  episode.ID,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create entity %s: %v", entity.Title, err)
			}
		}

		// 3. Create Relationships between Entities
		for _, rel := range relations {
			// 在图中查找标签为 Entity 且 name 属性等于 $source 的节点。
			// 在图中查找标签为 Entity 且 name 属性等于 $target 的节点。
			// 尝试在 s 和 t 之间寻找一条类型为 RELATED_TO 且 description 属性完全匹配的关系。
			// 无论关系是新建的还是复用的，都强制更新其 weight 属性。
			createRelQuery := `
				MATCH (s:Entity {name: $source})
				MATCH (t:Entity {name: $target})
				MERGE (s)-[r:RELATED_TO {description: $description}]->(t)
				SET r.weight = $weight
			`
			_, err := tx.Run(ctx, createRelQuery, map[string]interface{}{
				"source":      rel.Source,
				"target":      rel.Target,
				"description": rel.Description,
				"weight":      rel.Weight,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create relationship between %s and %s: %v", rel.Source, rel.Target, err)
			}
		}

		return nil, nil
	})

	if err != nil {
		logger.Errorf(ctx, "failed to save episode: %v", err)
		return err
	}

	return nil
}

// 在特定的用户记忆库中，快速定位所有提及了指定关键词的历史对话片段（Episodes），并按时间倒序返回。
// 这是连接“静态知识（Entity）”与“动态历史（Episode）”的关键查询，常用于回答“我什么时候说过关于 X 的事？”或“回顾一下关于 Y 的讨论”。

func (r *MemoryRepository) FindRelatedEpisodes(ctx context.Context, userID string, keywords []string, limit int) ([]*types.Episode, error) {
	session := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		// 寻找一条路径，从 Episode 节点（e）出发，通过 [:MENTIONS] 关系，指向 Entity 节点（n）。
		// 确保只查询当前用户的记忆数据，防止不同用户的数据混淆，同时精确匹配实体名称。
		// 返回唯一的 Episode 节点，也即多次匹配同一个 Episode 只返回一次。
		// 按创建时间倒序排列，将最新的记忆排在最前面。
		// 限制返回数量（例如只取最近 5 次），防止因历史数据过多导致上下文窗口爆炸或响应过慢。
		querySimple := `
			MATCH (e:Episode)-[:MENTIONS]->(n:Entity)
			WHERE e.user_id = $user_id AND n.name IN $keywords
			RETURN DISTINCT e
			ORDER BY e.created_at DESC
			LIMIT $limit
		`

		res, err := tx.Run(ctx, querySimple, map[string]interface{}{
			"user_id":  userID,
			"keywords": keywords,
			"limit":    limit,
		})
		if err != nil {
			return nil, err
		}

		var episodes []*types.Episode
		for res.Next(ctx) {
			record := res.Record()
			node, _ := record.Get("e")
			episodeNode := node.(neo4j.Node)

			createdAtStr := episodeNode.Props["created_at"].(string)
			createdAt, _ := time.Parse(time.RFC3339, createdAtStr)

			episodes = append(episodes, &types.Episode{
				ID:        episodeNode.Props["id"].(string),
				UserID:    episodeNode.Props["user_id"].(string),
				SessionID: episodeNode.Props["session_id"].(string),
				Summary:   episodeNode.Props["summary"].(string),
				CreatedAt: createdAt,
			})
		}
		return episodes, nil
	})

	if err != nil {
		return nil, err
	}

	return result.([]*types.Episode), nil
}

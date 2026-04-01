package neo4j

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// 目标：将一组节点（Nodes）和关系（Relations）安全、高效地存入 Neo4j，同时保证数据不重复且原有关联信息不丢失。

// Neo4jRepository is a repository for Neo4j
type Neo4jRepository struct {
	driver     neo4j.Driver
	nodePrefix string
}

// NewNeo4jRepository creates a new Neo4j repository
func NewNeo4jRepository(driver neo4j.Driver) interfaces.RetrieveGraphRepository {
	return &Neo4jRepository{driver: driver, nodePrefix: "ENTITY"}
}

// _remove_hyphen removes hyphens from a string
func _remove_hyphen(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// Labels returns the labels for a namespace
func (n *Neo4jRepository) Labels(namespace types.NameSpace) []string {
	res := make([]string, 0)
	for _, label := range namespace.Labels() {
		res = append(res, n.nodePrefix+_remove_hyphen(label))
	}
	return res
}

// Label returns the label for a namespace
func (n *Neo4jRepository) Label(namespace types.NameSpace) string {
	labels := n.Labels(namespace)
	return strings.Join(labels, ":")
}

// AddGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) AddGraph(ctx context.Context, namespace types.NameSpace, graphs []*types.GraphData) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	for _, graph := range graphs {
		if err := n.addGraph(ctx, namespace, graph); err != nil {
			return err
		}
	}
	return nil
}

// addGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) addGraph(ctx context.Context, namespace types.NameSpace, graph *types.GraphData) error {
	// 声明这是一个写操作会话，驱动会将其路由到 Neo4j 集群的 Leader 节点（如果是集群模式）。
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	// 开启一个写事务
	//	- 自动重试：如果发生死锁或临时网络波动，Neo4j 驱动会自动重试整个闭包内的逻辑，无需手动编写重试循环。
	//	- 原子性：节点导入和关系导入要么全部成功，要么全部失败回滚，保证数据一致性。
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {

		// Step 1 : 把 graph.Node 中的节点导入（或更新）到 Neo4j 数据库中。

		// UNWIND $data AS row:
		//	将传入的 JSON 数组 $data 展开成多行。
		//	这意味着一次数据库交互可以处理成千上万个节点，极大地减少了网络往返次数（RTT），比循环单条插入快几十倍。
		//
		// CALL apoc.merge.node(...) (核心):
		//	这是 APOC 库提供的增强版 MERGE。
		//	参数 1 (row.labels): 动态指定节点的 Label。
		//	参数 2 ({name:..., kg:...}): 匹配键。
		//		Neo4j 会查找是否已存在具有相同 Label、相同 name 和相同 knowledge_id 的节点。
		//		 - 如果存在：更新该节点。
		//		 - 如果不存在：创建新节点。
		//	参数 3 (row.props): 设置/更新节点的属性（如 attributes）。
		//	参数 4 ({}): 如果创建新节点，额外的初始化属性（这里为空）。
		//
		// SET node.chunks = apoc.coll.union(node.chunks, row.chunks) (亮点):
		//	业务场景：同一个实体（例如“马云”）可能出现在文档的第 1 页、第 5 页和第 10 页。
		//	问题：如果用普通的 SET node.chunks = row.chunks，后导入的数据会覆盖掉之前的页码，导致溯源信息丢失。
		//	解决：apoc.coll.union 将数据库中现有的 chunks 列表与新导入的 chunks 列表取并集，并自动去重。
		//	结果：无论导入多少次，“马云”节点下的 chunks 列表都会累积所有出现过的页码。

		// Node import query
		node_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.labels, {name: row.name, kg: row.knowledge_id}, row.props, {}) YIELD node
			SET node.chunks = apoc.coll.union(node.chunks, row.chunks)
			RETURN distinct 'done' AS result
		`

		// 将 Node 结构体转换为 Map，以便传递给 Cypher 查询。
		nodeData := []map[string]interface{}{}
		for _, node := range graph.Node {
			nodeData = append(nodeData, map[string]interface{}{
				"name":         node.Name,
				"knowledge_id": namespace.Knowledge,
				"props":        map[string][]string{"attributes": node.Attributes},
				"chunks":       node.Chunks,
				"labels":       n.Labels(namespace),
			})
		}

		// 执行节点导入
		if _, err := tx.Run(ctx, node_import_query, map[string]interface{}{"data": nodeData}); err != nil {
			return nil, fmt.Errorf("failed to create nodes: %v", err)
		}

		// Step 2 : 把 graph 中的“关系”（边）导入到图数据库中。

		// CALL apoc.merge.node(row.source_labels, {name: row.source, kg: row.knowledge_id}, {}, {}) YIELD node as source
		//
		// 逻辑：
		//	去数据库找：有没有一个节点，标签是 row.source_labels，且属性 name 和 kg 与指定值匹配？
		//	如果有：直接返回这个节点对象，赋值给变量 source。
		//	如果没有：立刻创建这个新节点，并返回它。
		// 为什么这么做？
		//	防止报错：Neo4j 不允许创建指向“不存在节点”的关系。如果数据导入顺序错了（先导入了关系，后导入节点），普通写法会报错。
		//	容错性：即使上游数据漏发了节点定义，只要关系里提到了名字，这里也能自动把节点补上（虽然属性可能不全，但保证了图不断裂）。
		//
		// CALL apoc.merge.node(row.target_labels, {name: row.target, kg: row.knowledge_id}, {}, {}) YIELD node as target
		//
		//	逻辑：同上，对终点节点进行同样的“查找或创建”操作，赋值给变量 target。
		//	结果：执行完这两句后，内存中一定有两个有效的节点对象 source 和 target，无论它们之前是否存在。
		//
		// CALL apoc.merge.relationship(source, row.type, {}, row.attributes, target) YIELD rel
		//
		//	参数详解：
		//		source: 起点节点对象（上一步生成的）。
		//		row.type: 关系的类型字符串（例如 "KNOWS", "WORKS_FOR"）。
		//		{}: 匹配键（Identity Map）。
		//			这里传的是空地图 {}。
		//			含义：只要 起点 + 终点 + 类型 这三者相同，就认为是同一条关系。
		//			注：如果你希望只有当“关系属性也完全相同”才算同一条，需要在这里填入属性键。通常为了去重，只靠结构（起止点+类型）就够了。
		//		row.attributes: 更新属性。
		//			如果关系是新建的：这些属性会被写入。
		//			如果关系已存在：这些属性会覆盖/更新旧属性。
		//		target: 终点节点对象。
		//
		//	行为总结：
		//		不存在则建：创建一条从 source 到 target 类型为 row.type 的边。
		//		存在则更：更新这条边的属性为 row.attributes。
		//		绝对不重：保证数据库中不会有两条完全一样的边（Duplicate Relationships）。
		//
		// RETURN distinct 'done'
		//	含义：因为 UNWIND 产生了多行，每一行都会执行上述逻辑并返回一个 'done' 字符串。
		//	distinct：去重。不管处理了多少条数据，最终只返回一行结果 'done'。
		//	作用：减少网络传输的数据量。Go 代码只需要知道“执行成功了”，不需要接收几千个 'done' 字符串。

		// Relationship import query
		rel_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.source_labels, {name: row.source, kg: row.knowledge_id}, {}, {}) YIELD node as source
			CALL apoc.merge.node(row.target_labels, {name: row.target, kg: row.knowledge_id}, {}, {}) YIELD node as target
			CALL apoc.merge.relationship(source, row.type, {}, row.attributes, target) YIELD rel
			RETURN distinct 'done'
		`

		// 节点关系
		relData := []map[string]interface{}{}
		for _, rel := range graph.Relation {
			relData = append(relData, map[string]interface{}{
				"source":        rel.Node1,
				"target":        rel.Node2,
				"knowledge_id":  namespace.Knowledge,
				"type":          rel.Type,
				"source_labels": n.Labels(namespace),
				"target_labels": n.Labels(namespace),
			})
		}
		if _, err := tx.Run(ctx, rel_import_query, map[string]interface{}{"data": relData}); err != nil {
			return nil, fmt.Errorf("failed to create relationships: %v", err)
		}
		return nil, nil
	})
	if err != nil {
		logger.Errorf(ctx, "failed to add graph: %v", err)
		return err
	}
	return nil
}

// DelGraph deletes a graph from the Neo4j repository
func (n *Neo4jRepository) DelGraph(ctx context.Context, namespaces []types.NameSpace) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	result, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		for _, namespace := range namespaces {
			labelExpr := n.Label(namespace)

			// 在删除节点之前，必须先清理掉它们身上的关系。如果不先删关系，直接删节点会报错。

			// MATCH (n: labels {kg: $knowledge_id})-[r]-(m: labels {kg: $knowledge_id}) RETURN r
			// 作用：定义“要删除哪些数据”。
			// 逻辑：
			//	查找所有满足条件的关系 r 。
			//
			//	(n: labels {kg: $knowledge_id}): 起点节点 (n)
			//		: labels: 这是一个动态占位符。在实际执行时，它会被替换为具体的标签（如 :Person:Entity）。这限定了节点的类型。
			//		{kg: $knowledge_id}: 这是属性过滤。只选择那些 kg 属性等于传入参数 $knowledge_id 的节点。
			//		含义：找到一个属于特定知识库、且类型正确的节点 n。
			//	-[r]-: 关系 (r)
			//		-: 表示无向连接（不区分方向，无论是 n->m 还是 m->n 都匹配）。
			//		[r]: 捕获这条连线本身，命名为 r。
			//		含义：找到连接两个节点的任何类型的边。
			//	(m: labels {kg: $knowledge_id}): 终点节点 (m)
			//		条件与起点 n 完全一致。
			//		含义：找到另一个同样属于该知识库、且类型正确的节点 m。
			//	RETURN r: 最终输出
			//		关键点：我们只返回关系 r，不返回节点 n 或 m。
			//		原因：因为接下来的操作是 DELETE r（只删边）。只返回需要操作的对象可以减少内存传输开销，提高效率。

			// {batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
			//
			// 这是 apoc.periodic.iterate 过程的配置选项（Config Map）：
			//	batchSize: 1000
			//		含义：每次事务只处理 1000 条数据。
			//		流程：
			//			APOC 先运行查询语句，找到所有符合条件的数据（比如 100 万条）。
			//			它不会一次性把这 100 万条都加载到内存里。
			//			它每次只取 1000 条。
			//			开启一个小事务 -> 执行删除 -> 提交事务 -> 释放这 1000 条的内存。
			//			再取下一批 1000 条，重复上述过程。
			//		为什么重要？
			//			防止内存溢出 (OOM)：如果设为 0 或不设（默认可能很大），Neo4j 会尝试一次性加载所有数据，导致服务器内存爆满崩溃。
			//			避免长事务锁：小批次意味着事务很快提交，不会长时间锁定数据库资源，允许其他用户并发读写。
			//		调优建议：
			//			1000 是一个安全的默认值。
			//			如果服务器内存很大且追求极致速度，可以调到 5000 或 10000。
			//			如果关系非常复杂（属性很多），可以降到 500。
			//  parallel: true —— 多线程加速器
			//		含义：启用并行模式。
			//		工作流程：
			//			关闭时 (false)：串行执行。处理完第 1 批 (1-1000)，再处理第 2 批 (1001-2000)... 像单线程排队。
			//			开启时 (true)：APOC 会启动多个后台线程（通常等于 CPU 核心数）。
			//				线程 A 处理第 1 批。
			//				线程 B 同时处理第 2 批。
			//				线程 C 同时处理第 3 批。
			//		为什么重要？
			//			速度提升：对于 DELETE 这种主要消耗 I/O 和 CPU 的操作，并行处理可以将速度提升 3 倍到 8 倍（取决于核心数）。
			//		注意事项：
			//			只有在操作互不冲突时才安全。删除不同的关系通常没有依赖关系，所以非常安全。
			//			如果是更新操作（且更新同一节点的不同属性），并行可能会导致死锁或竞争，那时需要设为 false。
			//			但在删除关系场景下，true 是最佳选择。
			//  params: {knowledge_id: $knowledge_id} —— 变量透传与安全
			//		含义：将外部传入的参数 $knowledge_id 显式地传递给内部的查询语句和执行语句。
			//		为什么需要它？
			//			apoc.periodic.iterate 内部实际上是在运行两个独立的 Cypher 字符串（一个查，一个删）。
			//			外层的 $knowledge_id 变量不会自动进入这两个内部字符串的作用域。
			//			你必须通过 params 地图，手动把变量“喂”给内部语句。
			//		安全性：
			//			这是一种参数化查询的最佳实践。
			//			它防止了 SQL/Cypher 注入攻击（因为值是作为参数传递，而不是拼接到字符串里）。
			//			确保内部执行的每一批操作都严格使用同一个 knowledge_id，保证数据隔离。
			deleteRelsQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id})-[r]-(m:` + labelExpr + ` {kg: $knowledge_id}) RETURN r",
					"DELETE r",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteRelsQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete relationships: %v", err)
			}

			deleteNodesQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id}) RETURN n",
					"DELETE n",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteNodesQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete nodes: %v", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	logger.Infof(ctx, "delete graph result: %v", result)
	return nil
}

// 在指定的知识库（Namespace）中，查找名称包含给定关键词列表的节点，
// 并返回这些节点及其直接相连的邻居节点和关系，最终组装成前端可用的图结构数据（GraphData）。

// MATCH (n:` + labelExpr + `)-[r]-(m:` + labelExpr + `)
// WHERE ANY(nodeText IN $nodes WHERE n.name CONTAINS nodeText)
// RETURN n, r, m
//
// MATCH (n:Label)-[r]-(m:Label): 查找所有连接两个同标签节点的关系。
//	注意：这里没有指定方向（-[r]- 是无向的），意味着只要 n 和 m 有连线，无论方向如何都会匹配。
//	限制：n 和 m 都必须属于当前 namespace 定义的标签（通过 labelExpr 动态注入）。
//
// WHERE ANY(nodeText IN $nodes WHERE n.name CONTAINS nodeText)
//	逻辑：遍历传入的参数列表 $nodes（例如 ["苹果", "香蕉"]）。
//	判断：只要起点节点 n 的 name 属性包含列表中的任意一个字符串，该路径就符合条件。
//	效果：实现了多关键词模糊搜索。
//
//	如果搜 "苹果"，会找到名字叫 "红苹果"、"苹果公司" 的节点作为起点 n。
//	隐含逻辑：虽然 WHERE 只限制了 n，但 MATCH 返回了 m（邻居）。
//		这意味着：搜索结果不仅包含匹配的节点，还包含它们的一度关联节点。
//		这通常用于展示“上下文”或“关联图谱”。
//
// RETURN n, r, m
//	返回起点 n、关系 r、终点 m。

// SearchNode searches for nodes in the Neo4j repository
func (n *Neo4jRepository) SearchNode(
	ctx context.Context,
	namespace types.NameSpace,
	nodes []string, // 搜索关键词列表，例如 ["苹果", "手机"]
) (*types.GraphData, error) {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil, nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		labelExpr := n.Label(namespace)
		query := `
			MATCH (n:` + labelExpr + `)-[r]-(m:` + labelExpr + `)
			WHERE ANY(nodeText IN $nodes WHERE n.name CONTAINS nodeText)
			RETURN n, r, m
		`
		params := map[string]interface{}{"nodes": nodes}
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, fmt.Errorf("failed to run query: %v", err)
		}

		graphData := &types.GraphData{}
		nodeSeen := make(map[string]bool)
		for result.Next(ctx) {
			record := result.Record()
			node, _ := record.Get("n")
			rel, _ := record.Get("r")
			targetNode, _ := record.Get("m")

			nodeData := node.(neo4j.Node)
			targetNodeData := targetNode.(neo4j.Node)

			// Convert node to types.Node
			for _, n := range []neo4j.Node{nodeData, targetNodeData} {
				nameStr := n.Props["name"].(string)
				if _, ok := nodeSeen[nameStr]; !ok {
					nodeSeen[nameStr] = true
					// 提取节点
					graphData.Node = append(graphData.Node, &types.GraphNode{
						Name:       nameStr,
						Chunks:     listI2listS(n.Props["chunks"].([]interface{})),
						Attributes: listI2listS(n.Props["attributes"].([]interface{})),
					})
				}
			}

			// 提取关系

			// Convert relationship to types.Relation
			relData := rel.(neo4j.Relationship)
			graphData.Relation = append(graphData.Relation, &types.GraphRelation{
				Node1: nodeData.Props["name"].(string),
				Node2: targetNodeData.Props["name"].(string),
				Type:  relData.Type,
			})
		}
		return graphData, nil
	})
	if err != nil {
		logger.Errorf(ctx, "search node failed: %v", err)
		return nil, err
	}
	return result.(*types.GraphData), nil
}

func listI2listS(list []any) []string {
	result := make([]string, len(list))
	for i, v := range list {
		result[i] = fmt.Sprintf("%v", v)
	}
	return result
}

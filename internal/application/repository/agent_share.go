package repository

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// 构建了一个“创建者将 Agent 共享给特定组织，该组织的成员即可使用”的权限体系。

// 表关系与核心逻辑说明：
//
// 1. custom_agents (核心资源表)
//    - 含义：存储用户创建的 AI Agent（智能体）定义。
//    - 关键字段：id, tenant_id (所属租户), deleted_at。
//    - 角色：被共享的资源主体。
//    - 关系：
//      * 一个 Agent 属于一个特定的租户 (source_tenant_id)。
//      * 一个 Agent 可以被共享给多个组织（一对多关系，通过 agent_shares 表实现）。
//
// 2. organizations (组织机构表)
//    - 含义：存储租户下的组织/团队信息。
//    - 关键字段：id, deleted_at。
//    - 角色：共享的接收方容器。
//    - 关系：
//      * 一个组织可以接收来自不同租户的多个 Agent 共享。
//      * 一个组织包含多个成员。
//
// 3. organization_members (用户 - 组织关联表)
//    - 含义：多对多关系表，记录哪些用户属于哪些组织。
//    - 关键字段：user_id, organization_id。
//    - 角色：权限桥梁。
//    - 逻辑：用户本身不直接拥有共享记录，而是通过“属于某个组织”来间接获得该组织接收到的 Agent 的使用权。
//    - 代码体现：JOIN organization_members ON ... user_id = ?
//
// 4. agent_shares (共享关系核心表)
//    - 含义：记录具体的共享行为。
//    - 关键字段：
//      * agent_id: 指向 custom_agents.id (谁被共享了？)
//      * source_tenant_id: 创建者的租户 ID (谁分享的？)
//      * organization_id: 指向 organizations.id (共享给了哪个组织？)
//      * deleted_at: 软删除标记。
//    - 角色：连接资源与容器的纽带。
//    - 核心逻辑：
//      * 它定义了 custom_agents 和 organizations 之间的多对多关系。
//      * 重要约束：代码中多次出现 JOIN custom_agents ... AND custom_agents.deleted_at IS NULL。
//        这意味着如果源 Agent 被删除了，即使这条共享记录还在，它也是无效的（级联逻辑通过 JOIN 隐式实现，而非物理删除共享记录）。

var (
	ErrAgentShareNotFound      = errors.New("agent share not found")
	ErrAgentShareAlreadyExists = errors.New("agent already shared to this organization")
)

// agentShareRepository implements AgentShareRepository interface
type agentShareRepository struct {
	db *gorm.DB
}

// NewAgentShareRepository creates a new agent share repository
func NewAgentShareRepository(db *gorm.DB) interfaces.AgentShareRepository {
	return &agentShareRepository{db: db}
}

// Create creates a new agent share record
func (r *agentShareRepository) Create(ctx context.Context, share *types.AgentShare) error {
	var count int64
	r.db.WithContext(ctx).Model(&types.AgentShare{}).
		Where("agent_id = ? AND source_tenant_id = ? AND organization_id = ? AND deleted_at IS NULL",
			share.AgentID, share.SourceTenantID, share.OrganizationID).
		Count(&count)
	if count > 0 {
		return ErrAgentShareAlreadyExists
	}
	return r.db.WithContext(ctx).Create(share).Error
}

// GetByID gets a share record by ID
func (r *agentShareRepository) GetByID(ctx context.Context, id string) (*types.AgentShare, error) {
	var share types.AgentShare
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&share).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAgentShareNotFound
		}
		return nil, err
	}
	return &share, nil
}

// GetByAgentAndOrg gets a share record by agent ID and organization ID
func (r *agentShareRepository) GetByAgentAndOrg(ctx context.Context, agentID string, orgID string) (*types.AgentShare, error) {
	var share types.AgentShare
	err := r.db.WithContext(ctx).
		Where("agent_id = ? AND organization_id = ?", agentID, orgID).
		First(&share).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAgentShareNotFound
		}
		return nil, err
	}
	return &share, nil
}

// Update updates a share record
func (r *agentShareRepository) Update(ctx context.Context, share *types.AgentShare) error {
	return r.db.WithContext(ctx).Model(&types.AgentShare{}).
		Where("id = ?", share.ID).Updates(share).Error
}

// Delete soft deletes a share record
func (r *agentShareRepository) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&types.AgentShare{}).Error
}

// DeleteByAgentIDAndSourceTenant soft deletes all share records for an agent (id, tenant_id)
func (r *agentShareRepository) DeleteByAgentIDAndSourceTenant(ctx context.Context, agentID string, sourceTenantID uint64) error {
	return r.db.WithContext(ctx).
		Where("agent_id = ? AND source_tenant_id = ?", agentID, sourceTenantID).
		Delete(&types.AgentShare{}).Error
}

// DeleteByOrganizationID soft deletes all share records for an organization
func (r *agentShareRepository) DeleteByOrganizationID(ctx context.Context, orgID string) error {
	return r.db.WithContext(ctx).Where("organization_id = ?", orgID).Delete(&types.AgentShare{}).Error
}

// ListByAgent lists all share records for an agent
func (r *agentShareRepository) ListByAgent(ctx context.Context, agentID string) ([]*types.AgentShare, error) {
	var shares []*types.AgentShare
	err := r.db.WithContext(ctx).
		Preload("Organization").
		Where("agent_id = ?", agentID).
		Order("created_at DESC").
		Find(&shares).Error
	if err != nil {
		return nil, err
	}
	return shares, nil
}

// ListByOrganization lists all share records for an organization (excluding deleted agents)
func (r *agentShareRepository) ListByOrganization(ctx context.Context, orgID string) ([]*types.AgentShare, error) {
	var shares []*types.AgentShare
	err := r.db.WithContext(ctx).
		Joins("JOIN custom_agents ON custom_agents.id = agent_shares.agent_id AND custom_agents.tenant_id = agent_shares.source_tenant_id AND custom_agents.deleted_at IS NULL").
		Preload("Agent").
		Preload("Organization").
		Where("agent_shares.organization_id = ? AND agent_shares.deleted_at IS NULL", orgID).
		Order("agent_shares.created_at DESC").
		Find(&shares).Error
	if err != nil {
		return nil, err
	}
	return shares, nil
}

// ListByOrganizations lists all share records for the given organizations (batch).
func (r *agentShareRepository) ListByOrganizations(ctx context.Context, orgIDs []string) ([]*types.AgentShare, error) {
	if len(orgIDs) == 0 {
		return nil, nil
	}
	var shares []*types.AgentShare
	err := r.db.WithContext(ctx).
		Joins("JOIN custom_agents ON custom_agents.id = agent_shares.agent_id AND custom_agents.tenant_id = agent_shares.source_tenant_id AND custom_agents.deleted_at IS NULL").
		Preload("Agent").
		Preload("Organization").
		Where("agent_shares.organization_id IN ? AND agent_shares.deleted_at IS NULL", orgIDs).
		Order("agent_shares.created_at DESC").
		Find(&shares).Error
	if err != nil {
		return nil, err
	}
	return shares, nil
}

// CountByOrganizations 批量统计指定组织列表中的有效 Agent 共享数量。
//
// 主要功能：
// 1. 接收一组组织 ID (orgIDs)，统计每个组织当前拥有的有效共享 Agent 数量。
// 2. 数据有效性校验：通过 JOIN `custom_agents` 表，确保仅统计源 Agent 存在且未被删除的共享记录。
//    如果源 Agent 已被删除，即使共享记录存在，也不计入总数。
// 3. 软删除过滤：仅统计 `agent_shares` 表中 `deleted_at` 为 NULL 的记录。
// 4. 完整性保证：返回的 map 必定包含输入列表中的所有 orgID。如果某个组织没有有效共享记录，其计数值设为 0。
//
// 参数:
//   - ctx: 上下文对象，用于控制超时、取消操作及传递链路追踪信息。
//   - orgIDs: 目标组织的 ID 列表。如果列表为空，返回一个空的 map。
//
// 返回:
//   - map[string]int64: 键为组织 ID，值为对应的有效共享数量。
//   - error: 数据库操作错误。
//
// 注意:
//   - 该函数使用 SQL GROUP BY 进行聚合查询，适合批量统计场景。
//   - 即使数据库中没有某组织的记录，结果中也会包含该组织且计数为 0，方便调用方直接使用。

// CountByOrganizations returns share counts per organization (only orgs in orgIDs). Excludes deleted agents.
func (r *agentShareRepository) CountByOrganizations(ctx context.Context, orgIDs []string) (map[string]int64, error) {
	if len(orgIDs) == 0 {
		return make(map[string]int64), nil
	}
	type row struct {
		OrgID string `gorm:"column:organization_id"`
		Count int64  `gorm:"column:count"`
	}
	var rows []row
	err := r.db.WithContext(ctx).Model(&types.AgentShare{}).
		Joins("JOIN custom_agents ON custom_agents.id = agent_shares.agent_id AND custom_agents.tenant_id = agent_shares.source_tenant_id AND custom_agents.deleted_at IS NULL").
		Select("agent_shares.organization_id as organization_id, COUNT(*) as count").
		Where("agent_shares.organization_id IN ? AND agent_shares.deleted_at IS NULL", orgIDs).
		Group("agent_shares.organization_id").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64)
	for _, o := range orgIDs {
		out[o] = 0
	}
	for _, r := range rows {
		out[r.OrgID] = r.Count
	}
	return out, nil
}

// ListSharedAgentsForUser 查询当前用户有权访问的所有共享 Agent 记录。
//
// 主要功能：
// 1. 基于用户身份 (`userID`) 检索其所属组织接收到的所有有效共享记录。
// 2. 多重有效性校验：
//    - 源 Agent 有效：通过 JOIN `custom_agents` 确保源智能体存在且未删除。
//    - 目标组织有效：通过 JOIN `organizations` 确保目标组织存在且未删除。
//    - 共享记录有效：过滤 `agent_shares` 表中已软删除的记录。
// 3. 权限验证：通过 JOIN `organization_members` 表，严格限制仅返回该用户作为成员的组织所对应的共享记录。
// 4. 关联加载：自动预加载 (Preload) `Agent` (源智能体) 和 `Organization` (目标组织) 详情。
// 5. 排序：结果按共享创建时间倒序排列。
//
// 参数:
//   - ctx: 上下文对象，用于控制超时、取消操作及传递链路追踪信息。
//   - userID: 当前用户的唯一标识符。
//
// 返回:
//   - []*types.AgentShare: 用户有权访问的共享记录列表。若无记录，返回空切片。
//   - error: 数据库操作错误。
//
// 注意:
//   - 该查询涉及多表连接 (Agent, Organization, Member, Share)，确保了数据在租户、组织和用户维度上的完整一致性。
//   - 只有当用户同时满足“是组织成员”、“组织未删除”、“源Agent未删除”且“共享记录未删除”时，数据才会被返回。

// ListSharedAgentsForUser lists all agents shared to organizations that the user belongs to
func (r *agentShareRepository) ListSharedAgentsForUser(ctx context.Context, userID string) ([]*types.AgentShare, error) {
	var shares []*types.AgentShare
	err := r.db.WithContext(ctx).
		Joins("JOIN custom_agents ON custom_agents.id = agent_shares.agent_id AND custom_agents.tenant_id = agent_shares.source_tenant_id AND custom_agents.deleted_at IS NULL").
		Preload("Agent").
		Preload("Organization").
		Joins("JOIN organization_members ON organization_members.organization_id = agent_shares.organization_id").
		Joins("JOIN organizations ON organizations.id = agent_shares.organization_id AND organizations.deleted_at IS NULL").
		Where("organization_members.user_id = ?", userID).
		Where("agent_shares.deleted_at IS NULL").
		Order("agent_shares.created_at DESC").
		Find(&shares).Error
	if err != nil {
		return nil, err
	}
	return shares, nil
}

// GetShareByAgentIDForUser 查询用户是否有权限通过共享方式访问指定的 Agent，并返回对应的共享记录。
//
// 主要功能：
// 1. 权限验证：检查指定用户 (`userID`) 是否属于接收该 Agent 共享的组织 (通过 `organization_members` 表关联)。
// 2. 来源排除：显式排除源租户 ID (`source_tenant_id`) 等于 `excludeTenantID` 的记录。
//    - 业务场景：通常用于区分“用户自己创建的 Agent”和“他人共享给用户的 Agent”。
//   			如果源租户是用户自己的租户，则视为自有资源，不走共享逻辑，因此予以排除。
// 3. 有效性过滤：仅查询未软删除 (`deleted_at IS NULL`) 的共享记录。
// 4. 单次查询：通过链式调用一次性完成过滤和查找，效率高。
//
// 参数:
//   - ctx: 上下文对象，用于控制超时、取消操作及传递链路追踪信息。
//   - userID: 当前用户的唯一标识符。
//   - agentID: 目标 Agent 的唯一标识符。
//   - excludeTenantID: 需要排除的源租户 ID (通常是用户所属的主租户 ID)。
//
// 返回:
//   - *types.AgentShare: 找到的共享记录指针。
//   - error:
//     - 若未找到记录，返回 `ErrAgentShareNotFound`。
//     - 若发生其他数据库错误，返回原始错误。
//
// 注意:
//   - 此方法不预加载关联的 Agent 或 Organization 信息，仅返回共享记录本身，适用于仅需验证权限或获取共享元数据的场景。
//   - 如果用户不在任何接收了该 Agent 共享的组织中，或者该共享来源于被排除的租户，都将视为“未找到”。

// GetShareByAgentIDForUser returns one share for the given agentID that the user can access (user in org), excluding source_tenant_id == excludeTenantID. Single query.
func (r *agentShareRepository) GetShareByAgentIDForUser(ctx context.Context, userID, agentID string, excludeTenantID uint64) (*types.AgentShare, error) {
	var share types.AgentShare
	err := r.db.WithContext(ctx).
		Joins("JOIN organization_members ON organization_members.organization_id = agent_shares.organization_id").
		Where("agent_shares.agent_id = ?", agentID).
		Where("organization_members.user_id = ?", userID).
		Where("agent_shares.source_tenant_id != ?", excludeTenantID).
		Where("agent_shares.deleted_at IS NULL").
		First(&share).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAgentShareNotFound
		}
		return nil, err
	}
	return &share, nil
}

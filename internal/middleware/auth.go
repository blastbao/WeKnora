package middleware

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// 请求鉴权流程
//
// 	请求进来
//	  ↓
//	1. 跳过 OPTIONS 预检请求
//	2. 检查是不是【免登录接口】（健康检查、登录、注册）
//	  ↓ 是 → 直接通过
//	  ↓ 否
//	3. 先尝试验证 JWT Token（用户登录）
//	  ↓ 成功
//		 → 检查是否要【跨租户访问】
//		 → 校验目标租户是否存在
//		 → 把【用户信息 + 租户信息】放入上下文
//	  ↓ 失败
//	4. 再尝试验证 API-Key（系统/程序调用）
//	  ↓ 成功
//		 → 校验租户
//		 → 放入租户信息
//	  ↓ 失败
//	5. 返回 401 未授权

// 核心功能：双轨认证机制，以适应不同的使用场景，并遵循“JWT 优先，API Key 兜底”的原则。
//
// JWT Token 认证 (面向用户)
//	适用场景：主要用于 Web UI 或 App 的用户登录态管理。
//	认证流程：
//		- 从请求头 Authorization: Bearer <token> 中提取 Token。
//		- 调用 userService.ValidateToken 验证 Token 的有效性，并获取关联的用户信息 (user)。
//	高级特性 - 租户切换：这是该实现的一大亮点。
//		- 即使用户认证成功，系统还会检查一个特殊的请求头 X-Tenant-ID。
//		- 如果存在此头，且当前用户拥有 CanAccessAllTenants 权限（例如系统管理员），中间件会调用 canAccessTenant 函数进行二次授权。
//		- 授权通过后，请求的上下文将被“切换”到目标租户。这意味着一个管理员用户可以用一个账号，在不重新登录的情况下，访问和管理不同租户的数据。
//
// API Key 认证 (面向服务/系统)
//	适用场景：主要用于系统间集成、自动化脚本或 MCP 客户端调用。
//	认证流程：
//		- 从请求头 X-API-Key 中提取密钥。
//		- 调用 tenantService.ExtractTenantIDFromAPIKey 从密钥中解析出 tenantID。这个密钥通常是一个加密字符串，内部包含了租户ID信息，确保了密钥与租户的强绑定。
//		- 查询数据库，验证该密钥是否与数据库中存储的完全一致，确保密钥未被篡改或撤销。
//	特点：这种认证方式是无状态的，权限直接绑定到整个租户，不涉及具体用户。

// 无需认证的API列表
var noAuthAPI = map[string][]string{
	"/health":               {"GET"},
	"/api/v1/auth/register": {"POST"},
	"/api/v1/auth/login":    {"POST"},
	"/api/v1/auth/refresh":  {"POST"},
}

// 检查请求是否在无需认证的API列表中
func isNoAuthAPI(path string, method string) bool {
	for api, methods := range noAuthAPI {
		// 如果以*结尾，按照前缀匹配，否则按照全路径匹配
		if strings.HasSuffix(api, "*") {
			if strings.HasPrefix(path, strings.TrimSuffix(api, "*")) && slices.Contains(methods, method) {
				return true
			}
		} else if path == api && slices.Contains(methods, method) {
			return true
		}
	}
	return false
}

// canAccessTenant checks if a user can access a target tenant
func canAccessTenant(user *types.User, targetTenantID uint64, cfg *config.Config) bool {
	// 1. 检查功能是否启用
	if cfg == nil || cfg.Tenant == nil || !cfg.Tenant.EnableCrossTenantAccess {
		return false
	}
	// 2. 检查用户权限
	if !user.CanAccessAllTenants {
		return false
	}
	// 3. 如果目标租户是用户自己的租户，允许访问
	if user.TenantID == targetTenantID {
		return true
	}
	// 4. 用户有跨租户权限，允许访问（具体验证在中间件中完成）
	return true
}

// Auth 认证中间件
func Auth(
	tenantService interfaces.TenantService,
	userService interfaces.UserService,
	cfg *config.Config,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ignore OPTIONS request
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		// 检查请求是否在无需认证的API列表中
		if isNoAuthAPI(c.Request.URL.Path, c.Request.Method) {
			c.Next()
			return
		}

		// 尝试JWT Token认证
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			user, err := userService.ValidateToken(c.Request.Context(), token)
			if err == nil && user != nil {
				// JWT Token认证成功
				// 检查是否有跨租户访问请求
				targetTenantID := user.TenantID
				tenantHeader := c.GetHeader("X-Tenant-ID")
				if tenantHeader != "" {
					// 解析目标租户ID
					parsedTenantID, err := strconv.ParseUint(tenantHeader, 10, 64)
					if err == nil {
						// 检查用户是否有跨租户访问权限
						if canAccessTenant(user, parsedTenantID, cfg) {
							// 验证目标租户是否存在
							targetTenant, err := tenantService.GetTenantByID(c.Request.Context(), parsedTenantID)
							if err == nil && targetTenant != nil {
								targetTenantID = parsedTenantID
								log.Printf("User %s switching to tenant %d", user.ID, targetTenantID)
							} else {
								log.Printf("Error getting target tenant by ID: %v, tenantID: %d", err, parsedTenantID)
								c.JSON(http.StatusBadRequest, gin.H{
									"error": "Invalid target tenant ID",
								})
								c.Abort()
								return
							}
						} else {
							// 用户没有权限访问目标租户
							log.Printf("User %s attempted to access tenant %d without permission", user.ID, parsedTenantID)
							c.JSON(http.StatusForbidden, gin.H{
								"error": "Forbidden: insufficient permissions to access target tenant",
							})
							c.Abort()
							return
						}
					}
				}

				// 获取租户信息（使用目标租户ID）
				tenant, err := tenantService.GetTenantByID(c.Request.Context(), targetTenantID)
				if err != nil {
					log.Printf("Error getting tenant by ID: %v, tenantID: %d, userID: %s", err, targetTenantID, user.ID)
					c.JSON(http.StatusUnauthorized, gin.H{
						"error": "Unauthorized: invalid tenant",
					})
					c.Abort()
					return
				}

				// 存储用户和租户信息到上下文
				c.Set(types.TenantIDContextKey.String(), targetTenantID)
				c.Set(types.TenantInfoContextKey.String(), tenant)
				c.Set(types.UserContextKey.String(), user)
				c.Set(types.UserIDContextKey.String(), user.ID)
				c.Request = c.Request.WithContext(
					context.WithValue(
						context.WithValue(
							context.WithValue(
								context.WithValue(c.Request.Context(), types.TenantIDContextKey, targetTenantID),
								types.TenantInfoContextKey, tenant,
							),
							types.UserContextKey, user,
						),
						types.UserIDContextKey, user.ID,
					),
				)
				c.Next()
				return
			}
		}

		// 尝试X-API-Key认证（兼容模式）
		apiKey := c.GetHeader("X-API-Key")
		if apiKey != "" {
			// Get tenant information
			tenantID, err := tenantService.ExtractTenantIDFromAPIKey(apiKey)
			if err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "Unauthorized: invalid API key format",
				})
				c.Abort()
				return
			}

			// Verify API key validity (matches the one in database)
			t, err := tenantService.GetTenantByID(c.Request.Context(), tenantID)
			if err != nil {
				log.Printf("Error getting tenant by ID: %v, tenantID: %d", err, tenantID)
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "Unauthorized: invalid API key",
				})
				c.Abort()
				return
			}

			if t == nil || t.APIKey != apiKey {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "Unauthorized: invalid API key",
				})
				c.Abort()
				return
			}

			// Store tenant ID in context
			c.Set(types.TenantIDContextKey.String(), tenantID)
			c.Set(types.TenantInfoContextKey.String(), t)
			c.Request = c.Request.WithContext(
				context.WithValue(
					context.WithValue(c.Request.Context(), types.TenantIDContextKey, tenantID),
					types.TenantInfoContextKey, t,
				),
			)
			c.Next()
			return
		}

		// 没有提供任何认证信息
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: missing authentication"})
		c.Abort()
	}
}

// GetTenantIDFromContext helper function to get tenant ID from context
func GetTenantIDFromContext(ctx context.Context) (uint64, error) {
	tenantID, ok := ctx.Value("tenantID").(uint64)
	if !ok {
		return 0, errors.New("tenant ID not found in context")
	}
	return tenantID, nil
}

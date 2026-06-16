package services

import (
	"context"
	"encoding/json"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/failover"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
	"gpt-load/internal/syncer"
	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const GroupUpdateChannel = "groups:updated"

// GroupManager 管理分组数据的缓存
type GroupManager struct {
	syncer          *syncer.CacheSyncer[map[string]*models.Group]
	db              *gorm.DB
	store           store.Store
	settingsManager *config.SystemSettingsManager
	subGroupManager *SubGroupManager
}

// NewGroupManager 创建未初始化的 GroupManager
func NewGroupManager(
	db *gorm.DB,
	store store.Store,
	settingsManager *config.SystemSettingsManager,
	subGroupManager *SubGroupManager,
) *GroupManager {
	return &GroupManager{
		db:              db,
		store:           store,
		settingsManager: settingsManager,
		subGroupManager: subGroupManager,
	}
}

// Initialize 设置 CacheSyncer。单独调用以处理潜在的错误
func (gm *GroupManager) Initialize() error {
	loader := func() (map[string]*models.Group, error) {
		var groups []*models.Group
		if err := gm.db.Find(&groups).Error; err != nil {
			return nil, fmt.Errorf("failed to load groups from db: %w", err)
		}

		// 加载聚合分组的所有子分组关系（仅有效的，权重 > 0 的）
		var allSubGroups []models.GroupSubGroup
		if err := gm.db.Where("weight > 0").Find(&allSubGroups).Error; err != nil {
			return nil, fmt.Errorf("failed to load valid sub groups: %w", err)
		}

		// 按聚合分组 ID 对子分组进行分组
		subGroupsByAggregateID := make(map[uint][]models.GroupSubGroup)
		for _, sg := range allSubGroups {
			subGroupsByAggregateID[sg.GroupID] = append(subGroupsByAggregateID[sg.GroupID], sg)
		}

		// 创建分组 ID 到分组对象的映射，用于子分组查找
		groupByID := make(map[uint]*models.Group)
		for _, group := range groups {
			groupByID[group.ID] = group
		}

		groupMap := make(map[string]*models.Group, len(groups))
		for _, group := range groups {
			g := *group
			g.EffectiveConfig = gm.settingsManager.GetEffectiveConfig(g.Config)
			g.ProxyKeysMap = utils.StringToSet(g.ProxyKeys, ",")

			matcher, err := failover.ParseStatusCodeMatcher(g.EffectiveConfig.FailoverStatusCodes)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"group_name": g.Name,
					"spec":       g.EffectiveConfig.FailoverStatusCodes,
					"error":      err,
				}).Warn("Invalid failover status codes spec, ignoring")
			} else {
				g.FailoverStatusCodeMatcher = matcher
			}

			// 带错误处理地解析头部规则
			if len(group.HeaderRules) > 0 {
				if err := json.Unmarshal(group.HeaderRules, &g.HeaderRuleList); err != nil {
					logrus.WithError(err).WithField("group_name", g.Name).Warn("Failed to parse header rules for group")
					g.HeaderRuleList = []models.HeaderRule{}
				}
			} else {
				g.HeaderRuleList = []models.HeaderRule{}
			}

			// 带错误处理地解析模型重定向规则
			g.ModelRedirectMap = make(map[string]string)
			if len(group.ModelRedirectRules) > 0 {
				hasInvalidRules := false
				for key, value := range group.ModelRedirectRules {
					if valueStr, ok := value.(string); ok {
						g.ModelRedirectMap[key] = valueStr
					} else {
						logrus.WithFields(logrus.Fields{
							"group_name": g.Name,
							"rule_key":   key,
							"value_type": fmt.Sprintf("%T", value),
							"value":      value,
						}).Error("Invalid model redirect rule value type, skipping this rule")
						hasInvalidRules = true
					}
				}
				if hasInvalidRules {
					logrus.WithField("group_name", g.Name).Warn("Group has invalid model redirect rules, some rules were skipped. Please check the configuration.")
				}
			}

			// 加载聚合分组的子分组
			if g.GroupType == "aggregate" {
				if subGroups, ok := subGroupsByAggregateID[g.ID]; ok {
					g.SubGroups = make([]models.GroupSubGroup, len(subGroups))
					for i, sg := range subGroups {
						g.SubGroups[i] = sg
						if subGroup, exists := groupByID[sg.SubGroupID]; exists {
							g.SubGroups[i].SubGroupName = subGroup.Name
						}
					}
				}
			}

			groupMap[g.Name] = &g
			logrus.WithFields(logrus.Fields{
				"group_name":                 g.Name,
				"effective_config":           g.EffectiveConfig,
				"header_rules_count":         len(g.HeaderRuleList),
				"model_redirect_rules_count": len(g.ModelRedirectMap),
				"model_redirect_strict":      g.ModelRedirectStrict,
				"sub_group_count":            len(g.SubGroups),
			}).Debug("Loaded group with effective config")
		}

		return groupMap, nil
	}

	afterReload := func(newCache map[string]*models.Group) {
		gm.subGroupManager.RebuildSelectors(newCache)
	}

	syncer, err := syncer.NewCacheSyncer(
		loader,
		gm.store,
		GroupUpdateChannel,
		logrus.WithField("syncer", "groups"),
		afterReload,
	)
	if err != nil {
		return fmt.Errorf("failed to create group syncer: %w", err)
	}
	gm.syncer = syncer
	return nil
}

// GetGroupByName 从缓存中按名称获取单个分组
func (gm *GroupManager) GetGroupByName(name string) (*models.Group, error) {
	if gm.syncer == nil {
		return nil, fmt.Errorf("GroupManager is not initialized")
	}

	groups := gm.syncer.Get()
	group, ok := groups[name]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return group, nil
}

// GetGroupByID 从缓存中按 ID 获取单个分组
func (gm *GroupManager) GetGroupByID(id uint) (*models.Group, error) {
	if gm.syncer == nil {
		return nil, fmt.Errorf("GroupManager is not initialized")
	}

	groups := gm.syncer.Get()
	for _, group := range groups {
		if group.ID == id {
			return group, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

// Invalidate 触发所有实例的缓存重新加载
func (gm *GroupManager) Invalidate() error {
	if gm.syncer == nil {
		return fmt.Errorf("GroupManager is not initialized")
	}
	return gm.syncer.Invalidate()
}

// Stop 优雅停止 GroupManager 的后台同步器
func (gm *GroupManager) Stop(ctx context.Context) {
	if gm.syncer != nil {
		gm.syncer.Stop()
	}
}

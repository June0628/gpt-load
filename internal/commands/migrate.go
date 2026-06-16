package commands

import (
	"flag"
	"fmt"
	"gpt-load/internal/container"
	db "gpt-load/internal/db/migrations"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
	"gpt-load/internal/types"
	"gpt-load/internal/utils"
	"os"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// RunMigrateKeys 处理 migrate-keys 命令入口
func RunMigrateKeys(args []string) {
	// 解析 migrate-keys 子命令参数
	migrateCmd := flag.NewFlagSet("migrate-keys", flag.ExitOnError)
	fromKey := migrateCmd.String("from", "", "源加密密钥（用于解密现有数据）")
	toKey := migrateCmd.String("to", "", "目标加密密钥（用于加密新数据）")

	// 自定义使用说明
	migrateCmd.Usage = func() {
		fmt.Println("GPT-Load 密钥迁移工具")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  启用加密: gpt-load migrate-keys --to new-key")
		fmt.Println("  禁用加密: gpt-load migrate-keys --from old-key")
		fmt.Println("  更换密钥: gpt-load migrate-keys --from old-key --to new-key")
		fmt.Println()
		fmt.Println("参数:")
		migrateCmd.PrintDefaults()
		fmt.Println()
		fmt.Println("⚠️  重要提示:")
		fmt.Println("  1. 迁移前务必备份数据库")
		fmt.Println("  2. 迁移期间请停止服务")
		fmt.Println("  3. 迁移完成后重启服务")
	}

	// 解析参数
	if err := migrateCmd.Parse(args); err != nil {
		logrus.Fatalf("参数解析失败: %v", err)
	}

	// 检查是否需要显示帮助
	if len(args) == 0 || (*fromKey == "" && *toKey == "") {
		migrateCmd.Usage()
		os.Exit(0)
	}

	// 构建依赖注入容器
	cont, err := container.BuildContainer()
	if err != nil {
		logrus.Fatalf("构建容器失败: %v", err)
	}

	// 初始化全局日志
	if err := cont.Invoke(func(configManager types.ConfigManager) {
		utils.SetupLogger(configManager)
	}); err != nil {
		logrus.Fatalf("设置日志失败: %v", err)
	}

	// 执行迁移命令
	if err := cont.Invoke(func(db *gorm.DB, configManager types.ConfigManager, cacheStore store.Store) {
		migrateKeysCmd := NewMigrateKeysCommand(db, configManager, cacheStore, *fromKey, *toKey)
		if err := migrateKeysCmd.Execute(); err != nil {
			logrus.Fatalf("密钥迁移失败: %v", err)
		}
	}); err != nil {
		logrus.Fatalf("执行迁移失败: %v", err)
	}

	logrus.Info("密钥迁移命令完成")
}

// 迁移批处理大小
const migrationBatchSize = 1000

// MigrateKeysCommand 密钥迁移命令
type MigrateKeysCommand struct {
	db            *gorm.DB
	configManager types.ConfigManager
	cacheStore    store.Store
	fromKey       string
	toKey         string
}

// NewMigrateKeysCommand 创建迁移命令实例
func NewMigrateKeysCommand(db *gorm.DB, configManager types.ConfigManager, cacheStore store.Store, fromKey, toKey string) *MigrateKeysCommand {
	return &MigrateKeysCommand{
		db:            db,
		configManager: configManager,
		cacheStore:    cacheStore,
		fromKey:       fromKey,
		toKey:         toKey,
	}
}

// Execute 执行密钥迁移
func (cmd *MigrateKeysCommand) Execute() error {
	db.HandleLegacyIndexes(cmd.db)
	// 预处理：数据库迁移和修复
	if err := cmd.db.AutoMigrate(&models.APIKey{}); err != nil {
		return fmt.Errorf("数据库自动迁移失败: %w", err)
	}

	// 1. 验证参数并获取迁移场景
	scenario, err := cmd.validateAndGetScenario()
	if err != nil {
		return fmt.Errorf("参数验证失败: %w", err)
	}

	logrus.Infof("开始密钥迁移，场景: %s", scenario)

	// 2. 预检查 - 验证当前密钥可解密所有数据
	if err := cmd.preCheck(); err != nil {
		return fmt.Errorf("预检查失败: %w", err)
	}

	// 3. 迁移数据到临时列
	if err := cmd.createBackupTableAndMigrate(); err != nil {
		return fmt.Errorf("数据迁移失败: %w", err)
	}

	// 4. 验证临时列数据完整性
	if err := cmd.verifyTempColumns(); err != nil {
		logrus.Errorf("数据验证失败: %v", err)
		return fmt.Errorf("数据验证失败: %w", err)
	}

	// 5. 原子切换列
	if err := cmd.switchColumns(); err != nil {
		logrus.Errorf("列切换失败: %v", err)
		return fmt.Errorf("列切换失败: %w", err)
	}

	// 6. 清除缓存
	if err := cmd.clearCache(); err != nil {
		logrus.Warnf("缓存清理失败，建议手动重启服务: %v", err)
	}

	// 7. 清理临时表
	if err := cmd.dropTempTable(); err != nil {
		logrus.Warnf("临时表清理失败，可手动删除 temp_migration 表: %v", err)
	}

	logrus.Info("密钥迁移成功完成！")
	logrus.Info("建议重启服务以确保所有缓存数据正确加载")

	return nil
}

// validateAndGetScenario 验证参数并返回迁移场景
func (cmd *MigrateKeysCommand) validateAndGetScenario() (string, error) {
	hasFrom := cmd.fromKey != ""
	hasTo := cmd.toKey != ""

	switch {
	case !hasFrom && hasTo:
		// 启用加密
		utils.ValidatePasswordStrength(cmd.toKey, "新加密密钥")
		return "启用加密", nil
	case hasFrom && !hasTo:
		// 禁用加密
		return "禁用加密", nil
	case hasFrom && hasTo:
		// 更换加密密钥
		if cmd.fromKey == cmd.toKey {
			return "", fmt.Errorf("新旧密钥不能相同")
		}
		utils.ValidatePasswordStrength(cmd.toKey, "新加密密钥")
		return "更换加密密钥", nil
	default:
		return "", fmt.Errorf("必须指定 --from 或 --to 参数，或两者都指定")
	}
}

// preCheck 验证当前数据是否可正确处理
func (cmd *MigrateKeysCommand) preCheck() error {
	logrus.Info("执行预检查...")

	// 关键检查：启用加密时（fromKey为空），确保数据未被加密
	if cmd.fromKey == "" && cmd.toKey != "" {
		if err := cmd.detectIfAlreadyEncrypted(); err != nil {
			return err
		}
	}

	// 根据参数创建当前加密服务
	var currentService encryption.Service
	var err error

	if cmd.fromKey != "" {
		// 使用 fromKey 创建加密服务进行验证
		currentService, err = encryption.NewService(cmd.fromKey)
	} else {
		// 启用加密场景：数据应未加密，使用空服务验证
		currentService, err = encryption.NewService("")
	}

	if err != nil {
		return fmt.Errorf("创建当前加密服务失败: %w", err)
	}

	// 检查数据库中的密钥数量
	var totalCount int64
	if err := cmd.db.Model(&models.APIKey{}).Count(&totalCount).Error; err != nil {
		return fmt.Errorf("获取密钥总数失败: %w", err)
	}

	if totalCount == 0 {
		logrus.Info("数据库中无密钥数据，跳过预检查")
		return nil
	}

	logrus.Infof("开始验证 %d 个密钥...", totalCount)

	// 批量验证所有密钥可正确解密
	offset := 0
	failedCount := 0

	for {
		var keys []models.APIKey
		if err := cmd.db.Order("id").Offset(offset).Limit(migrationBatchSize).Find(&keys).Error; err != nil {
			return fmt.Errorf("获取密钥数据失败: %w", err)
		}

		if len(keys) == 0 {
			break
		}

		for _, key := range keys {
			_, err := currentService.Decrypt(key.KeyValue)
			if err != nil {
				logrus.Errorf("密钥 ID %d 解密失败: %v", key.ID, err)
				failedCount++
			}
		}

		offset += migrationBatchSize
		// 确保不显示超过总数
		actualVerified := offset
		if int64(offset) > totalCount {
			actualVerified = int(totalCount)
		}
		logrus.Infof("已验证 %d/%d 个密钥", actualVerified, totalCount)
	}

	if failedCount > 0 {
		return fmt.Errorf("发现 %d 个密钥无法解密，请检查 --from 参数", failedCount)
	}

	logrus.Info("预检查通过，所有密钥验证成功")
	return nil
}

// detectIfAlreadyEncrypted 检测数据是否已加密，防止重复加密
func (cmd *MigrateKeysCommand) detectIfAlreadyEncrypted() error {
	logrus.Info("检测数据是否已加密...")

	// 抽样检查
	var sampleKeys []models.APIKey
	if err := cmd.db.Limit(20).Where("key_hash IS NOT NULL AND key_hash != ''").Find(&sampleKeys).Error; err != nil {
		return fmt.Errorf("获取样本密钥失败: %w", err)
	}

	if len(sampleKeys) == 0 {
		logrus.Info("数据库中未找到密钥，可安全继续")
		return nil
	}

	// 1. 哈希一致性检查
	// 若数据未加密，key_hash 应等于 SHA256(key_value)
	hashConsistentCount := 0
	noopService, err := encryption.NewService("") // SHA256 服务用于未加密数据
	if err != nil {
		return fmt.Errorf("创建空服务失败: %w", err)
	}

	for _, key := range sampleKeys {
		// 未加密数据：key_hash 应匹配 SHA256(key_value)
		expectedHash := noopService.Hash(key.KeyValue)
		if expectedHash == key.KeyHash {
			hashConsistentCount++
		}
	}

	// 2. 分析结果
	if hashConsistentCount == len(sampleKeys) {
		// 所有哈希匹配 SHA256(key_value) - 数据未加密
		logrus.Info("哈希检查通过：数据似乎未加密（SHA256 哈希匹配）")
		return nil // 可安全继续加密

	}

	if hashConsistentCount == 0 {
		// 无哈希匹配 SHA256(key_value) - 数据已加密！

		// 3. 进一步检查：能否用目标密钥解密？
		if cmd.toKey != "" {
			targetService, err := encryption.NewService(cmd.toKey)
			if err != nil {
				return fmt.Errorf("创建目标加密服务失败: %w", err)
			}

			canDecryptCount := 0
			for _, key := range sampleKeys {
				decrypted, err := targetService.Decrypt(key.KeyValue)
				if err == nil {
					// 验证哈希匹配
					expectedHash := targetService.Hash(decrypted)
					if expectedHash == key.KeyHash {
						canDecryptCount++
					}
				}
			}

			if canDecryptCount > 0 {
				return fmt.Errorf(
					"严重：数据已使用目标密钥加密！%d/%d 个密钥可用目标密钥解密",
					canDecryptCount,
					len(sampleKeys),
				)
			}
		}

		return fmt.Errorf(
			"严重：数据似乎已加密！0/%d 个密钥的 SHA256 哈希匹配（未加密数据应有的状态）",
			len(sampleKeys),
		)
	}

	// 部分匹配 - 数据状态不一致
	return fmt.Errorf(
		"警告：检测到数据状态不一致！%d/%d 个密钥似乎未加密（SHA256 哈希匹配），%d/%d 个密钥似乎已加密（SHA256 哈希不匹配）",
		hashConsistentCount,
		len(sampleKeys),
		len(sampleKeys)-hashConsistentCount,
		len(sampleKeys),
	)
}

// createBackupTableAndMigrate 使用临时表执行迁移
func (cmd *MigrateKeysCommand) createBackupTableAndMigrate() error {
	logrus.Info("开始使用临时表进行密钥迁移...")

	// 1. 创建临时表
	if err := cmd.createTempTable(); err != nil {
		return fmt.Errorf("创建临时表失败: %w", err)
	}

	// 2. 创建新旧加密服务
	oldService, newService, err := cmd.createMigrationServices()
	if err != nil {
		return err
	}

	// 3. 获取待迁移总数
	var totalCount int64
	if err := cmd.db.Model(&models.APIKey{}).Count(&totalCount).Error; err != nil {
		return fmt.Errorf("获取密钥数量失败: %w", err)
	}

	if totalCount == 0 {
		logrus.Info("无需迁移的密钥")
		return nil
	}

	logrus.Infof("开始迁移 %d 个密钥...", totalCount)

	// 4. 分批处理迁移
	processedCount := 0
	lastID := uint(0)

	for {
		var keys []models.APIKey
		// 使用基于 ID 的分页确保结果稳定
		if err := cmd.db.Where("id > ?", lastID).Order("id").Limit(migrationBatchSize).Find(&keys).Error; err != nil {
			return fmt.Errorf("获取密钥数据失败: %w", err)
		}

		if len(keys) == 0 {
			break
		}

		// 处理当前批次到临时表
		if err := cmd.processBatchToTempTable(keys, oldService, newService); err != nil {
			return fmt.Errorf("处理批次数据失败: %w", err)
		}

		processedCount += len(keys)
		lastID = keys[len(keys)-1].ID
		logrus.Infof("已处理 %d/%d 个密钥", processedCount, totalCount)
	}

	logrus.Info("数据迁移到临时表完成")
	return nil
}

// createTempTable 创建迁移临时表
func (cmd *MigrateKeysCommand) createTempTable() error {
	logrus.Info("创建临时迁移表...")

	// 删除已存在的临时表
	if err := cmd.db.Exec("DROP TABLE IF EXISTS temp_migration").Error; err != nil {
		logrus.WithError(err).Warn("删除临时表失败，继续执行")
	}

	dbType := cmd.db.Dialector.Name()
	var createTableSQL string

	// 使用数据库特定语法以获得更好的兼容性
	switch dbType {
	case "mysql":
		createTableSQL = `
			CREATE TABLE temp_migration (
				id BIGINT PRIMARY KEY,
				key_value_new TEXT,
				key_hash_new VARCHAR(255)
			)
		`
	case "postgres":
		createTableSQL = `
			CREATE TABLE temp_migration (
				id BIGINT PRIMARY KEY,
				key_value_new TEXT,
				key_hash_new VARCHAR(255)
			)
		`
	case "sqlite":
		// SQLite 使用 INTEGER 作为主键
		createTableSQL = `
			CREATE TABLE temp_migration (
				id INTEGER PRIMARY KEY,
				key_value_new TEXT,
				key_hash_new VARCHAR(255)
			)
		`
	default:
		// 回退到通用语法
		createTableSQL = `
			CREATE TABLE temp_migration (
				id INTEGER PRIMARY KEY,
				key_value_new TEXT,
				key_hash_new VARCHAR(255)
			)
		`
	}

	// 创建最小结构的临时表
	if err := cmd.db.Exec(createTableSQL).Error; err != nil {
		return fmt.Errorf("创建 temp_migration 表失败: %w", err)
	}

	// 主键已隐式创建索引，无需额外创建

	return nil
}

// dropTempTable 删除临时迁移表
func (cmd *MigrateKeysCommand) dropTempTable() error {
	logrus.Info("删除临时迁移表...")

	if err := cmd.db.Exec("DROP TABLE IF EXISTS temp_migration").Error; err != nil {
		return fmt.Errorf("删除 temp_migration 表失败: %w", err)
	}

	logrus.Info("临时表删除成功")
	return nil
}

// createMigrationServices 创建迁移用的新旧加密服务
func (cmd *MigrateKeysCommand) createMigrationServices() (oldService, newService encryption.Service, err error) {
	// 根据参数创建旧加密服务（用于解密）
	if cmd.fromKey != "" {
		// 使用指定密钥解密
		oldService, err = encryption.NewService(cmd.fromKey)
		if err != nil {
			return nil, nil, fmt.Errorf("创建旧加密服务失败: %w", err)
		}
	} else {
		// 启用加密场景：数据应未加密，使用空服务（空密钥=不加密）
		oldService, err = encryption.NewService("")
		if err != nil {
			return nil, nil, fmt.Errorf("创建源空加密服务失败: %w", err)
		}
	}

	// 根据参数创建新加密服务（用于加密）
	if cmd.toKey != "" {
		// 使用指定密钥加密
		newService, err = encryption.NewService(cmd.toKey)
		if err != nil {
			return nil, nil, fmt.Errorf("创建新加密服务失败: %w", err)
		}
	} else {
		// 禁用加密场景：数据应未加密，使用空服务（空密钥=不加密）
		newService, err = encryption.NewService("")
		if err != nil {
			return nil, nil, fmt.Errorf("创建目标空加密服务失败: %w", err)
		}
	}

	return oldService, newService, nil
}

// processBatchToTempTable 处理一批密钥并写入临时表
func (cmd *MigrateKeysCommand) processBatchToTempTable(keys []models.APIKey, oldService, newService encryption.Service) error {
	// 准备待插入的批量数据
	type TempMigration struct {
		ID          uint   `gorm:"primaryKey"`
		KeyValueNew string `gorm:"column:key_value_new"`
		KeyHashNew  string `gorm:"column:key_hash_new"`
	}

	var tempRecords []TempMigration

	for _, key := range keys {
		// 1. 使用旧服务解密
		decrypted, err := oldService.Decrypt(key.KeyValue)
		if err != nil {
			return fmt.Errorf("密钥 ID %d 解密失败: %w", key.ID, err)
		}

		// 2. 使用新服务加密
		encrypted, err := newService.Encrypt(decrypted)
		if err != nil {
			return fmt.Errorf("密钥 ID %d 加密失败: %w", key.ID, err)
		}

		// 3. 使用新服务生成新哈希
		newHash := newService.Hash(decrypted)

		tempRecords = append(tempRecords, TempMigration{
			ID:          key.ID,
			KeyValueNew: encrypted,
			KeyHashNew:  newHash,
		})
	}

	// 在事务中批量插入临时表
	return cmd.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("temp_migration").Create(&tempRecords).Error; err != nil {
			return fmt.Errorf("批量插入 temp_migration 失败: %w", err)
		}
		return nil
	})
}

// verifyTempColumns 验证临时表数据完整性
func (cmd *MigrateKeysCommand) verifyTempColumns() error {
	logrus.Info("验证临时表数据完整性...")

	// 创建验证用的新加密服务
	var newService encryption.Service
	var err error

	if cmd.toKey != "" {
		newService, err = encryption.NewService(cmd.toKey)
	} else {
		newService, err = encryption.NewService("")
	}

	if err != nil {
		return fmt.Errorf("创建验证加密服务失败: %w", err)
	}

	// 获取总数
	var totalCount int64
	if err := cmd.db.Model(&models.APIKey{}).Count(&totalCount).Error; err != nil {
		return fmt.Errorf("获取密钥数量失败: %w", err)
	}

	if totalCount == 0 {
		return nil
	}

	// 验证临时表已填充
	var migratedCount int64
	if err := cmd.db.Table("temp_migration").Count(&migratedCount).Error; err != nil {
		return fmt.Errorf("统计已迁移密钥失败: %w", err)
	}

	if migratedCount != totalCount {
		return fmt.Errorf("迁移不完整: %d/%d 个密钥已迁移", migratedCount, totalCount)
	}

	// 验证样本密钥可正确解密
	verifiedCount := 0
	for {
		var keys []struct {
			ID          uint
			KeyValueNew string `gorm:"column:key_value_new"`
		}

		if err := cmd.db.Table("temp_migration").Select("id, key_value_new").Order("id").Limit(100).Offset(verifiedCount).Scan(&keys).Error; err != nil {
			return fmt.Errorf("获取验证密钥失败: %w", err)
		}

		if len(keys) == 0 {
			break
		}

		for _, key := range keys {
			_, err := newService.Decrypt(key.KeyValueNew)
			if err != nil {
				return fmt.Errorf("密钥 ID %d 验证失败: 临时列数据无效: %w", key.ID, err)
			}
		}

		verifiedCount += len(keys)
		if verifiedCount >= int(totalCount) || verifiedCount >= 1000 { // 为性能最多验证1000个密钥
			break
		}
	}

	logrus.Infof("成功验证 %d 个密钥", verifiedCount)
	return nil
}

// switchColumns 从临时表原子更新到原始表
func (cmd *MigrateKeysCommand) switchColumns() error {
	logrus.Info("从临时表更新原始表...")

	dbType := cmd.db.Dialector.Name()

	return cmd.db.Transaction(func(tx *gorm.DB) error {
		var updateSQL string

		switch dbType {
		case "mysql":
			// MySQL 使用 JOIN 语法进行跨表 UPDATE
			updateSQL = `
				UPDATE api_keys a
				INNER JOIN temp_migration t ON a.id = t.id
				SET a.key_value = t.key_value_new,
				    a.key_hash = t.key_hash_new
			`

		case "postgres":
			// PostgreSQL 使用 FROM 子句进行跨表 UPDATE
			updateSQL = `
				UPDATE api_keys
				SET key_value = t.key_value_new,
				    key_hash = t.key_hash_new
				FROM temp_migration t
				WHERE api_keys.id = t.id
			`

		case "sqlite":
			// SQLite 使用子查询进行跨表 UPDATE（兼容所有版本）
			updateSQL = `
				UPDATE api_keys
				SET key_value = (SELECT key_value_new FROM temp_migration WHERE temp_migration.id = api_keys.id),
				    key_hash = (SELECT key_hash_new FROM temp_migration WHERE temp_migration.id = api_keys.id)
				WHERE EXISTS (SELECT 1 FROM temp_migration WHERE temp_migration.id = api_keys.id)
			`

		default:
			return fmt.Errorf("不支持的数据库类型: %s", dbType)
		}

		logrus.Infof("执行 %s 跨表 UPDATE...", dbType)
		if err := tx.Exec(updateSQL).Error; err != nil {
			return fmt.Errorf("从 temp_migration 更新 api_keys 失败: %w", err)
		}

		logrus.Info("成功使用迁移数据更新原始表")
		return nil
	})
}

// clearCache 清除缓存
func (cmd *MigrateKeysCommand) clearCache() error {
	logrus.Info("开始缓存清理...")

	if cmd.cacheStore == nil {
		logrus.Info("未配置缓存存储，跳过缓存清理")
		return nil
	}

	logrus.Info("执行缓存清理...")
	if err := cmd.cacheStore.Clear(); err != nil {
		return fmt.Errorf("缓存清理失败: %w", err)
	}

	logrus.Info("缓存清理成功")
	return nil
}

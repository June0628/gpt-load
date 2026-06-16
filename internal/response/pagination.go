package response

import (
	"math"
	"reflect"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	DefaultPageSize = 15
	MaxPageSize     = 1000
)

// Pagination 表示响应中的分页详情
type Pagination struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	TotalItems int64 `json:"total_items,omitempty"`
	TotalPages int   `json:"total_pages,omitempty"`
	HasMore    bool  `json:"has_more,omitempty"`
}

// PaginatedResponse 是所有分页API响应的标准结构
type PaginatedResponse struct {
	Items      any        `json:"items"`
	Pagination Pagination `json:"pagination"`
}

// Paginate 对GORM查询执行分页并返回标准化响应，enableCount参数控制是否执行COUNT(*)查询获取总数
func Paginate(c *gin.Context, query *gorm.DB, dest any, enableCount ...bool) (*PaginatedResponse, error) {
	// 1. 从查询参数获取页码和每页大小
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		page = 1
	}

	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(DefaultPageSize)))
	if err != nil || pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}

	shouldEnableCount := true
	if len(enableCount) > 0 {
		shouldEnableCount = enableCount[0]
	}

	var totalItems int64
	var totalPages int
	var hasMore bool

	// 2. 计算偏移量
	offset := (page - 1) * pageSize

	if shouldEnableCount {
		// 启用时获取总项目数
		if err := query.Count(&totalItems).Error; err != nil {
			return nil, err
		}
		totalPages = int(math.Ceil(float64(totalItems) / float64(pageSize)))

		// 获取当前页数据
		if err := query.Limit(pageSize).Offset(offset).Find(dest).Error; err != nil {
			return nil, err
		}
	} else {
		// 无限滚动模式（无COUNT）：多获取一条记录以检查是否有更多数据
		if err := query.Limit(pageSize + 1).Offset(offset).Find(dest).Error; err != nil {
			return nil, err
		}

		// 检查是否有更多记录
		items := reflect.ValueOf(dest).Elem()
		hasMore = items.Len() > pageSize

		// 如果多获取了记录则裁剪到实际每页大小
		if hasMore {
			trimmedDest := reflect.MakeSlice(items.Type(), pageSize, pageSize)
			// 从dest复制前pageSize个元素到trimmedDest
			reflect.Copy(trimmedDest, items.Slice(0, pageSize))
			// 写回调用方传入的切片（dest 是指向切片的指针）
			reflect.ValueOf(dest).Elem().Set(trimmedDest)
		}
		// 如果hasMore=false，dest已包含恰好pageSize条记录（或最后一页较少）
	}

	// 4. 构建分页响应
	paginatedData := &PaginatedResponse{
		Items: dest,
		Pagination: Pagination{
			Page:       page,
			PageSize:   pageSize,
			TotalItems: totalItems,
			TotalPages: totalPages,
			HasMore:    hasMore,
		},
	}

	return paginatedData, nil
}

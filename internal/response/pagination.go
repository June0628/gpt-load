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

// Pagination represents the pagination details in a response.
type Pagination struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	TotalItems int64 `json:"total_items,omitempty"`
	TotalPages int   `json:"total_pages,omitempty"`
	HasMore    bool  `json:"has_more,omitempty"`
}

// PaginatedResponse is the standard structure for all paginated API responses.
type PaginatedResponse struct {
	Items      any        `json:"items"`
	Pagination Pagination `json:"pagination"`
}

// Paginate performs pagination on a GORM query and returns a standardized response.
// It takes a Gin context, a GORM query builder, and a destination slice for the results.
// The enableCount parameter controls whether to execute a COUNT(*) query to get the total items.
// When enableCount is false, pagination uses has_more for infinite scrolling instead of exact total.
func Paginate(c *gin.Context, query *gorm.DB, dest any, enableCount ...bool) (*PaginatedResponse, error) {
	// 1. Get page and page size from query parameters
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

	// 2. Calculate offset
	offset := (page - 1) * pageSize

	if shouldEnableCount {
		// Get total count of items when enabled
		if err := query.Count(&totalItems).Error; err != nil {
			return nil, err
		}
		totalPages = int(math.Ceil(float64(totalItems) / float64(pageSize)))

		// Retrieve the data for the current page
		if err := query.Limit(pageSize).Offset(offset).Find(dest).Error; err != nil {
			return nil, err
		}
	} else {
		// For infinite scroll without COUNT: fetch one extra record to check if there are more
		if err := query.Limit(pageSize + 1).Offset(offset).Find(dest).Error; err != nil {
			return nil, err
		}

		// Check if there are more records
		items := reflect.ValueOf(dest).Elem()
		hasMore = items.Len() > pageSize

		// Trim to actual page size if we fetched an extra record
		if hasMore {
			trimmedDest := reflect.MakeSlice(items.Type(), pageSize, pageSize).Interface()
			// Copy the first pageSize elements from dest to trimmedDest
			reflect.Copy(reflect.ValueOf(trimmedDest), items.Slice(0, pageSize))
			dest = trimmedDest
		}
		// If hasMore=false, dest already contains exactly pageSize records (or less for last page)
	}

	// 4. Construct the paginated response
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

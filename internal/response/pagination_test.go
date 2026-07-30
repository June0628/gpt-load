package response

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type item struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func setupDB(t *testing.T, count int) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&item{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for i := 1; i <= count; i++ {
		if err := db.Create(&item{Name: string(rune('a' + i%26))}).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return db
}

func ginContext(query string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/items?"+query, nil)
	return c
}

func TestPaginateWithCount(t *testing.T) {
	db := setupDB(t, 25)

	var items []item
	got, err := Paginate(ginContext("page=2&page_size=10"), db.Model(&item{}), &items)
	if err != nil {
		t.Fatalf("Paginate() error: %v", err)
	}

	if len(items) != 10 {
		t.Errorf("len(items) = %d, want 10", len(items))
	}
	if items[0].ID != 11 {
		t.Errorf("first item ID = %d, want 11 (offset applied)", items[0].ID)
	}
	want := Pagination{Page: 2, PageSize: 10, TotalItems: 25, TotalPages: 3}
	if got.Pagination != want {
		t.Errorf("Pagination = %+v, want %+v", got.Pagination, want)
	}
}

func TestPaginateLastPage(t *testing.T) {
	db := setupDB(t, 25)

	var items []item
	got, err := Paginate(ginContext("page=3&page_size=10"), db.Model(&item{}), &items)
	if err != nil {
		t.Fatalf("Paginate() error: %v", err)
	}

	if len(items) != 5 {
		t.Errorf("len(items) = %d, want 5", len(items))
	}
	if got.Pagination.TotalPages != 3 || got.Pagination.HasMore {
		t.Errorf("Pagination = %+v", got.Pagination)
	}
}

func TestPaginateQueryParamDefaults(t *testing.T) {
	db := setupDB(t, 1)

	tests := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"no params", "", 1, DefaultPageSize},
		{"invalid page", "page=abc", 1, DefaultPageSize},
		{"zero page", "page=0", 1, DefaultPageSize},
		{"negative page", "page=-3", 1, DefaultPageSize},
		{"invalid page size", "page_size=abc", 1, DefaultPageSize},
		{"zero page size", "page_size=0", 1, DefaultPageSize},
		{"page size above max is clamped", "page_size=99999", 1, MaxPageSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var items []item
			got, err := Paginate(ginContext(tt.query), db.Model(&item{}), &items)
			if err != nil {
				t.Fatalf("Paginate() error: %v", err)
			}
			if got.Pagination.Page != tt.wantPage || got.Pagination.PageSize != tt.wantPageSize {
				t.Errorf("Pagination = %+v, want page %d size %d", got.Pagination, tt.wantPage, tt.wantPageSize)
			}
		})
	}
}

func TestPaginateWithoutCountSetsHasMore(t *testing.T) {
	db := setupDB(t, 25)

	var items []item
	got, err := Paginate(ginContext("page=1&page_size=10"), db.Model(&item{}), &items, false)
	if err != nil {
		t.Fatalf("Paginate() error: %v", err)
	}

	if len(items) != 10 {
		t.Errorf("len(items) = %d, want 10 (extra probe row trimmed)", len(items))
	}
	if !got.Pagination.HasMore {
		t.Error("HasMore = false, want true")
	}
	if got.Pagination.TotalItems != 0 || got.Pagination.TotalPages != 0 {
		t.Errorf("totals should be zero without count: %+v", got.Pagination)
	}
}

func TestPaginateWithoutCountLastPage(t *testing.T) {
	db := setupDB(t, 20)

	var items []item
	got, err := Paginate(ginContext("page=2&page_size=10"), db.Model(&item{}), &items, false)
	if err != nil {
		t.Fatalf("Paginate() error: %v", err)
	}

	if len(items) != 10 {
		t.Errorf("len(items) = %d, want 10", len(items))
	}
	if got.Pagination.HasMore {
		t.Error("HasMore = true, want false on the last page")
	}
}

func TestPaginateReturnsQueryError(t *testing.T) {
	db := setupDB(t, 1)

	var items []item
	if _, err := Paginate(ginContext(""), db.Table("missing_table"), &items); err == nil {
		t.Error("expected error for a query against a missing table")
	}

	if _, err := Paginate(ginContext(""), db.Table("missing_table"), &items, false); err == nil {
		t.Error("expected error for a countless query against a missing table")
	}
}

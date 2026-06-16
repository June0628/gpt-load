package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"gpt-load/internal/store"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	globalTaskKey = "global_task"
	ResultTTL     = 60 * time.Minute
)

const (
	TaskTypeKeyValidation = "KEY_VALIDATION"
	TaskTypeKeyImport     = "KEY_IMPORT"
	TaskTypeKeyDelete     = "KEY_DELETE"
)

// TaskStatus 表示长时间运行任务的完整生命周期
type TaskStatus struct {
	TaskType        string     `json:"task_type"`
	IsRunning       bool       `json:"is_running"`
	GroupName       string     `json:"group_name,omitempty"`
	Processed       int        `json:"processed"`
	Total           int        `json:"total"`
	Result          any        `json:"result,omitempty"`
	Error           string     `json:"error,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	DurationSeconds float64    `json:"duration_seconds,omitempty"`
}

// TaskService 使用 store 接口管理单个全局长时间运行任务的状态
type TaskService struct {
	store store.Store
}

// NewTaskService 创建新的 TaskService
func NewTaskService(store store.Store) *TaskService {
	return &TaskService{
		store: store,
	}
}

// StartTask 启动新任务，使用 SetNX 保证原子性
func (s *TaskService) StartTask(taskType, groupName string, total int) (*TaskStatus, error) {
	status := &TaskStatus{
		TaskType:  taskType,
		IsRunning: true,
		GroupName: groupName,
		Total:     total,
		Processed: 0,
		StartedAt: time.Now(),
	}
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize new task status: %w", err)
	}

	// 使用 SetNX 原子地设置任务状态，如果已有任务在运行则失败
	ok, err := s.store.SetNX(globalTaskKey, statusBytes, ResultTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to set initial task status: %w", err)
	}
	if !ok {
		return nil, errors.New("a task is already running, please wait")
	}

	return status, nil
}

// GetTaskStatus 返回任务的当前状态
func (s *TaskService) GetTaskStatus() (*TaskStatus, error) {
	statusBytes, err := s.store.Get(globalTaskKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &TaskStatus{IsRunning: false}, nil
		}
		return nil, fmt.Errorf("failed to get task status: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return nil, fmt.Errorf("failed to deserialize task status: %w", err)
	}

	// 如果任务标记为运行中但已超过30分钟，自动标记为失败并清除
	if status.IsRunning && time.Since(status.StartedAt) > 30*time.Minute {
		logrus.Warnf("Task '%s' has been running for over 30 minutes, auto-clearing stuck task", status.TaskType)
		// 使用 EndTask 统一状态流转，记录超时错误
		if err := s.EndTask(nil, fmt.Errorf("task timed out after 30 minutes")); err != nil {
			logrus.WithError(err).Error("Failed to end timed-out task, forcing delete")
			_ = s.store.Delete(globalTaskKey)
		}
		return &TaskStatus{IsRunning: false}, nil
	}

	if !status.IsRunning && status.FinishedAt != nil {
		status.DurationSeconds = status.FinishedAt.Sub(status.StartedAt).Seconds()
	}

	return &status, nil
}

// UpdateProgress 更新当前任务的进度
func (s *TaskService) UpdateProgress(processed int) error {
	status, err := s.GetTaskStatus()
	if err != nil {
		return err
	}
	if !status.IsRunning {
		return nil
	}

	status.Processed = processed
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to serialize updated status: %w", err)
	}

	return s.store.Set(globalTaskKey, statusBytes, ResultTTL)
}

// EndTask 将当前任务标记为已完成并存储最终结果
func (s *TaskService) EndTask(resultData any, taskErr error) error {
	status, err := s.GetTaskStatus()
	if err != nil {
		return fmt.Errorf("failed to get task object to end task: %w", err)
	}
	if !status.IsRunning {
		return nil
	}

	now := time.Now()
	status.IsRunning = false
	status.FinishedAt = &now
	status.DurationSeconds = now.Sub(status.StartedAt).Seconds()
	if taskErr != nil {
		status.Error = taskErr.Error()
	} else {
		status.Result = resultData
	}

	updatedTaskBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to serialize final task status: %w", err)
	}

	return s.store.Set(globalTaskKey, updatedTaskBytes, ResultTTL)
}

// ForceClearTask 强制清除卡住的任务
func (s *TaskService) ForceClearTask() error {
	return s.store.Delete(globalTaskKey)
}

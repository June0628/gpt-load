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
	// globalTaskKey 是正在运行的任务锁，仅在任务运行期间存在。任务完成后会被删除以允许新任务启动。
	globalTaskKey = "global_task"
	// globalTaskResultKey 存储已完成任务的最终结果，保留 ResultTTL 时间供前端轮询查询。
	globalTaskResultKey = "global_task_result"
	// ResultTTL 是任务结果在存储中的保留时长。
	ResultTTL = 60 * time.Minute
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

// TaskService 使用 store 接口管理长时间运行任务的状态。
// 任务锁（globalTaskKey）与任务结果（globalTaskResultKey）分离存储，
// 确保已完成的任务结果不会阻塞新任务启动。
type TaskService struct {
	store store.Store
}

// NewTaskService 创建新的 TaskService
func NewTaskService(store store.Store) *TaskService {
	return &TaskService{
		store: store,
	}
}

// StartTask 启动新任务。
// 使用 SetNX 对 globalTaskKey 原子加锁：仅当没有其他任务正在运行时才成功。
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

	// 使用 SetNX 原子地设置任务锁，如果已有任务在运行则失败
	ok, err := s.store.SetNX(globalTaskKey, statusBytes, ResultTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to set initial task status: %w", err)
	}
	if !ok {
		return nil, errors.New("a task is already running, please wait")
	}

	return status, nil
}

// GetTaskStatus 返回任务的当前状态。
// 优先检查运行中的任务（globalTaskKey），若不存在则返回已完成的结果（globalTaskResultKey）。
func (s *TaskService) GetTaskStatus() (*TaskStatus, error) {
	// 1. 检查运行中的任务
	statusBytes, err := s.store.Get(globalTaskKey)
	if err == nil {
		var status TaskStatus
		if err := json.Unmarshal(statusBytes, &status); err != nil {
			return nil, fmt.Errorf("failed to deserialize running task status: %w", err)
		}

		// 超时保护：运行超过30分钟自动清理
		if status.IsRunning && time.Since(status.StartedAt) > 30*time.Minute {
			logrus.Warnf("Task '%s' has been running for over 30 minutes, auto-clearing stuck task", status.TaskType)
			// 将超时结果写入结果 key 供前端读取
			now := time.Now()
			status.IsRunning = false
			status.FinishedAt = &now
			status.DurationSeconds = now.Sub(status.StartedAt).Seconds()
			status.Error = "task timed out after 30 minutes"
			if resultBytes, mErr := json.Marshal(status); mErr == nil {
				if sErr := s.store.Set(globalTaskResultKey, resultBytes, ResultTTL); sErr != nil {
					logrus.WithError(sErr).Warn("Failed to write timed-out task result to store")
				}
			}
			// 释放运行锁
			if dErr := s.store.Delete(globalTaskKey); dErr != nil {
				logrus.WithError(dErr).Warn("Failed to release running task lock after timeout")
			}
			return &TaskStatus{IsRunning: false}, nil
		}

		return &status, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("failed to get running task status: %w", err)
	}

	// 2. 检查已完成任务的结果
	resultBytes, err := s.store.Get(globalTaskResultKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &TaskStatus{IsRunning: false}, nil
		}
		return nil, fmt.Errorf("failed to get task result: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(resultBytes, &status); err != nil {
		return nil, fmt.Errorf("failed to deserialize task result: %w", err)
	}

	if status.FinishedAt != nil {
		status.DurationSeconds = status.FinishedAt.Sub(status.StartedAt).Seconds()
	}

	return &status, nil
}

// UpdateProgress 更新运行中任务的进度。
func (s *TaskService) UpdateProgress(processed int) error {
	statusBytes, err := s.store.Get(globalTaskKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 任务已结束，忽略进度更新
			return nil
		}
		return fmt.Errorf("failed to get task for progress update: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return fmt.Errorf("failed to deserialize task status: %w", err)
	}
	if !status.IsRunning {
		return nil
	}

	status.Processed = processed
	updatedBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to serialize updated status: %w", err)
	}

	return s.store.Set(globalTaskKey, updatedBytes, ResultTTL)
}

// EndTask 将任务标记为已完成。
// 将最终结果写入 globalTaskResultKey（保留 ResultTTL 供前端查询），然后删除 globalTaskKey 以释放锁。
func (s *TaskService) EndTask(resultData any, taskErr error) error {
	statusBytes, err := s.store.Get(globalTaskKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 任务锁不存在，可能已被超时清理或已结束
			return nil
		}
		return fmt.Errorf("failed to get task to end: %w", err)
	}

	var status TaskStatus
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return fmt.Errorf("failed to deserialize task status: %w", err)
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

	finalBytes, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to serialize final task status: %w", err)
	}

	// 1. 保存结果供前端查询（保留 ResultTTL）
	if err := s.store.Set(globalTaskResultKey, finalBytes, ResultTTL); err != nil {
		return fmt.Errorf("failed to save task result: %w", err)
	}
	// 2. 释放运行锁，允许启动新任务
	if err := s.store.Delete(globalTaskKey); err != nil {
		logrus.WithError(err).Warn("Failed to delete running task lock after completion")
	}

	return nil
}

// ForceClearTask 强制清除所有任务相关的键。
func (s *TaskService) ForceClearTask() error {
	_ = s.store.Delete(globalTaskKey)
	return s.store.Delete(globalTaskResultKey)
}

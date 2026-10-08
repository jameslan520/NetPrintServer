package main

import (
	"fmt"
	"sync"
	"time"
)

// JobRecord 一条打印作业的生命周期记录
type JobRecord struct {
	ID       string    `json:"id"`
	Printer  string    `json:"printer"`
	Doc      string    `json:"doc"`
	Source   string    `json:"source"`
	Protocol string    `json:"protocol"` // RAW / LPD / 本地测试
	Bytes    int64     `json:"bytes"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitempty"`
	State    string    `json:"state"` // 接收中/已提交/打印中/已完成/失败
	Detail   string    `json:"detail,omitempty"`
	WinJobID uint32    `json:"win_job_id,omitempty"`
	tracked  bool
}

const jobStateDone = "已完成"
const jobStateErr = "失败"
const jobStateSpooling = "接收中"
const jobStateSubmitted = "已提交"
const jobStatePrinting = "打印中"

type JobTracker struct {
	mu      sync.Mutex
	records []*JobRecord // 环形缓冲
	max     int
	seq     uint64
}

func NewJobTracker(max int) *JobTracker {
	if max <= 0 {
		max = 200
	}
	return &JobTracker{max: max}
}

func (t *JobTracker) Begin(printer, doc, source, proto string) *JobRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	rec := &JobRecord{
		ID:       fmt.Sprintf("J%06d", t.seq),
		Printer:  printer,
		Doc:      doc,
		Source:   source,
		Protocol: proto,
		Started:  time.Now(),
		State:    jobStateSpooling,
		tracked:  true,
	}
	if len(t.records) < t.max {
		t.records = append(t.records, rec)
	} else {
		// 覆盖最旧的一条
		copy(t.records, t.records[1:])
		t.records[len(t.records)-1] = rec
	}
	return rec
}

func (t *JobTracker) Finish(rec *JobRecord, bytes int64, err error) {
	if rec == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rec.Bytes = bytes
	rec.Ended = time.Now()
	if err != nil {
		rec.State = jobStateErr
		rec.Detail = err.Error()
	} else {
		rec.State = jobStateSubmitted
		rec.Detail = "已提交到 Windows 打印队列"
	}
}

// MarkPrinted 标记为完成（作业从队列消失或文件后端落盘）
func (t *JobTracker) MarkPrinted(rec *JobRecord, detail string) {
	if rec == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec.State == jobStateErr {
		return
	}
	rec.State = jobStateDone
	if detail != "" {
		rec.Detail = detail
	}
	if rec.Ended.IsZero() {
		rec.Ended = time.Now()
	}
}

func (t *JobTracker) SetWinJob(rec *JobRecord, id uint32) {
	if rec == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rec.WinJobID = id
	rec.State = jobStateSubmitted
}

// List 返回最近的作业（新的在前）
func (t *JobTracker) List(limit int) []JobRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]JobRecord, 0, len(t.records))
	for i := len(t.records) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *t.records[i])
	}
	return out
}

// Active 返回仍在队列中的作业记录（Windows 轮询用）
func (t *JobTracker) Active() []*JobRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*JobRecord
	for _, r := range t.records {
		if r.WinJobID != 0 && r.State != jobStateDone && r.State != jobStateErr {
			out = append(out, r)
		}
	}
	return out
}

// UpdateFromQueue 根据 Windows 队列里的作业状态更新记录
func (t *JobTracker) UpdateFromQueue(printer string, jobs map[uint32]uint32, missing map[uint32]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.records {
		if r.Printer != printer || r.WinJobID == 0 {
			continue
		}
		if r.State == jobStateDone || r.State == jobStateErr {
			continue
		}
		if missing[r.WinJobID] {
			r.State = jobStateDone
			r.Detail = "已打印完成"
			if r.Ended.IsZero() {
				r.Ended = time.Now()
			}
			continue
		}
		if st, ok := jobs[r.WinJobID]; ok {
			r.State = describeJobStatus(st)
		}
	}
}

func describeJobStatus(st uint32) string {
	// winuser.h JOB_STATUS_*
	const (
		paused   = 0x00000001
		errorf   = 0x00000002
		deleting = 0x00000004
		spooling = 0x00000008
		printing = 0x00000010
		offline  = 0x00000020
		paperout = 0x00000040
		printed  = 0x00000080
		deleted  = 0x00000100
		blocked  = 0x00000200
		interv   = 0x00000400
		complete = 0x00001000
	)
	switch {
	case st&complete != 0 || st&printed != 0:
		return jobStateDone
	case st&deleted != 0 || st&deleting != 0:
		return "已删除"
	case st&(errorf|blocked|interv) != 0:
		return "打印出错"
	case st&offline != 0:
		return "打印机脱机"
	case st&paperout != 0:
		return "缺纸/耗材"
	case st&printing != 0:
		return jobStatePrinting
	case st&spooling != 0:
		return "后台处理中"
	case st&paused != 0:
		return "已暂停"
	default:
		return "排队中"
	}
}

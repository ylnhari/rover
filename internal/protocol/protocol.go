package protocol

import "time"

type SessionStatus string

const (
	StatusRunning   SessionStatus = "running"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
)

type ExecRequest struct {
	Command string `json:"command"`
	Secret  string `json:"secret"`
}

type StreamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type SessionSummary struct {
	ID        string         `json:"id"`
	Command   string         `json:"command"`
	Status    SessionStatus  `json:"status"`
	StartTime time.Time      `json:"start_time"`
	EndTime   *time.Time     `json:"end_time,omitempty"`
	ExitCode  int            `json:"exit_code"`
}

type SessionDetail struct {
	ID        string         `json:"id"`
	Command   string         `json:"command"`
	Status    SessionStatus  `json:"status"`
	StartTime time.Time      `json:"start_time"`
	EndTime   *time.Time     `json:"end_time,omitempty"`
	ExitCode  int            `json:"exit_code"`
	Stdout    string         `json:"stdout"`
	Stderr    string         `json:"stderr"`
}

type PingResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

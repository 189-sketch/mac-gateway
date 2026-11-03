// Package protocol defines the messages exchanged between gateway and worker
// over the worker's outbound WebSocket connection.
//
// There is deliberately no message that carries a command to execute: the
// gateway names a capability and supplies input, and the worker decides what
// that means using its local manifest.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version is bumped on incompatible protocol changes.
const Version = 1

// Message types.
const (
	TypeHello     = "hello"
	TypeWelcome   = "welcome"
	TypeHeartbeat = "heartbeat"
	TypeHeartAck  = "heartbeat_ack"
	TypeAssign    = "assign"
	TypeEvent     = "event"
	TypeAck       = "ack"
	TypeAbort     = "abort"
	// TypeCapabilityStatus is sent by a worker when what it can run has changed.
	TypeCapabilityStatus = "capability_status"
)

// Job event types, as carried by Event.Type.
const (
	EventStarted   = "started"
	EventProgress  = "progress"
	EventCompleted = "completed"
	EventFailed    = "failed"
)

// Failure codes reported by workers.
const (
	CodeInvalidInput      = "INVALID_INPUT"
	CodeCommandFailed     = "COMMAND_FAILED"
	CodeStartFailed       = "START_FAILED"
	CodeTimedOut          = "TIMED_OUT"
	CodeOutputMissing     = "OUTPUT_MISSING"
	CodeDownloadFailed    = "DOWNLOAD_FAILED"
	CodeUploadFailed      = "UPLOAD_FAILED"
	CodeWorkerRestarted   = "WORKER_RESTARTED"
	CodeUnknownCapability = "UNKNOWN_CAPABILITY"
	// CodeServiceError means an HTTP service refused the job or reported failure.
	CodeServiceError = "SERVICE_ERROR"
	// CodeServiceUnavailable means an HTTP service couldn't be reached or asked
	// to be tried later, so resubmitting may work.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	// CodeBadResponse means an HTTP service answered with something unusable.
	CodeBadResponse = "BAD_RESPONSE"
)

// Envelope frames every WebSocket message.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Encode wraps a payload in an Envelope.
func Encode(msgType string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", msgType, err)
	}
	return json.Marshal(Envelope{Type: msgType, Data: data})
}

// Capability is what a node advertises for one service.
type Capability struct {
	Name           string          `json:"name"`
	InputSchema    json.RawMessage `json:"input_schema"`
	Requires       map[string]int  `json:"requires,omitempty"`
	TimeoutSeconds int64           `json:"timeout_seconds"`
	RetryIfLost    bool            `json:"retry_if_lost"`
	InputFiles     []string        `json:"input_files,omitempty"`
	// NotReady says why the worker's own check found this can't run jobs right now.
	// Empty means ready, which is also how a worker with no checks appears.
	NotReady string `json:"not_ready,omitempty"`
}

// CapabilityStatus is the complete list of a worker's capabilities that aren't
// ready, by name. Any capability it doesn't mention is ready.
type CapabilityStatus struct {
	NotReady map[string]string `json:"not_ready"`
}

// InputFile is a file a worker must download before running a job.
type InputFile struct {
	Field  string `json:"field"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// AttemptState is a worker's report of an attempt it still holds on disk.
type AttemptState struct {
	AttemptID string `json:"attempt_id"`
	LastSeq   int64  `json:"last_seq"`
}

// Hello is the first message a worker sends on every connection.
type Hello struct {
	ProtocolVersion int               `json:"protocol_version"`
	NodeName        string            `json:"node_name"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	Labels          map[string]string `json:"labels,omitempty"`
	Resources       map[string]int    `json:"resources,omitempty"`
	MaxJobs         int               `json:"max_jobs"`
	Capabilities    []Capability      `json:"capabilities"`
	Attempts        []AttemptState    `json:"attempts"`
}

// Welcome is the gateway's reconciled view of the attempts in Hello.
type Welcome struct {
	// Acks gives, per attempt the worker should keep, the highest event
	// sequence the gateway has durably stored.
	Acks map[string]int64 `json:"acks"`
	// Abort lists attempts the worker must stop and discard.
	Abort []string `json:"abort"`
	// HeartbeatMS is how often the worker must send a heartbeat.
	HeartbeatMS int64 `json:"heartbeat_ms"`
}

// Assign asks a worker to run one attempt of a job.
type Assign struct {
	JobID      string          `json:"job_id"`
	AttemptID  string          `json:"attempt_id"`
	Capability string          `json:"capability"`
	Input      json.RawMessage `json:"input"`
	Files      []InputFile     `json:"files,omitempty"`
}

// Event is one numbered, durable fact about an attempt. Seq starts at 1 for
// each attempt and has no gaps.
type Event struct {
	AttemptID string          `json:"attempt_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Ack says every event of an attempt up to and including Seq is stored.
type Ack struct {
	AttemptID string `json:"attempt_id"`
	Seq       int64  `json:"seq"`
}

// Abort tells a worker to kill an attempt and delete its local state.
type Abort struct {
	AttemptID string `json:"attempt_id"`
	Reason    string `json:"reason"`
}

// Progress is the payload of a progress event.
type Progress struct {
	Stage   string   `json:"stage,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
	Current string   `json:"current,omitempty"`
	Total   string   `json:"total,omitempty"`
	Message string   `json:"message,omitempty"`
}

// Failure is the payload of a failed event.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ArtifactRef identifies an uploaded output file in a completed event.
type ArtifactRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Completion is the payload of a completed event.
type Completion struct {
	Artifacts []ArtifactRef `json:"artifacts"`
	// Result is a small inline answer from an HTTP service, if it was asked for.
	Result json.RawMessage `json:"result,omitempty"`
}

// Package protocol 是 userspace 唯一合同：信封、事件种类、命令帧（spec §4）。
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

const SchemaVersion = "1"

type Actor string

const (
	ActorAgent  Actor = "agent"
	ActorUser   Actor = "user"
	ActorTool   Actor = "tool"
	ActorSystem Actor = "system"
)

// Envelope 是所有事件的线上形态。结构永不变，只许增可选字段（§4 版本纪律）。
type Envelope struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"sessionId"`
	RunID         string          `json:"runId,omitempty"` // 所属 run（对账锚点，Arkloop run_events.run_id 同义；run 外事件为空）
	Seq           uint64          `json:"seq"`
	Kind          string          `json:"kind"`
	Actor         Actor           `json:"actor"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	OccurredAt    time.Time       `json:"occurredAt"`
	SchemaVersion string          `json:"schemaVersion"`
	Native        json.RawMessage `json:"native,omitempty"`
}

func NewEventID() string   { return newID("evt_") }
func NewSessionID() string { return newID("ses_") }
func NewRunID() string     { return newID("run_") }
func NewCorrID() string    { return newID("cor_") }

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("protocol: rand failed: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}

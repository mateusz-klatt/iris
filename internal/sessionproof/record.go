// Package sessionproof defines the exact-record model and strict validation for ADR 039.
package sessionproof

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// FormatVersion is the only exact-record schema version accepted by stage 1b.
	FormatVersion = 1
	// RecordReadLimit bounds exact-record reads; records must be strictly smaller than 4096 bytes.
	RecordReadLimit = 4096
)

// Origin identifies whether a hook registration or shim succession created an exact record.
type Origin string

const (
	OriginHook Origin = "hook"
	OriginShim Origin = "shim"
)

// ErrInvalidRecord reports a malformed, oversized, inconsistent, or unsupported exact record.
var ErrInvalidRecord = errors.New("invalid exact session record")

// ErrInvalidExecutionID reports an execution identifier outside the ex_ + 32-hex format.
var ErrInvalidExecutionID = errors.New("invalid execution id")

// FileState classifies the local state of one session evidence file.
type FileState string

const (
	FileAbsent  FileState = "absent"
	FileValid   FileState = "valid"
	FileDamaged FileState = "damaged"
	FileSkipped FileState = "skipped"
)

// Pair identifies one native session's positive generation and Execution ID.
type Pair struct {
	Generation  int    `json:"generation"`
	ExecutionID string `json:"execution_id"`
}

// Validate checks that p holds a positive generation and valid Execution ID.
func (p Pair) Validate() error {
	if p.Generation <= 0 {
		return fmt.Errorf("%w: generation must be positive", ErrInvalidRecord)
	}
	if err := ValidateExecutionID(p.ExecutionID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	return nil
}

// Record is the versioned exact-record JSON persisted in <hash>.execution.
type Record struct {
	Version          int    `json:"version"`
	Origin           Origin `json:"origin"`
	Generation       int    `json:"generation"`
	ExecutionID      string `json:"execution_id"`
	PriorGeneration  int    `json:"prior_generation,omitempty"`
	PriorExecutionID string `json:"prior_execution_id,omitempty"`
}

// Snapshot holds the three local evidence states observed together under the sessions lock.
type Snapshot struct {
	BasePath        string
	Generation      int
	GenerationState FileState
	Record          Record
	RecordState     FileState
	Ended           int
	EndedState      FileState
}

// Pair returns the exact pair recorded by r.
func (r Record) Pair() Pair {
	return Pair{Generation: r.Generation, ExecutionID: r.ExecutionID}
}

// PriorPair returns the predecessor exact pair for a shim-origin record.
func (r Record) PriorPair() (Pair, bool) {
	if r.Origin != OriginShim {
		return Pair{}, false
	}
	return Pair{Generation: r.PriorGeneration, ExecutionID: r.PriorExecutionID}, true
}

// Validate checks every version, origin, generation, and predecessor invariant on r.
func (r Record) Validate() error {
	if r.Version != FormatVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidRecord, r.Version)
	}
	if err := r.Pair().Validate(); err != nil {
		return err
	}
	switch r.Origin {
	case OriginHook:
		if r.PriorGeneration != 0 || r.PriorExecutionID != "" {
			return fmt.Errorf("%w: hook record forbids predecessor fields", ErrInvalidRecord)
		}
		return nil
	case OriginShim:
		if r.Generation < 2 {
			return fmt.Errorf("%w: shim record requires generation >= 2", ErrInvalidRecord)
		}
		if r.PriorGeneration != r.Generation-1 {
			return fmt.Errorf("%w: shim prior_generation %d must equal generation-1 (%d)", ErrInvalidRecord, r.PriorGeneration, r.Generation-1)
		}
		if err := ValidateExecutionID(r.PriorExecutionID); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
		}
		if r.PriorExecutionID == r.ExecutionID {
			return fmt.Errorf("%w: shim prior_execution_id must differ from execution_id", ErrInvalidRecord)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown origin %q", ErrInvalidRecord, r.Origin)
	}
}

// ValidateExecutionID validates the hub's ex_ + 32 lowercase hex execution identifier format.
func ValidateExecutionID(id string) error {
	const prefix = "ex_"
	if len(id) != len(prefix)+32 || id[:len(prefix)] != prefix {
		return fmt.Errorf("%w: expected %s plus 32 lowercase hex characters", ErrInvalidExecutionID, prefix)
	}
	for i := len(prefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: non-hex byte at index %d", ErrInvalidExecutionID, i)
		}
	}
	return nil
}

// EncodeRecord validates r and marshals it as canonical JSON strictly under RecordReadLimit.
func EncodeRecord(r Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(r)
	return raw, nil
}

// DecodeRecord strictly parses and validates raw exact-record JSON under ADR 039 §11 B rules.
func DecodeRecord(raw []byte) (Record, error) {
	if len(raw) >= RecordReadLimit {
		return Record{}, fmt.Errorf("%w: size %d reaches limit %d", ErrInvalidRecord, len(raw), RecordReadLimit)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return Record{}, fmt.Errorf("%w: expected JSON object", ErrInvalidRecord)
	}

	seen := make(map[string]bool, 6)
	var rec Record
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			return Record{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
		}
		key := kTok.(string)
		if seen[key] {
			return Record{}, fmt.Errorf("%w: duplicate key %q", ErrInvalidRecord, key)
		}
		seen[key] = true
		if err := decodeRecordField(dec, &rec, key); err != nil {
			return Record{}, err
		}
	}
	// dec.More() returned false only after verifying the closing '}' token.
	_, _ = dec.Token()
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, fmt.Errorf("%w: trailing content", ErrInvalidRecord)
	}
	if err := checkRecordKeys(seen, rec.Origin); err != nil {
		return Record{}, err
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func decodeRecordField(dec *json.Decoder, rec *Record, key string) error {
	var err error
	switch key {
	case "version":
		err = dec.Decode(&rec.Version)
	case "origin":
		err = dec.Decode(&rec.Origin)
	case "generation":
		err = dec.Decode(&rec.Generation)
	case "execution_id":
		err = dec.Decode(&rec.ExecutionID)
	case "prior_generation":
		err = dec.Decode(&rec.PriorGeneration)
	case "prior_execution_id":
		err = dec.Decode(&rec.PriorExecutionID)
	default:
		return fmt.Errorf("%w: unknown key %q", ErrInvalidRecord, key)
	}
	if err != nil {
		return fmt.Errorf("%w: field %s: %w", ErrInvalidRecord, key, err)
	}
	return nil
}

func checkRecordKeys(seen map[string]bool, origin Origin) error {
	for _, required := range []string{"version", "origin", "generation", "execution_id"} {
		if !seen[required] {
			return fmt.Errorf("%w: missing required key %q", ErrInvalidRecord, required)
		}
	}
	switch origin {
	case OriginHook:
		if seen["prior_generation"] || seen["prior_execution_id"] {
			return fmt.Errorf("%w: hook record forbids predecessor keys", ErrInvalidRecord)
		}
	case OriginShim:
		if !seen["prior_generation"] || !seen["prior_execution_id"] {
			return fmt.Errorf("%w: shim record requires predecessor keys", ErrInvalidRecord)
		}
	}
	return nil
}

// ExactPair returns the valid exact pair when RecordState is FileValid.
func (s Snapshot) ExactPair() (Pair, bool) {
	if s.RecordState != FileValid {
		return Pair{}, false
	}
	return s.Record.Pair(), true
}

// GenerationFloor returns the maximum valid local generation observed in s.
func (s Snapshot) GenerationFloor() int {
	floor := 0
	if s.GenerationState == FileValid && s.Generation > floor {
		floor = s.Generation
	}
	if s.RecordState == FileValid && s.Record.Generation > floor {
		floor = s.Record.Generation
	}
	return floor
}

// AnyDamaged reports whether any inspected local evidence file in s is damaged.
func (s Snapshot) AnyDamaged() bool {
	return s.GenerationState == FileDamaged || s.RecordState == FileDamaged || s.EndedState == FileDamaged
}

// DamagedPaths returns the paths of any damaged files in s in deterministic order.
func (s Snapshot) DamagedPaths() []string {
	var paths []string
	if s.GenerationState == FileDamaged {
		paths = append(paths, s.BasePath)
	}
	if s.RecordState == FileDamaged {
		paths = append(paths, s.BasePath+".execution")
	}
	if s.EndedState == FileDamaged {
		paths = append(paths, s.BasePath+".ended")
	}
	return paths
}

// Package entry defines the hypergraphia log format: one transaction per line,
// "<ULID> <json>". The ULID prefix is the entry's transaction time and makes a
// lexicographic sort chronological. Lines are kept byte-verbatim once written;
// they are never re-serialized.
package entry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Version is the log format version this build writes and understands.
const Version = 1

// Op is the kind of transaction an entry records.
type Op string

const (
	OpRemember Op = "remember"
	OpRetract  Op = "retract"
	OpPurge    Op = "purge"
	OpSynonym  Op = "synonym"
)

// Kinds a memory may have.
var Kinds = []string{"fact", "decision", "gotcha", "convention", "howto"}

// Limits.
const (
	MaxLine       = 4096
	MaxBody       = 2048
	MaxTags       = 8
	MaxTag        = 64
	MaxAnchors    = 16
	MaxAnchor     = 256
	MaxSupersedes = 8
	MaxReason     = 512
	MinTerms      = 2
	MaxTerms      = 16
	MaxTerm       = 64
)

// Author is who wrote an entry. It is a claim made by the writer, not an
// authenticated identity.
type Author struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Agent string `json:"agent,omitempty"`
}

// Entry is one log transaction. Field order is the serialization order.
type Entry struct {
	ID ulid.ULID `json:"-"`

	V  int    `json:"v"`
	Op Op     `json:"op"`
	By Author `json:"by"`

	// remember
	Kind       string   `json:"kind,omitempty"`
	Body       string   `json:"body,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Anchors    []string `json:"anchors,omitempty"`
	Supersedes []string `json:"supersedes,omitempty"`

	// retract, purge
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`

	// synonym
	Terms []string `json:"terms,omitempty"`

	// Raw is the exact line this entry was parsed from or formatted to,
	// without the trailing newline.
	Raw string `json:"-"`
}

// Time is the entry's transaction time.
func (e *Entry) Time() time.Time { return ulid.Time(e.ID.Time()) }

// Short returns the 12-character display form of a ULID string.
func Short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Errors returned by Parse and Validate. ErrUnknownVersion and ErrUnknownOp
// mark lines written by a newer client: skip them, but don't treat them as
// corrupt.
var (
	ErrUnknownVersion = errors.New("unknown format version")
	ErrUnknownOp      = errors.New("unknown op")
)

// InvalidError describes why an entry failed validation.
type InvalidError struct {
	Field  string
	Reason string
}

func (e *InvalidError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason) }

func invalid(field, format string, a ...any) error {
	return &InvalidError{Field: field, Reason: fmt.Sprintf(format, a...)}
}

// Format serializes e (whose ID must be set) to a log line and records it in
// e.Raw.
func Format(e *Entry) (string, error) {
	if e.ID == (ulid.ULID{}) {
		return "", invalid("id", "missing")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return "", err
	}
	line := e.ID.String() + " " + strings.TrimSuffix(buf.String(), "\n")
	if len(line) > MaxLine {
		return "", invalid("line", "%d bytes exceeds %d", len(line), MaxLine)
	}
	e.Raw = line
	return line, nil
}

// Parse decodes and validates one log line. Lines from other clones are
// untrusted: anything that fails validation must not be indexed or shown.
func Parse(line string) (*Entry, error) {
	if len(line) > MaxLine {
		return nil, invalid("line", "%d bytes exceeds %d", len(line), MaxLine)
	}
	idStr, rest, ok := strings.Cut(line, " ")
	if !ok {
		return nil, invalid("line", "missing separator")
	}
	id, err := ulid.ParseStrict(idStr)
	if err != nil {
		return nil, invalid("id", "%v", err)
	}
	e := &Entry{}
	if err := json.Unmarshal([]byte(rest), e); err != nil {
		return nil, invalid("json", "%v", err)
	}
	e.ID = id
	e.Raw = line
	if err := Validate(e); err != nil {
		return nil, err
	}
	return e, nil
}

// earliest is the lower bound for plausible entry timestamps.
var earliest = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// Validate checks e against the format rules. Text fields must already be
// clean (see Sanitize); unclean text is rejected rather than repaired, because
// a well-behaved writer always sanitizes before writing.
func Validate(e *Entry) error {
	t := e.Time()
	if t.Before(earliest) || t.After(time.Now().AddDate(1, 0, 0)) {
		return invalid("id", "implausible timestamp %s", t.UTC().Format(time.RFC3339))
	}
	if e.V != Version {
		return fmt.Errorf("%w: %d", ErrUnknownVersion, e.V)
	}
	if err := checkText("by.email", e.By.Email, 254, false); err != nil {
		return err
	}
	if err := checkText("by.name", e.By.Name, 128, false); err != nil {
		return err
	}
	if err := checkText("by.agent", e.By.Agent, 64, false); err != nil {
		return err
	}
	switch e.Op {
	case OpRemember:
		return validateRemember(e)
	case OpRetract:
		if err := checkID("target", e.Target); err != nil {
			return err
		}
		if e.Reason == "" {
			return invalid("reason", "required")
		}
		return checkText("reason", e.Reason, MaxReason, false)
	case OpPurge:
		if e.Reason != "" {
			return invalid("reason", "purge entries carry no reason")
		}
		return checkID("target", e.Target)
	case OpSynonym:
		if len(e.Terms) < MinTerms || len(e.Terms) > MaxTerms {
			return invalid("terms", "need %d-%d terms, got %d", MinTerms, MaxTerms, len(e.Terms))
		}
		for _, term := range e.Terms {
			if term == "" || strings.ContainsAny(term, " \t\n") || strings.ToLower(term) != term {
				return invalid("terms", "%q must be a single lowercase word", term)
			}
			if err := checkText("terms", term, MaxTerm, false); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownOp, e.Op)
	}
}

func validateRemember(e *Entry) error {
	if !validKind(e.Kind) {
		return invalid("kind", "%q is not one of %s", e.Kind, strings.Join(Kinds, ", "))
	}
	if strings.TrimSpace(e.Body) == "" {
		return invalid("body", "empty")
	}
	if err := checkText("body", e.Body, MaxBody, true); err != nil {
		return err
	}
	if len(e.Tags) > MaxTags {
		return invalid("tags", "%d exceeds %d", len(e.Tags), MaxTags)
	}
	for _, tag := range e.Tags {
		if tag == "" {
			return invalid("tags", "empty tag")
		}
		if err := checkText("tags", tag, MaxTag, false); err != nil {
			return err
		}
	}
	if len(e.Anchors) > MaxAnchors {
		return invalid("anchors", "%d exceeds %d", len(e.Anchors), MaxAnchors)
	}
	for _, a := range e.Anchors {
		if a == "" {
			return invalid("anchors", "empty anchor")
		}
		if err := checkText("anchors", a, MaxAnchor, false); err != nil {
			return err
		}
	}
	if len(e.Supersedes) > MaxSupersedes {
		return invalid("supersedes", "%d exceeds %d", len(e.Supersedes), MaxSupersedes)
	}
	for _, s := range e.Supersedes {
		if err := checkID("supersedes", s); err != nil {
			return err
		}
		if s == e.ID.String() {
			return invalid("supersedes", "a memory cannot supersede itself")
		}
	}
	if e.Target != "" || e.Reason != "" || len(e.Terms) > 0 {
		return invalid("remember", "unexpected retract/synonym fields")
	}
	return nil
}

func validKind(k string) bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

func checkID(field, s string) error {
	if _, err := ulid.ParseStrict(s); err != nil {
		return invalid(field, "%q is not a ULID", s)
	}
	if strings.ToUpper(s) != s {
		return invalid(field, "%q is not canonical (uppercase)", s)
	}
	return nil
}

func checkText(field, s string, max int, multiline bool) error {
	if len(s) > max {
		return invalid(field, "%d bytes exceeds %d", len(s), max)
	}
	if Sanitize(s, multiline) != s {
		return invalid(field, "contains control, bidi, zero-width or non-NFKC characters")
	}
	return nil
}

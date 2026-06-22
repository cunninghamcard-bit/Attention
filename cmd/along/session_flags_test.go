package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/session"
)

// stubLister returns canned metadata; the scoped (CWD-filtered) list and the
// full list are tracked separately so id lookups across cwds are exercisable.
type stubLister struct {
	scoped map[string][]session.Metadata
	all    []session.Metadata
	err    error
}

func (s stubLister) List(ctx context.Context, opts ...session.JsonlSessionListOptions) ([]session.Metadata, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(opts) == 1 && opts[0].CWD != "" {
		return s.scoped[opts[0].CWD], nil
	}
	return s.all, nil
}

func meta(id, cwd string, modified time.Time) session.Metadata {
	return session.Metadata{
		ID:       id,
		CWD:      cwd,
		Path:     filepath.Join("/sessions", id+".jsonl"),
		Modified: modified,
	}
}

func TestValidateSessionFlags(t *testing.T) {
	tests := []struct {
		name    string
		flags   sessionFlags
		wantErr string // substring; empty => no error
	}{
		{name: "empty ok"},
		{name: "session only", flags: sessionFlags{session: "abc"}},
		{name: "continue only", flags: sessionFlags{cont: true}},
		{name: "resume only", flags: sessionFlags{resume: true}},
		{name: "session-id only", flags: sessionFlags{sessionID: "id"}},
		{name: "no-session only", flags: sessionFlags{noSession: true}},
		{
			name:    "session-id + session",
			flags:   sessionFlags{sessionID: "id", session: "abc"},
			wantErr: "--session-id cannot be combined",
		},
		{
			name:    "session-id + continue",
			flags:   sessionFlags{sessionID: "id", cont: true},
			wantErr: "--session-id cannot be combined",
		},
		{
			name:    "session-id + resume",
			flags:   sessionFlags{sessionID: "id", resume: true},
			wantErr: "--session-id cannot be combined",
		},
		{
			name:    "session-id + no-session",
			flags:   sessionFlags{sessionID: "id", noSession: true},
			wantErr: "--session-id cannot be combined",
		},
		{
			name:    "no-session + continue",
			flags:   sessionFlags{noSession: true, cont: true},
			wantErr: "--no-session cannot be combined",
		},
		{
			name:    "no-session + session",
			flags:   sessionFlags{noSession: true, session: "abc"},
			wantErr: "--no-session cannot be combined",
		},
		{
			name:    "session + continue mutually exclusive",
			flags:   sessionFlags{session: "abc", cont: true},
			wantErr: "mutually exclusive",
		},
		{
			name:    "continue + resume mutually exclusive",
			flags:   sessionFlags{cont: true, resume: true},
			wantErr: "mutually exclusive",
		},
		{name: "fork only", flags: sessionFlags{fork: "abc"}},
		{
			name:    "fork + session",
			flags:   sessionFlags{fork: "abc", session: "def"},
			wantErr: "--fork cannot be combined",
		},
		{
			name:    "fork + continue",
			flags:   sessionFlags{fork: "abc", cont: true},
			wantErr: "--fork cannot be combined",
		},
		{
			name:    "fork + resume",
			flags:   sessionFlags{fork: "abc", resume: true},
			wantErr: "--fork cannot be combined",
		},
		{
			name:    "fork + session-id",
			flags:   sessionFlags{fork: "abc", sessionID: "id"},
			wantErr: "--fork cannot be combined",
		},
		{
			name:    "fork + no-session",
			flags:   sessionFlags{fork: "abc", noSession: true},
			wantErr: "--fork cannot be combined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionFlags(tt.flags)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestMostRecentSession(t *testing.T) {
	now := time.Now()
	cwd := "/work"
	lister := stubLister{
		scoped: map[string][]session.Metadata{
			cwd: {
				meta("old", cwd, now.Add(-2*time.Hour)),
				meta("newest", cwd, now),
				meta("mid", cwd, now.Add(-1*time.Hour)),
			},
		},
	}
	got, err := mostRecentSession(context.Background(), lister, cwd)
	if err != nil {
		t.Fatalf("mostRecentSession: %v", err)
	}
	if got.ID != "newest" {
		t.Fatalf("id = %q, want newest", got.ID)
	}
}

func TestMostRecentSessionEmpty(t *testing.T) {
	lister := stubLister{scoped: map[string][]session.Metadata{}}
	_, err := mostRecentSession(context.Background(), lister, "/work")
	if err == nil || !strings.Contains(err.Error(), "no session to continue") {
		t.Fatalf("error = %v, want 'no session to continue'", err)
	}
}

func TestMatchSessionIDExact(t *testing.T) {
	sessions := []session.Metadata{
		meta("abc123", "/w", time.Now()),
		meta("def456", "/w", time.Now()),
	}
	got, ok, err := matchSessionID(sessions, "def456", false)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.ID != "def456" {
		t.Fatalf("id = %q, want def456", got.ID)
	}
}

func TestMatchSessionIDPrefixUnambiguous(t *testing.T) {
	sessions := []session.Metadata{
		meta("abc123", "/w", time.Now()),
		meta("def456", "/w", time.Now()),
	}
	got, ok, err := matchSessionID(sessions, "abc", true)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.ID != "abc123" {
		t.Fatalf("id = %q, want abc123", got.ID)
	}
}

func TestMatchSessionIDPrefixAmbiguous(t *testing.T) {
	sessions := []session.Metadata{
		meta("abc111", "/w", time.Now()),
		meta("abc222", "/w", time.Now()),
	}
	_, _, err := matchSessionID(sessions, "abc", true)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous", err)
	}
}

func TestMatchSessionIDPrefixDisallowed(t *testing.T) {
	sessions := []session.Metadata{meta("abc123", "/w", time.Now())}
	// allowPrefix=false: a non-exact value must not match.
	_, ok, err := matchSessionID(sessions, "abc", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected no match for prefix when allowPrefix=false")
	}
}

func TestResolveSessionPlanNoFlags(t *testing.T) {
	lister := stubLister{}
	plan, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planNew {
		t.Fatalf("kind = %v, want planNew", plan.kind)
	}
	if plan.createOptions.CWD != "/work" || plan.createOptions.ID != "" {
		t.Fatalf("createOptions = %+v", plan.createOptions)
	}
}

func TestResolveSessionPlanNoSession(t *testing.T) {
	lister := stubLister{}
	plan, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{noSession: true})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planEphemeral {
		t.Fatalf("kind = %v, want planEphemeral", plan.kind)
	}
	if plan.ephemeral == nil {
		t.Fatal("expected an ephemeral session")
	}
	if got := plan.ephemeral.GetMetadata().CWD; got != "/work" {
		t.Fatalf("ephemeral cwd = %q, want /work", got)
	}
}

func TestResolveSessionPlanSessionIDExistingOpens(t *testing.T) {
	cwd := "/work"
	existing := meta("sid", cwd, time.Now())
	lister := stubLister{scoped: map[string][]session.Metadata{cwd: {existing}}}
	plan, err := resolveSessionPlan(context.Background(), lister, cwd, sessionFlags{sessionID: "sid"})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planOpen {
		t.Fatalf("kind = %v, want planOpen", plan.kind)
	}
	if plan.metadata.ID != "sid" {
		t.Fatalf("metadata id = %q, want sid", plan.metadata.ID)
	}
}

func TestResolveSessionPlanSessionIDMissingCreates(t *testing.T) {
	lister := stubLister{}
	plan, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{sessionID: "newid"})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planNew {
		t.Fatalf("kind = %v, want planNew", plan.kind)
	}
	if plan.createOptions.ID != "newid" || plan.createOptions.CWD != "/work" {
		t.Fatalf("createOptions = %+v, want ID=newid CWD=/work", plan.createOptions)
	}
}

func TestResolveSessionPlanContinue(t *testing.T) {
	cwd := "/work"
	now := time.Now()
	lister := stubLister{scoped: map[string][]session.Metadata{
		cwd: {
			meta("old", cwd, now.Add(-time.Hour)),
			meta("new", cwd, now),
		},
	}}
	plan, err := resolveSessionPlan(context.Background(), lister, cwd, sessionFlags{cont: true})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planOpen || plan.metadata.ID != "new" {
		t.Fatalf("plan = %+v, want planOpen id=new", plan)
	}
	// The effective cwd of a resumed session is the recorded cwd.
	if plan.cwd != cwd {
		t.Fatalf("plan cwd = %q, want %q", plan.cwd, cwd)
	}
}

func TestResolveSessionPlanResumeDegradesToMostRecent(t *testing.T) {
	cwd := "/work"
	now := time.Now()
	lister := stubLister{scoped: map[string][]session.Metadata{
		cwd: {
			meta("a", cwd, now.Add(-time.Hour)),
			meta("b", cwd, now),
		},
	}}
	plan, err := resolveSessionPlan(context.Background(), lister, cwd, sessionFlags{resume: true})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planOpen || plan.metadata.ID != "b" {
		t.Fatalf("plan = %+v, want planOpen id=b", plan)
	}
}

func TestResolveSessionPlanSessionByID(t *testing.T) {
	cwd := "/work"
	target := meta("abc123", cwd, time.Now())
	lister := stubLister{scoped: map[string][]session.Metadata{cwd: {target}}}
	// --session with an id value (not a file path) opens the matching session.
	plan, err := resolveSessionPlan(context.Background(), lister, cwd, sessionFlags{session: "abc"})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planOpen || plan.metadata.ID != "abc123" {
		t.Fatalf("plan = %+v, want planOpen id=abc123", plan)
	}
}

func TestResolveSessionPlanSessionByPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "explicit.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lister := stubLister{}
	plan, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{session: path})
	if err != nil {
		t.Fatalf("resolveSessionPlan: %v", err)
	}
	if plan.kind != planOpen {
		t.Fatalf("kind = %v, want planOpen", plan.kind)
	}
	if plan.metadata.Path != path {
		t.Fatalf("metadata path = %q, want %q", plan.metadata.Path, path)
	}
}

func TestResolveSessionPlanSessionUnknown(t *testing.T) {
	lister := stubLister{}
	_, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{session: "nope"})
	if err == nil || !strings.Contains(err.Error(), "no session found matching") {
		t.Fatalf("error = %v, want 'no session found matching'", err)
	}
}

func TestResolveSessionPlanListError(t *testing.T) {
	wantErr := errors.New("list boom")
	lister := stubLister{err: wantErr}
	_, err := resolveSessionPlan(context.Background(), lister, "/work", sessionFlags{cont: true})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

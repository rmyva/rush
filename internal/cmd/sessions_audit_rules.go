package cmd

// The readers and normalizers the rules in sessions_audit_findings.go count
// with: one query over the selected sessions' messages and the per-part
// folding in between.
//
// Both queries read only the columns a rule needs, and never SELECT *: the
// messages of a live session can carry very large parts, and an audit that
// pulls a column no rule looks at pays for it on every row.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds on the evidence a finding carries. A command is cut at 110 bytes
// (long enough to recognize, short enough for one report line); a tool error
// is normalized into at most 160 bytes and its class is the first 60 of those.
const (
	auditCommandMaxLen = 110
	auditErrorTextLen  = 160
	auditErrorClassLen = 60
)

// The notice kinds the notices rule counts: a supervision loop (the session
// is woken over and over by its own supervisor) and a wake that failed after
// the notice had already been persisted.
const (
	auditNoticeSupervision = "supervision"
	auditNoticeWakeFailed  = "wake_failed"
)

// auditDigits folds every run of digits, so `sleep 5` and `sleep 42`
// collapse onto one key.
var auditDigits = regexp.MustCompile(`[0-9]+`)

// auditNormalizeCommand folds a command's numbers and cuts it to the length a
// finding carries.
func auditNormalizeCommand(command string) string {
	return auditCut(auditDigits.ReplaceAllString(command, "N"), auditCommandMaxLen)
}

// auditIsWaitOnly reports whether an already digit-normalized command does
// nothing but wait. The command must not be length-cut yet: the patterns in
// auditWaitOnlyPatterns are matched against the whole line.
func auditIsWaitOnly(normalized string) bool {
	for _, pattern := range auditWaitOnlyPatterns {
		if pattern.MatchString(normalized) {
			return true
		}
	}
	return false
}

// auditNormalizeError turns a failed tool result's content into the class it
// belongs to: numbers become "N" (line numbers, counts, sizes, ids), the text
// is cut to auditErrorTextLen and the class is its first auditErrorClassLen
// bytes. Paths and quotes are deliberately left alone — they are the part an
// operator recognizes.
func auditNormalizeError(content string) string {
	normalized := auditDigits.ReplaceAllString(content, "N")
	return auditCut(auditCut(normalized, auditErrorTextLen), auditErrorClassLen)
}

// auditCut truncates s to at most max bytes without splitting a UTF-8 rune:
// an error class that ends mid-rune would render as garbage and never match
// the class the same error produces on the next run.
func auditCut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// auditPlaceholders renders "? , ? , ?" for an IN (...) clause of n ids.
func auditPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// auditPartEnvelope is the on-disk shape of one content part, exactly as
// internal/message/parts_codec.go writes it: {"type": "...", "data": {...}}.
type auditPartEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// auditToolCallPart and auditToolResultPart mirror the JSON fields of
// message.ToolCall and message.ToolResult that a rule reads. Only these are
// declared: a stricter shape would reject rows the rest of Rush still reads.
type auditToolCallPart struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

type auditToolResultPart struct {
	Name    string `json:"name"`
	IsError bool   `json:"is_error"`
	Content string `json:"content"`
}

// Part types the rules look at; the rest (text, reasoning, finish, ...) carry
// no suspicion.
const (
	auditPartToolCall   = "tool_call"
	auditPartToolResult = "tool_result"
)

// auditQueryChunk caps the IN (...) list of one statement. SQLite's variable
// limit is build-dependent (999 on old builds, 32766 on current ones), and a
// --all over thousands of sessions must not bet on either; 900 sits below the
// smallest of them.
const auditQueryChunk = 900

// auditScanMessages reads the selected sessions' messages and folds every row
// into tally, in chunks of auditQueryChunk ids per statement. The rows' order
// is deliberately irrelevant: every rule is a pure per-session counter, so
// the query carries no ORDER BY and never pays a temp-B-tree sort of the
// whole result.
func auditScanMessages(ctx context.Context, db *sql.DB, schema auditSchema, ids []string, since time.Duration, tally *auditTally) error {
	for start := 0; start < len(ids); start += auditQueryChunk {
		end := start + auditQueryChunk
		if end > len(ids) {
			end = len(ids)
		}
		if err := auditScanMessageChunk(ctx, db, schema, ids[start:end], since, tally); err != nil {
			return err
		}
	}
	return nil
}

// auditScanMessageChunk scans one IN-chunk of sessions' messages.
func auditScanMessageChunk(ctx context.Context, db *sql.DB, schema auditSchema, ids []string, since time.Duration, tally *auditTally) error {
	query, params := auditMessagesQuery(schema, ids, since)
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		return fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			sessionID string
			parts     sql.NullString
			createdAt int64
			notice    sql.NullString
		)
		dest := []any{&sessionID, &parts, &createdAt}
		if schema.MessagesNoticeKind {
			dest = append(dest, &notice)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan message: %w", err)
		}
		tally.applyMessage(sessionID, parts.String, notice.String)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate messages: %w", err)
	}
	return nil
}

// auditMessagesQuery builds the one query that feeds every message rule. The
// notice_kind column is selected only where the schema has it: a database too
// old for it is audited without the notices rule instead of failing.
func auditMessagesQuery(schema auditSchema, ids []string, since time.Duration) (string, []any) {
	columns := []string{"session_id", "parts", "created_at"}
	if schema.MessagesNoticeKind {
		columns = append(columns, "notice_kind")
	}

	query := "SELECT " + strings.Join(columns, ", ") +
		" FROM messages WHERE session_id IN (" + auditPlaceholders(len(ids)) + ")"
	params := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		params = append(params, id)
	}
	// --since is a window over the message's own timestamp, which rush.db
	// stores in Unix SECONDS.
	if since > 0 {
		query += " AND created_at >= ?"
		params = append(params, time.Now().Add(-since).Unix())
	}
	// No ORDER BY: the rules are order-independent counters, and a scan of a
	// large database must not pay a temp-B-tree sort of its whole result.
	return query, params
}

// applyMessage folds one message row into the tally. A message whose parts
// cannot be read contributes nothing and is counted, not reported.
func (tally *auditTally) applyMessage(sessionID, parts, noticeKind string) {
	if noticeKind == auditNoticeSupervision || noticeKind == auditNoticeWakeFailed {
		tally.count(tally.notices, sessionID, noticeKind)
	}
	if strings.TrimSpace(parts) == "" {
		return
	}

	var envelopes []auditPartEnvelope
	if err := json.Unmarshal([]byte(parts), &envelopes); err != nil {
		tally.parseFailures++
		return
	}
	for _, envelope := range envelopes {
		switch envelope.Type {
		case auditPartToolCall:
			tally.applyToolCall(sessionID, envelope.Data)
		case auditPartToolResult:
			tally.applyToolResult(sessionID, envelope.Data)
		}
	}
}

// applyToolCall folds one tool call: a view feeds repeated-view, a bash
// command feeds repeated_command or wait_only. The tool's input is a JSON
// document carried as a STRING inside the part's own JSON.
func (tally *auditTally) applyToolCall(sessionID string, data json.RawMessage) {
	var call auditToolCallPart
	if err := json.Unmarshal(data, &call); err != nil {
		tally.parseFailures++
		return
	}

	switch call.Name {
	case "view":
		if path, ok := tally.toolInput(call.Input, "file_path"); ok && path != "" {
			tally.count(tally.fileViews, sessionID, path)
		}
	case "bash":
		command, ok := tally.toolInput(call.Input, "command")
		if !ok || command == "" {
			return
		}
		normalized := auditDigits.ReplaceAllString(command, "N")
		if auditIsWaitOnly(normalized) {
			// The generic repetition rule deliberately does not see it: the
			// wait_only rule is the diagnosis for this evidence, and
			// counting the same turns twice would report one anomaly as two.
			tally.count(tally.waitOnly, sessionID, auditCut(normalized, auditCommandMaxLen))
			return
		}
		tally.count(tally.commands, sessionID, auditCut(normalized, auditCommandMaxLen))
	}
}

// applyToolResult folds one tool result: a failed one is counted under the
// class its normalized content belongs to.
func (tally *auditTally) applyToolResult(sessionID string, data json.RawMessage) {
	var result auditToolResultPart
	if err := json.Unmarshal(data, &result); err != nil {
		tally.parseFailures++
		return
	}
	if !result.IsError {
		return
	}
	if class := auditNormalizeError(result.Content); class != "" {
		tally.count(tally.toolErrors, sessionID, class)
	}
}

// toolInput reads one field of a tool call's input. Input that is not a JSON
// object, or a field that is absent or not a string, is "not there": a rule
// that cannot read its evidence simply has none, but input that is not valid
// JSON at all is a parse failure.
func (tally *auditTally) toolInput(input, field string) (string, bool) {
	if strings.TrimSpace(input) == "" {
		return "", false
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(input), &values); err != nil {
		tally.parseFailures++
		return "", false
	}
	text, ok := values[field].(string)
	return text, ok
}

// count increments one session's counter for one piece of evidence, creating
// the session's map on first use.
func (tally *auditTally) count(counters map[string]map[string]int, sessionID, detail string) {
	if counters[sessionID] == nil {
		counters[sessionID] = map[string]int{}
	}
	counters[sessionID][detail]++
}

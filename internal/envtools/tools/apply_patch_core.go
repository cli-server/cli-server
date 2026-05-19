package tools

import (
	"fmt"
	"strings"
)

// Patch grammar markers (see /root/codex/codex-rs/core/src/tools/handlers/apply_patch.lark).
const (
	beginPatchMarker    = "*** Begin Patch"
	endPatchMarker      = "*** End Patch"
	addFileMarker       = "*** Add File: "
	deleteFileMarker    = "*** Delete File: "
	updateFileMarker    = "*** Update File: "
	moveToMarker        = "*** Move to: "
	eofMarker           = "*** End of File"
	changeContextMarker = "@@ "
	emptyChangeContext  = "@@"
)

// FileOpKind identifies the kind of operation.
type FileOpKind int

const (
	OpAdd FileOpKind = iota + 1
	OpUpdate
	OpDelete
	OpMove
)

// FileOp is one entry in the parsed patch.
type FileOp struct {
	Kind    FileOpKind
	Path    string
	NewPath string      // only set when Kind == OpMove
	Content string      // full file body for OpAdd; empty for others
	Hunks   []PatchHunk // only set when Kind == OpUpdate or OpMove
}

// PatchHunk is one @@-delimited block in an Update File entry.
// Lines are stored in their original order so the application step
// can reconstruct the modified file. Context (when non-empty) is the
// "@@ <text>" anchor that the applier locates first; this matters
// when the body context appears in multiple places in the source.
type PatchHunk struct {
	Context string
	Lines   []HunkLine
}

type HunkLineKind int

const (
	HunkContext HunkLineKind = iota + 1
	HunkAdd
	HunkRemove
)

type HunkLine struct {
	Kind HunkLineKind
	Text string
}

// parseErr formats errors with line numbers in a uniform shape.
func parseErr(line int, format string, args ...any) error {
	return fmt.Errorf("apply_patch: "+format+" (line %d)", append(args, line)...)
}

// ParsePatch parses a complete apply_patch document.
func ParsePatch(input string) ([]FileOp, error) {
	if input == "" {
		return nil, fmt.Errorf("apply_patch: empty patch")
	}
	// Preserve original line numbers in errors: index 0 ↔ line 1 in the
	// original input, even though Begin/End markers tolerate surrounding
	// whitespace. We split BEFORE trimming and track an offset.
	rawLines := splitLines(input)

	// Locate Begin/End markers while tolerating leading/trailing blank
	// lines (matches Rust's `trim().lines()` behavior, but without losing
	// per-line numbers).
	startIdx, endIdx, err := findPatchBoundaries(rawLines)
	if err != nil {
		return nil, err
	}

	var ops []FileOp
	i := startIdx + 1 // first content line after "*** Begin Patch"
	for i < endIdx {
		// Allow blank lines between hunks (Rust skips them inside Update;
		// being permissive at the top level avoids spurious failures from
		// model-generated whitespace).
		if strings.TrimSpace(rawLines[i]) == "" {
			i++
			continue
		}
		line := strings.TrimRight(rawLines[i], "\r")
		lineNum := i + 1
		switch {
		case strings.HasPrefix(line, addFileMarker):
			op, consumed, perr := parseAddFile(rawLines, i, endIdx)
			if perr != nil {
				return nil, perr
			}
			ops = append(ops, op)
			i += consumed
		case strings.HasPrefix(line, deleteFileMarker):
			path := strings.TrimSpace(strings.TrimPrefix(line, deleteFileMarker))
			if path == "" {
				return nil, parseErr(lineNum, "Delete File requires a path")
			}
			ops = append(ops, FileOp{Kind: OpDelete, Path: path})
			i++
		case strings.HasPrefix(line, updateFileMarker):
			op, consumed, perr := parseUpdateFile(rawLines, i, endIdx)
			if perr != nil {
				return nil, perr
			}
			ops = append(ops, op)
			i += consumed
		default:
			return nil, parseErr(lineNum, "unknown hunk header %q (expected '*** Add File:', '*** Delete File:', or '*** Update File:')", line)
		}
	}
	return ops, nil
}

// findPatchBoundaries returns indices of the Begin and End marker lines.
// Surrounding blank lines are tolerated; content between markers is not
// trimmed because line numbers in errors must match the caller's input.
func findPatchBoundaries(lines []string) (int, int, error) {
	start := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if t == beginPatchMarker {
			start = i
		}
		break
	}
	if start == -1 {
		return 0, 0, parseErr(1, "missing %q header", beginPatchMarker)
	}
	end := -1
	for i := len(lines) - 1; i > start; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if t == endPatchMarker {
			end = i
		}
		break
	}
	if end == -1 {
		return 0, 0, parseErr(len(lines), "missing %q footer", endPatchMarker)
	}
	return start, end, nil
}

func parseAddFile(lines []string, start, end int) (FileOp, int, error) {
	header := strings.TrimRight(lines[start], "\r")
	path := strings.TrimSpace(strings.TrimPrefix(header, addFileMarker))
	if path == "" {
		return FileOp{}, 0, parseErr(start+1, "Add File requires a path")
	}
	var b strings.Builder
	i := start + 1
	count := 0
	for i < end {
		line := strings.TrimRight(lines[i], "\r")
		if !strings.HasPrefix(line, "+") {
			break
		}
		b.WriteString(line[1:])
		b.WriteByte('\n')
		i++
		count++
	}
	if count == 0 {
		return FileOp{}, 0, parseErr(start+1, "Add File hunk for %q has no '+' lines", path)
	}
	return FileOp{Kind: OpAdd, Path: path, Content: b.String()}, i - start, nil
}

func parseUpdateFile(lines []string, start, end int) (FileOp, int, error) {
	header := strings.TrimRight(lines[start], "\r")
	path := strings.TrimSpace(strings.TrimPrefix(header, updateFileMarker))
	if path == "" {
		return FileOp{}, 0, parseErr(start+1, "Update File requires a path")
	}

	i := start + 1
	movePath := ""
	if i < end {
		l := strings.TrimRight(lines[i], "\r")
		if strings.HasPrefix(l, moveToMarker) {
			movePath = strings.TrimSpace(strings.TrimPrefix(l, moveToMarker))
			if movePath == "" {
				return FileOp{}, 0, parseErr(i+1, "Move to requires a path")
			}
			i++
		}
	}

	var hunks []PatchHunk
	first := true
	for i < end {
		// Skip blank separator lines inside Update blocks.
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		l := strings.TrimRight(lines[i], "\r")
		// A line beginning with '*' marks the start of the next file hunk
		// (Add/Delete/Update/Move/End of File handled below). Stop here so
		// the outer loop can dispatch.
		if strings.HasPrefix(l, "***") {
			break
		}
		hunk, consumed, perr := parseUpdateChunk(lines, i, end, first)
		if perr != nil {
			return FileOp{}, 0, perr
		}
		hunks = append(hunks, hunk)
		i += consumed
		first = false
	}

	if len(hunks) == 0 {
		return FileOp{}, 0, parseErr(start+1, "Update File hunk for %q is empty", path)
	}
	op := FileOp{Kind: OpUpdate, Path: path, Hunks: hunks}
	if movePath != "" {
		op.Kind = OpMove
		op.NewPath = movePath
	}
	return op, i - start, nil
}

func parseUpdateChunk(lines []string, start, end int, allowMissingContext bool) (PatchHunk, int, error) {
	i := start
	first := strings.TrimRight(lines[i], "\r")
	var contextAnchor string
	switch {
	case first == emptyChangeContext:
		i++
	case strings.HasPrefix(first, changeContextMarker):
		// The "@@ <text>" header anchors the hunk: ApplyHunks must locate
		// this text in the source and advance the cursor past it before
		// matching the body. The Rust reference does the same — without
		// it, the same context-body in two places would always patch the
		// FIRST occurrence even when the model meant the second.
		contextAnchor = strings.TrimSpace(strings.TrimPrefix(first, changeContextMarker))
		i++
	default:
		if !allowMissingContext {
			return PatchHunk{}, 0, parseErr(start+1, "expected '@@' context marker, got %q", first)
		}
	}

	if i >= end {
		return PatchHunk{}, 0, parseErr(i, "Update hunk has no body lines")
	}

	var hl []HunkLine
	for i < end {
		raw := strings.TrimRight(lines[i], "\r")
		if raw == eofMarker {
			i++
			break
		}
		// Next file hunk → stop without consuming.
		if strings.HasPrefix(raw, "***") {
			break
		}
		// Next @@ header → stop without consuming so the outer loop opens
		// a new chunk.
		if raw == emptyChangeContext || strings.HasPrefix(raw, changeContextMarker) {
			break
		}
		if raw == "" {
			// Empty line: treat as empty context (matches Rust behavior).
			hl = append(hl, HunkLine{Kind: HunkContext, Text: ""})
			i++
			continue
		}
		switch raw[0] {
		case ' ':
			hl = append(hl, HunkLine{Kind: HunkContext, Text: raw[1:]})
		case '+':
			hl = append(hl, HunkLine{Kind: HunkAdd, Text: raw[1:]})
		case '-':
			hl = append(hl, HunkLine{Kind: HunkRemove, Text: raw[1:]})
		default:
			if len(hl) == 0 {
				return PatchHunk{}, 0, parseErr(i+1, "unexpected line %q in update hunk; expected leading ' ', '+', or '-'", raw)
			}
			// Unknown leading char on a non-empty hunk means we walked
			// into the next hunk's territory. Stop without consuming.
			return PatchHunk{Context: contextAnchor, Lines: hl}, i - start, nil
		}
		i++
	}

	if len(hl) == 0 {
		return PatchHunk{}, 0, parseErr(start+1, "Update hunk has no body lines")
	}
	return PatchHunk{Context: contextAnchor, Lines: hl}, i - start, nil
}

// splitLines splits on '\n' and preserves an empty final element if the
// input ends in '\n', but DOES NOT add a spurious trailing empty entry —
// strings.Split would produce one when the input ends in '\n'. We deal
// with that explicitly so line numbers stay sane.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// ApplyHunks applies a list of hunks to a source file body, returning
// the resulting body. Context (and removed) lines must match the source
// byte-for-byte.
func ApplyHunks(source string, hunks []PatchHunk) (string, error) {
	srcLines := splitLines(source)
	// Record whether the source ended in '\n' so we can restore it.
	trailingNL := strings.HasSuffix(source, "\n") || source == ""

	pos := 0 // cursor into srcLines
	var out []string
	for hi, h := range hunks {
		// If the hunk carries an "@@ <text>" anchor, locate that line
		// first and emit everything between the prior cursor and the
		// anchor untouched. Matches the Rust reference's two-stage
		// search; disambiguates when the body context appears in
		// multiple places (e.g. two functions sharing a `} else {`).
		if h.Context != "" {
			anchorIdx := -1
			for j := pos; j < len(srcLines); j++ {
				if strings.TrimSpace(srcLines[j]) == h.Context ||
					strings.Contains(srcLines[j], h.Context) {
					anchorIdx = j
					break
				}
			}
			if anchorIdx == -1 {
				return "", fmt.Errorf("apply_patch: hunk %d anchor %q not found in source from line %d", hi+1, h.Context, pos+1)
			}
			// Emit everything from pos through the anchor untouched; the
			// body search resumes from the line *after* the anchor.
			out = append(out, srcLines[pos:anchorIdx+1]...)
			pos = anchorIdx + 1
		}

		// Build the "old" sequence (context + removed) and the "new"
		// sequence (context + added), preserving relative order.
		var oldSeq []string
		var newSeq []string
		for _, ln := range h.Lines {
			switch ln.Kind {
			case HunkContext:
				oldSeq = append(oldSeq, ln.Text)
				newSeq = append(newSeq, ln.Text)
			case HunkRemove:
				oldSeq = append(oldSeq, ln.Text)
			case HunkAdd:
				newSeq = append(newSeq, ln.Text)
			}
		}

		// Pure-insertion hunk (no oldSeq) appends at the current cursor.
		if len(oldSeq) == 0 {
			out = append(out, srcLines[pos:]...)
			out = append(out, newSeq...)
			pos = len(srcLines)
			continue
		}

		// Locate first match of oldSeq in srcLines[pos:].
		matchIdx := -1
		for j := pos; j+len(oldSeq) <= len(srcLines); j++ {
			ok := true
			for k, ol := range oldSeq {
				if srcLines[j+k] != ol {
					ok = false
					break
				}
			}
			if ok {
				matchIdx = j
				break
			}
		}
		if matchIdx == -1 {
			return "", fmt.Errorf("apply_patch: hunk %d context did not match source (looking for %q starting at source line %d)", hi+1, oldSeq[0], pos+1)
		}

		// Emit untouched prefix, then the new sequence, advance cursor.
		out = append(out, srcLines[pos:matchIdx]...)
		out = append(out, newSeq...)
		pos = matchIdx + len(oldSeq)
	}
	// Append any trailing source lines untouched.
	out = append(out, srcLines[pos:]...)

	res := strings.Join(out, "\n")
	if trailingNL && res != "" {
		res += "\n"
	}
	return res, nil
}

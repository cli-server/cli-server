package tools

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyPatchParse(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      []FileOp
		wantErr   string // substring expected in error message
		wantLineN string // substring like "(line 5)" — empty to skip
	}{
		{
			name:    "empty patch",
			input:   "",
			wantErr: "empty patch",
		},
		{
			name: "single add file with trailing blank line",
			input: `*** Begin Patch
*** Add File: hello.txt
+line one
+line two
+
*** End Patch
`,
			want: []FileOp{{
				Kind:    OpAdd,
				Path:    "hello.txt",
				Content: "line one\nline two\n\n",
			}},
		},
		{
			name: "single update file one hunk one line",
			input: `*** Begin Patch
*** Update File: src/a.go
@@
 package a
-var x = 1
+var x = 2
 func F() {}
*** End Patch
`,
			want: []FileOp{{
				Kind: OpUpdate,
				Path: "src/a.go",
				Hunks: []PatchHunk{{
					Lines: []HunkLine{
						{Kind: HunkContext, Text: "package a"},
						{Kind: HunkRemove, Text: "var x = 1"},
						{Kind: HunkAdd, Text: "var x = 2"},
						{Kind: HunkContext, Text: "func F() {}"},
					},
				}},
			}},
		},
		{
			name: "update file multiple hunks",
			input: `*** Begin Patch
*** Update File: m.go
@@ func A():
 a()
-old1
+new1
@@ func B():
 b()
-old2
+new2
*** End Patch
`,
			want: []FileOp{{
				Kind: OpUpdate,
				Path: "m.go",
				Hunks: []PatchHunk{
					{Context: "func A():", Lines: []HunkLine{
						{Kind: HunkContext, Text: "a()"},
						{Kind: HunkRemove, Text: "old1"},
						{Kind: HunkAdd, Text: "new1"},
					}},
					{Context: "func B():", Lines: []HunkLine{
						{Kind: HunkContext, Text: "b()"},
						{Kind: HunkRemove, Text: "old2"},
						{Kind: HunkAdd, Text: "new2"},
					}},
				},
			}},
		},
		{
			name: "delete file",
			input: `*** Begin Patch
*** Delete File: dead/file.go
*** End Patch
`,
			want: []FileOp{{Kind: OpDelete, Path: "dead/file.go"}},
		},
		{
			name: "move file with hunk",
			input: `*** Begin Patch
*** Update File: a/b.txt
*** Move to: c/d.txt
@@
-old
+new
*** End Patch
`,
			want: []FileOp{{
				Kind:    OpMove,
				Path:    "a/b.txt",
				NewPath: "c/d.txt",
				Hunks: []PatchHunk{{Lines: []HunkLine{
					{Kind: HunkRemove, Text: "old"},
					{Kind: HunkAdd, Text: "new"},
				}}},
			}},
		},
		{
			name: "multiple files add update delete",
			input: `*** Begin Patch
*** Add File: new.txt
+hello
*** Update File: e.txt
@@
 keep
-bye
+hi
*** Delete File: gone.txt
*** End Patch
`,
			want: []FileOp{
				{Kind: OpAdd, Path: "new.txt", Content: "hello\n"},
				{Kind: OpUpdate, Path: "e.txt", Hunks: []PatchHunk{{Lines: []HunkLine{
					{Kind: HunkContext, Text: "keep"},
					{Kind: HunkRemove, Text: "bye"},
					{Kind: HunkAdd, Text: "hi"},
				}}}},
				{Kind: OpDelete, Path: "gone.txt"},
			},
		},
		{
			name: "missing end patch",
			input: `*** Begin Patch
*** Add File: foo.txt
+x
`,
			wantErr:   "missing",
			wantLineN: "End Patch",
		},
		{
			name: "unknown hunk keyword",
			input: `*** Begin Patch
*** Frob File: weird.txt
+x
*** End Patch
`,
			wantErr:   "unknown hunk header",
			wantLineN: "(line 2)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePatch(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				if tc.wantLineN != "" && !strings.Contains(err.Error(), tc.wantLineN) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantLineN)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mismatch:\n got  = %#v\n want = %#v", got, tc.want)
			}
		})
	}
}

func TestApplyPatchHunksApply(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		patch   string
		want    string
		wantErr string
	}{
		{
			name:   "one line change with context",
			source: "package a\nvar x = 1\nfunc F() {}\n",
			patch: `*** Begin Patch
*** Update File: src/a.go
@@
 package a
-var x = 1
+var x = 2
 func F() {}
*** End Patch
`,
			want: "package a\nvar x = 2\nfunc F() {}\n",
		},
		{
			// Two hunks in order, no @@ anchors (empty headers). Applied
			// sequentially against the source.
			name:   "multi-hunk in order without anchors",
			source: "a()\nold1\nfiller\nb()\nold2\n",
			patch: `*** Begin Patch
*** Update File: m.go
@@
 a()
-old1
+new1
@@
 b()
-old2
+new2
*** End Patch
`,
			want: "a()\nnew1\nfiller\nb()\nnew2\n",
		},
		{
			name:   "pure insertion (no context, no remove)",
			source: "alpha\nbeta\n",
			patch: `*** Begin Patch
*** Update File: f.txt
@@
+gamma
*** End Patch
`,
			want: "alpha\nbeta\ngamma\n",
		},
		{
			name:   "context mismatch errors with line info",
			source: "hello\nworld\n",
			patch: `*** Begin Patch
*** Update File: f.txt
@@
 hello
-foo
+bar
*** End Patch
`,
			wantErr: "hunk 1 context did not match",
		},
		{
			name:   "first occurrence wins",
			source: "x\nfoo\ny\nfoo\nz\n",
			patch: `*** Begin Patch
*** Update File: f.txt
@@
-foo
+FOO
*** End Patch
`,
			// Only first "foo" replaced; subsequent foo stays.
			want: "x\nFOO\ny\nfoo\nz\n",
		},
		{
			// Two functions share the body context "    return x" but the
			// patch's @@ anchor names func bar() — without anchor support,
			// the applier would silently edit foo().
			name: "@@ anchor disambiguates between duplicate bodies",
			source: "func foo() {\n    x := 1\n    return x\n}\nfunc bar() {\n    x := 2\n    return x\n}\n",
			patch: `*** Begin Patch
*** Update File: f.go
@@ func bar() {
     x := 2
-    return x
+    return x * 10
 }
*** End Patch
`,
			want: "func foo() {\n    x := 1\n    return x\n}\nfunc bar() {\n    x := 2\n    return x * 10\n}\n",
		},
		{
			name:    "@@ anchor not found in source errors",
			source:  "func foo() {\n    return\n}\n",
			patch: `*** Begin Patch
*** Update File: f.go
@@ func nonexistent() {
     return
-    return
+    return 1
*** End Patch
`,
			wantErr: "anchor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := ParsePatch(tc.patch)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(ops) != 1 || ops[0].Kind != OpUpdate {
				t.Fatalf("expected one update op, got %#v", ops)
			}
			got, err := ApplyHunks(tc.source, ops[0].Hunks)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got != tc.want {
				t.Fatalf("mismatch:\n got  = %q\n want = %q", got, tc.want)
			}
		})
	}
}

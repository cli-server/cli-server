package executortools

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func (e *ToolExecutor) glob(ctx context.Context, rawArgs json.RawMessage) ExecuteResponse {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponse("invalid arguments: " + err.Error())
	}
	if args.Pattern == "" {
		return errResponse("pattern is required")
	}

	root := e.WorkDir
	if args.Path != "" {
		root = resolvePath(e.WorkDir, args.Path)
	}

	re, err := globToRegexp(args.Pattern)
	if err != nil {
		return errResponse("invalid pattern: " + err.Error())
	}

	var matches []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		if re.MatchString(rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return errResponse("glob failed: " + err.Error())
	}

	sort.Strings(matches)
	return okResponse(strings.Join(matches, "\n"))
}

// globToRegexp translates a glob with ** (recursive) and * (single-segment)
// wildcards into an anchored regular expression.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Pin mirrors the shape of codex-pin.json.
type Pin struct {
	UpstreamRepo             string                       `json:"upstream_repo"`
	Tag                      string                       `json:"tag"`
	Sha                      string                       `json:"sha"`
	TrackedFiles             map[string]string            `json:"tracked_files"`
	NormalizedEquivalentFiles map[string]NormalizedEntry  `json:"normalized_equivalent_files"`
	ApprovalMethods          []string                     `json:"approval_methods"`
}

// NormalizedEntry describes a file that is equivalent to an upstream file after normalization.
type NormalizedEntry struct {
	UpstreamPath    string `json:"upstream_path"`
	NormalizedSha256 string `json:"normalized_sha256"`
	Comment         string `json:"comment"`
}

// Mismatch records a single verification failure.
type Mismatch struct {
	File   string
	Reason string
	Want   string
	Got    string
}

// Report holds all mismatches found by Verify.
type Report struct {
	Mismatches []Mismatch
}

// goPackageRe matches lines of the form:
//
//	option go_package = "...";
//
// The pattern uses .* (not [^;]*) so that semicolons embedded inside the
// quoted package path (e.g. "github.com/…;relaypb") are handled correctly.
// In (?m) mode, . does not cross newlines, so the match stays on one line.
var goPackageRe = regexp.MustCompile(`(?m)^option go_package[[:space:]]*=.*;\s*$`)

// multiBlankRe matches two or more consecutive newlines.
var multiBlankRe = regexp.MustCompile(`\n{2,}`)

// normalize strips the `option go_package = ...;` directive and collapses any
// run of 2+ consecutive newlines to exactly one blank line (preserves the
// standard between-message separator).
func normalize(content []byte) []byte {
	// Normalise line endings to \n so the regex is line-end agnostic.
	b := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	b = goPackageRe.ReplaceAll(b, nil)
	b = multiBlankRe.ReplaceAll(b, []byte("\n\n"))
	return b
}

// hexSHA256 returns the hex-encoded SHA-256 of data.
func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Verify reads the pin file at pinPath and checks:
//  1. Every entry in tracked_files: the file at <upstreamRoot>/<upstream-path>
//     must have the expected sha256.
//  2. Every entry in normalized_equivalent_files: the file at <repoRoot>/<our-path>,
//     after normalization, must have the expected normalized_sha256.
//
// It returns a Report containing all mismatches found (not just the first).
func Verify(pinPath, repoRoot, upstreamRoot string) (*Report, error) {
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		return nil, fmt.Errorf("read pin file %s: %w", pinPath, err)
	}

	var pin Pin
	if err := json.Unmarshal(raw, &pin); err != nil {
		return nil, fmt.Errorf("parse pin file %s: %w", pinPath, err)
	}

	report := &Report{}

	// --- Check tracked_files -------------------------------------------------
	for upstreamPath, wantSha := range pin.TrackedFiles {
		abs := filepath.Join(upstreamRoot, upstreamPath)
		data, err := os.ReadFile(abs)
		if err != nil {
			report.Mismatches = append(report.Mismatches, Mismatch{
				File:   upstreamPath,
				Reason: "missing",
				Want:   wantSha,
				Got:    err.Error(),
			})
			continue
		}
		got := hexSHA256(data)
		if got != wantSha {
			report.Mismatches = append(report.Mismatches, Mismatch{
				File:   upstreamPath,
				Reason: "tracked-sha",
				Want:   wantSha,
				Got:    got,
			})
		}
	}

	// --- Check normalized_equivalent_files -----------------------------------
	for ourPath, entry := range pin.NormalizedEquivalentFiles {
		abs := filepath.Join(repoRoot, ourPath)
		data, err := os.ReadFile(abs)
		if err != nil {
			report.Mismatches = append(report.Mismatches, Mismatch{
				File:   ourPath,
				Reason: "missing",
				Want:   entry.NormalizedSha256,
				Got:    err.Error(),
			})
			continue
		}
		norm := normalize(data)
		got := hexSHA256(norm)
		if got != entry.NormalizedSha256 {
			report.Mismatches = append(report.Mismatches, Mismatch{
				File:   ourPath,
				Reason: "normalized-equivalent",
				Want:   entry.NormalizedSha256,
				Got:    got,
			})
		}
	}

	return report, nil
}

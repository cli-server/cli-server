package agent

import (
	"context"
	"testing"
)

func TestParseGoVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"go version go1.22.0 darwin/arm64", "1.22.0"},
		{"go version go1.21.5 linux/amd64", "1.21.5"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseGoVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseGoVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParsePythonVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Python 3.12.1", "3.12.1"},
		{"Python 3.11.0", "3.11.0"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parsePythonVersion(tt.input)
		if got != tt.want {
			t.Errorf("parsePythonVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseNodeVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v20.11.0", "20.11.0"},
		{"v18.19.1", "18.19.1"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseNodeVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseNodeVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseRustVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"rustc 1.77.0 (aedd173a2 2024-03-17)", "1.77.0"},
		{"rustc 1.75.0 (82e1608df 2023-12-21)", "1.75.0"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseRustVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseRustVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseJavaVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`openjdk version "21.0.1" 2023-10-17`, "21.0.1"},
		{`java version "17.0.9" 2023-10-17 LTS`, "17.0.9"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseJavaVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseJavaVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseRubyVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ruby 3.3.0 (2023-12-25 revision 5124f9ac75) [arm64-darwin23]", "3.3.0"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseRubyVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseRubyVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseDockerVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Docker version 25.0.3, build 4debf41", "25.0.3"},
		{"Docker version 24.0.7, build afdd53b", "24.0.7"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseDockerVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseDockerVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"git version 2.44.0", "2.44.0"},
		{"git version 2.39.3 (Apple Git-146)", "2.39.3"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseGitVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseGitVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseGenericFirstVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"GNU Make 4.4.1", "4.4.1"},
		{"cmake version 3.28.3", "3.28.3"},
		{"Terraform v1.7.4", "1.7.4"},
		{"aws-cli/2.15.17 Python/3.11.8 Darwin/23.3.0 source/arm64", "2.15.17"},
		{"Google Cloud SDK 464.0.0", "464.0.0"},
		{"ffmpeg version 6.1.1 Copyright (c) 2000-2023 the FFmpeg developers", "6.1.1"},
		{"Client Version: v1.29.2", "1.29.2"},
		{"v3.14.2+gf56ede7", "3.14.2"},
		{"PHP 8.3.3 (cli) (built: Feb 13 2024 09:46:46) (NTS)", "8.3.3"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseGenericVersion(tt.input)
		if got != tt.want {
			t.Errorf("parseGenericVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProbeCapabilities(t *testing.T) {
	ctx := context.Background()
	caps := ProbeCapabilities(ctx)

	if caps == nil {
		t.Fatal("ProbeCapabilities returned nil")
	}
	if caps.ProbedAt.IsZero() {
		t.Error("ProbedAt is zero")
	}

	for _, lang := range caps.Languages {
		if lang.Name == "" {
			t.Error("language entry has empty name")
		}
		if lang.Version == "" {
			t.Errorf("language %q has empty version", lang.Name)
		}
	}
	for _, tool := range caps.Tools {
		if tool.Name == "" {
			t.Error("tool entry has empty name")
		}
		if tool.Version == "" {
			t.Errorf("tool %q has empty version", tool.Name)
		}
	}

	t.Logf("found %d languages, %d tools, gpu=%v",
		len(caps.Languages), len(caps.Tools), caps.GPU != nil)
}

func TestProbeCapabilitiesRespectsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caps := ProbeCapabilities(ctx)
	if caps == nil {
		t.Fatal("ProbeCapabilities returned nil on cancelled context")
	}
}

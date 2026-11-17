// Package checkrun runs user-provided check scripts in both the original
// and the minified image and judges whether the minified image still works.
//
// Judgement model:
//
//   - Each script runs once per image; exit code, stdout and stderr are
//     captured. Comparison is never raw string equality: unstable content
//     (timestamps, PIDs, random ports) is neutralized first with
//     user-declared ignore patterns (regular expressions).
//   - Equal exit codes but different (normalized) output is "suspicious",
//     not "failed": the application ran, but behaved observably different.
//   - A timeout on either side yields "inconclusive" (undecided), never a
//     hang: every script runs under its own timeout and the remaining
//     scripts continue.
//   - If the tooling the script depends on is gone from the minified image
//     (e.g. the shell was slimmed away), the executor reports ToolMissing
//     and the verdict is "inconclusive" with reason tool-missing, which is
//     kept distinct from "the application itself is broken" (fail).
//   - Merging: any fail -> overall fail; else any suspicious -> overall
//     suspicious; else any inconclusive -> overall inconclusive; else pass.
//   - Exit codes (for pipeline gating): fail is always non-zero. Whether
//     "suspicious" (and "inconclusive") also map to a non-zero exit code
//     is the user's choice via ExitOptions.
package checkrun

import (
	"context"
	"regexp"
	"sort"
	"time"
)

// Status is the verdict of one script or of the whole run.
type Status string

const (
	StatusPass         Status = "pass"
	StatusFail         Status = "fail"
	StatusSuspicious   Status = "suspicious"
	StatusInconclusive Status = "inconclusive"
)

// Reason explains a non-pass verdict.
type Reason string

const (
	ReasonNone          Reason = ""
	ReasonExitCode      Reason = "exit-code-mismatch"
	ReasonOutput        Reason = "output-mismatch"
	ReasonTimeoutBefore Reason = "timeout-before"
	ReasonTimeoutAfter  Reason = "timeout-after"
	ReasonToolMissing   Reason = "tool-missing"
	ReasonExecutorError Reason = "executor-error"
)

// Outcome is what one script execution produced on one image.
type Outcome struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// TimedOut is set when the script did not finish within its timeout.
	TimedOut bool `json:"timed_out,omitempty"`
	// ToolMissing is set when the executor could not run the script at all
	// because the required tooling (e.g. a shell) is absent from the image.
	// This is distinct from the application failing.
	ToolMissing bool `json:"tool_missing,omitempty"`
}

// Script is one user-provided check.
type Script struct {
	Name    string        `json:"name"`
	Body    string        `json:"body"`
	Timeout time.Duration `json:"timeout,omitempty"`
	// IgnorePatterns are regular expressions; matches in stdout/stderr are
	// replaced with a fixed placeholder before comparison. Use them for
	// timestamps, PIDs, random ports and other unstable content.
	IgnorePatterns []string `json:"ignore_patterns,omitempty"`
}

// Result is the verdict for one script.
type Result struct {
	Script string  `json:"script"`
	Status Status  `json:"status"`
	Reason Reason  `json:"reason,omitempty"`
	Detail string  `json:"detail,omitempty"`
	Before Outcome `json:"before"`
	After  Outcome `json:"after"`
}

// Report is the full check run.
type Report struct {
	Results []Result `json:"results"`
	Overall Status   `json:"overall"`
}

// Executor runs one script inside one image. Implementations talk to a
// container runtime; tests use a fake. A nil error with ToolMissing set
// means "the check tooling is absent", not "the application is broken".
type Executor interface {
	Run(ctx context.Context, image string, script Script) (Outcome, error)
}

// ExitOptions controls how the overall verdict maps to a process exit code.
type ExitOptions struct {
	// SuspiciousNonZero makes "suspicious" exit non-zero (2). Default off:
	// pipelines that gate on the exit code then only hard-fail on "fail".
	SuspiciousNonZero bool
	// InconclusiveNonZero makes "inconclusive" exit non-zero (3).
	InconclusiveNonZero bool
}

// ExitCode maps an overall verdict to a process exit code.
func ExitCode(overall Status, opts ExitOptions) int {
	switch overall {
	case StatusFail:
		return 1
	case StatusSuspicious:
		if opts.SuspiciousNonZero {
			return 2
		}
	case StatusInconclusive:
		if opts.InconclusiveNonZero {
			return 3
		}
	}
	return 0
}

// normalize applies the ignore patterns so unstable content compares equal.
func normalize(s string, ignores []*regexp.Regexp) string {
	for _, re := range ignores {
		s = re.ReplaceAllString(s, "\x00IGNORED\x00")
	}
	return s
}

// Compare judges one script given its outcomes on both images.
func Compare(before, after Outcome, ignores []*regexp.Regexp) (Status, Reason, string) {
	// tooling problems first: they say nothing about the application
	if before.ToolMissing || after.ToolMissing {
		switch {
		case before.ToolMissing && after.ToolMissing:
			return StatusInconclusive, ReasonToolMissing, "check tooling missing in both images"
		case after.ToolMissing:
			return StatusInconclusive, ReasonToolMissing, "check tooling missing in minified image"
		default:
			return StatusInconclusive, ReasonToolMissing, "check tooling missing in original image"
		}
	}

	// a timeout on either side makes the comparison undecidable
	switch {
	case before.TimedOut && after.TimedOut:
		return StatusInconclusive, ReasonTimeoutBefore, "script timed out in both images"
	case before.TimedOut:
		return StatusInconclusive, ReasonTimeoutBefore, "script timed out in original image"
	case after.TimedOut:
		return StatusInconclusive, ReasonTimeoutAfter, "script timed out in minified image"
	}

	if before.ExitCode != after.ExitCode {
		return StatusFail, ReasonExitCode, "exit codes differ"
	}

	if normalize(before.Stdout, ignores) != normalize(after.Stdout, ignores) ||
		normalize(before.Stderr, ignores) != normalize(after.Stderr, ignores) {
		return StatusSuspicious, ReasonOutput, "same exit code but output differs"
	}

	return StatusPass, ReasonNone, ""
}

// Merge combines per-script verdicts: any fail -> fail; else any
// suspicious -> suspicious; else any inconclusive -> inconclusive.
func Merge(results []Result) Status {
	overall := StatusPass
	for _, r := range results {
		switch r.Status {
		case StatusFail:
			return StatusFail
		case StatusSuspicious:
			overall = StatusSuspicious
		case StatusInconclusive:
			if overall != StatusSuspicious {
				overall = StatusInconclusive
			}
		}
	}
	return overall
}

// Runner executes check scripts against both images.
type Runner struct {
	Executor    Executor
	BeforeImage string
	AfterImage  string
	// DefaultTimeout applies to scripts without their own timeout.
	DefaultTimeout time.Duration
}

// Run executes every script on both images and returns the full report.
// Scripts run sequentially; each is bounded by its own timeout, so one
// hanging script never blocks the rest. Results are ordered by script name
// for deterministic output.
func (r *Runner) Run(ctx context.Context, scripts []Script) (*Report, error) {
	sorted := make([]Script, len(scripts))
	copy(sorted, scripts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	report := &Report{}
	for _, script := range sorted {
		result := Result{Script: script.Name}

		ignores, err := compileIgnores(script)
		if err != nil {
			result.Status = StatusInconclusive
			result.Reason = ReasonExecutorError
			result.Detail = err.Error()
			report.Results = append(report.Results, result)
			continue
		}

		timeout := script.Timeout
		if timeout <= 0 {
			timeout = r.DefaultTimeout
		}

		result.Before, err = r.runOne(ctx, r.BeforeImage, script, timeout)
		if err != nil {
			result.Status = StatusInconclusive
			result.Reason = ReasonExecutorError
			result.Detail = err.Error()
			report.Results = append(report.Results, result)
			continue
		}

		result.After, err = r.runOne(ctx, r.AfterImage, script, timeout)
		if err != nil {
			result.Status = StatusInconclusive
			result.Reason = ReasonExecutorError
			result.Detail = err.Error()
			report.Results = append(report.Results, result)
			continue
		}

		result.Status, result.Reason, result.Detail = Compare(result.Before, result.After, ignores)
		report.Results = append(report.Results, result)
	}

	report.Overall = Merge(report.Results)
	return report, nil
}

func (r *Runner) runOne(ctx context.Context, image string, script Script, timeout time.Duration) (Outcome, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return r.Executor.Run(ctx, image, script)
}

func compileIgnores(script Script) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range script.IgnorePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

package checkrun

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeExecutor is a container-executor stand-in returning preset outcomes
// keyed by image name.
type fakeExecutor struct {
	outcomes map[string]Outcome // image -> outcome
	errs     map[string]error
	calls    []string
}

func (f *fakeExecutor) Run(ctx context.Context, image string, script Script) (Outcome, error) {
	f.calls = append(f.calls, image)
	if err, ok := f.errs[image]; ok {
		return Outcome{}, err
	}
	return f.outcomes[image], nil
}

const (
	imgBefore = "app:1.0"
	imgAfter  = "app:1.0-slim"
)

func runOne(t *testing.T, ex Executor, scripts []Script) *Report {
	t.Helper()
	r := &Runner{Executor: ex, BeforeImage: imgBefore, AfterImage: imgAfter}
	report, err := r.Run(context.Background(), scripts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestIdenticalOutputsPass(t *testing.T) {
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0, Stdout: "ok\n"},
		imgAfter:  {ExitCode: 0, Stdout: "ok\n"},
	}}
	report := runOne(t, ex, []Script{{Name: "health"}})

	if report.Overall != StatusPass {
		t.Fatalf("overall = %s", report.Overall)
	}
	if report.Results[0].Status != StatusPass {
		t.Fatalf("result = %+v", report.Results[0])
	}
	if got := ExitCode(report.Overall, ExitOptions{SuspiciousNonZero: true}); got != 0 {
		t.Fatalf("exit code = %d", got)
	}
}

func TestTimestampsIgnoredByPattern(t *testing.T) {
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0, Stdout: "started at 2026-11-18T10:00:00Z pid=123\n"},
		imgAfter:  {ExitCode: 0, Stdout: "started at 2026-11-18T11:31:07Z pid=981\n"},
	}}
	script := Script{
		Name: "boot",
		IgnorePatterns: []string{
			`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`,
			`pid=\d+`,
		},
	}
	report := runOne(t, ex, []Script{script})
	if report.Overall != StatusPass {
		t.Fatalf("overall = %s, want pass (unstable content ignored)", report.Overall)
	}
}

func TestTimestampsWithoutIgnoreAreSuspicious(t *testing.T) {
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0, Stdout: "started at 10:00:00\n"},
		imgAfter:  {ExitCode: 0, Stdout: "started at 11:31:07\n"},
	}}
	report := runOne(t, ex, []Script{{Name: "boot"}})

	res := report.Results[0]
	if res.Status != StatusSuspicious || res.Reason != ReasonOutput {
		t.Fatalf("result = %+v, want suspicious/output-mismatch", res)
	}
	if report.Overall != StatusSuspicious {
		t.Fatalf("overall = %s", report.Overall)
	}
	// same exit code + different output is suspicious, not failed
	if res.Status == StatusFail {
		t.Fatal("output-only difference must not be a failure")
	}
	// exit code mapping: user chooses whether suspicious is non-zero
	if got := ExitCode(report.Overall, ExitOptions{}); got != 0 {
		t.Fatalf("default suspicious exit = %d, want 0", got)
	}
	if got := ExitCode(report.Overall, ExitOptions{SuspiciousNonZero: true}); got != 2 {
		t.Fatalf("opt-in suspicious exit = %d, want 2", got)
	}
}

func TestExitCodeMismatchFails(t *testing.T) {
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0, Stdout: "ok\n"},
		imgAfter:  {ExitCode: 1, Stdout: "ok\n"},
	}}
	report := runOne(t, ex, []Script{{Name: "health"}})

	res := report.Results[0]
	if res.Status != StatusFail || res.Reason != ReasonExitCode {
		t.Fatalf("result = %+v", res)
	}
	if got := ExitCode(report.Overall, ExitOptions{}); got != 1 {
		t.Fatalf("fail exit = %d, want 1", got)
	}
}

func TestOneSideTimeoutIsInconclusiveAndDoesNotBlock(t *testing.T) {
	scripts := []Script{
		{Name: "a-slow", Timeout: 50 * time.Millisecond},
		{Name: "b-fast"},
	}
	// second script must still run: make it pass on both sides
	ex2 := &multiExecutor{
		byScript: map[string]map[string]Outcome{
			"a-slow": {
				imgBefore: {ExitCode: 0, Stdout: "ok\n"},
				imgAfter:  {TimedOut: true},
			},
			"b-fast": {
				imgBefore: {ExitCode: 0, Stdout: "fine\n"},
				imgAfter:  {ExitCode: 0, Stdout: "fine\n"},
			},
		},
	}
	report := runOne(t, ex2, scripts)

	if len(report.Results) != 2 {
		t.Fatalf("results = %d, timeout blocked the flow", len(report.Results))
	}
	// results ordered by script name
	if report.Results[0].Script != "a-slow" || report.Results[1].Script != "b-fast" {
		t.Fatalf("result order = %+v", report.Results)
	}
	slow := report.Results[0]
	if slow.Status != StatusInconclusive || slow.Reason != ReasonTimeoutAfter {
		t.Fatalf("slow = %+v, want inconclusive/timeout-after", slow)
	}
	if report.Results[1].Status != StatusPass {
		t.Fatalf("fast = %+v, want pass", report.Results[1])
	}
	if report.Overall != StatusInconclusive {
		t.Fatalf("overall = %s, want inconclusive", report.Overall)
	}
}

// multiExecutor presets outcomes per script and image.
type multiExecutor struct {
	byScript map[string]map[string]Outcome
}

func (m *multiExecutor) Run(ctx context.Context, image string, script Script) (Outcome, error) {
	return m.byScript[script.Name][image], nil
}

func TestToolMissingIsNotAppFailure(t *testing.T) {
	// shell was slimmed away: the check cannot run in the minified image
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0, Stdout: "ok\n"},
		imgAfter:  {ToolMissing: true},
	}}
	report := runOne(t, ex, []Script{{Name: "health"}})

	res := report.Results[0]
	if res.Status == StatusFail {
		t.Fatal("missing check tooling must not be reported as application failure")
	}
	if res.Status != StatusInconclusive || res.Reason != ReasonToolMissing {
		t.Fatalf("result = %+v, want inconclusive/tool-missing", res)
	}
}

func TestExecutorErrorIsInconclusive(t *testing.T) {
	ex := &fakeExecutor{
		outcomes: map[string]Outcome{imgBefore: {ExitCode: 0}},
		errs:     map[string]error{imgAfter: errors.New("container refused to start")},
	}
	report := runOne(t, ex, []Script{{Name: "health"}})
	res := report.Results[0]
	if res.Status != StatusInconclusive || res.Reason != ReasonExecutorError {
		t.Fatalf("result = %+v", res)
	}
}

func TestMergePrecedence(t *testing.T) {
	cases := []struct {
		name    string
		results []Result
		want    Status
	}{
		{"all pass", []Result{{Status: StatusPass}, {Status: StatusPass}}, StatusPass},
		{"fail wins over suspicious", []Result{{Status: StatusSuspicious}, {Status: StatusFail}}, StatusFail},
		{"suspicious without fail", []Result{{Status: StatusPass}, {Status: StatusSuspicious}}, StatusSuspicious},
		{"suspicious wins over inconclusive", []Result{{Status: StatusInconclusive}, {Status: StatusSuspicious}}, StatusSuspicious},
		{"inconclusive only", []Result{{Status: StatusPass}, {Status: StatusInconclusive}}, StatusInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Merge(tc.results); got != tc.want {
				t.Fatalf("Merge = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRunnerExecutesBothImages(t *testing.T) {
	ex := &fakeExecutor{outcomes: map[string]Outcome{
		imgBefore: {ExitCode: 0},
		imgAfter:  {ExitCode: 0},
	}}
	runOne(t, ex, []Script{{Name: "health"}})

	if len(ex.calls) != 2 || ex.calls[0] != imgBefore || ex.calls[1] != imgAfter {
		t.Fatalf("executor calls = %v, want one run per image", ex.calls)
	}
}

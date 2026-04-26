package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/pbsladek/wait-for/internal/output"
	"github.com/spf13/pflag"
)

type doctorStatus string

const (
	doctorOK   doctorStatus = "ok"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
)

type doctorCheck struct {
	Name     string       `json:"name"`
	Status   doctorStatus `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Required bool         `json:"required,omitempty"`
}

type doctorReport struct {
	Status  doctorStatus  `json:"status"`
	Version string        `json:"version"`
	Commit  string        `json:"commit,omitempty"`
	GOOS    string        `json:"goos"`
	GOARCH  string        `json:"goarch"`
	Checks  []doctorCheck `json:"checks"`
}

type doctorOptions struct {
	format   output.Format
	required map[string]bool
}

func isDoctorCommand(args []string) bool {
	return len(args) > 0 && args[0] == "doctor"
}

func runDoctor(args []string, stdout io.Writer, _ io.Writer) (int, error) {
	if doctorWantsHelp(args) {
		_, _ = io.WriteString(stdout, doctorHelpText())
		return ExitSatisfied, nil
	}
	opts, err := parseDoctorOptions(args)
	if err != nil {
		return ExitInvalid, err
	}
	report := buildDoctorReport(opts.required)
	if err := writeDoctorReport(stdout, opts.format, report); err != nil {
		return ExitFatal, err
	}
	if report.Status == doctorFail {
		return ExitFatal, nil
	}
	return ExitSatisfied, nil
}

func doctorWantsHelp(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

func parseDoctorOptions(args []string) (doctorOptions, error) {
	opts := doctorOptions{format: output.FormatText, required: map[string]bool{"temp": true}}
	var format string
	var required []string
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&format, "output", string(opts.format), "output format: text|json")
	fs.StringArrayVar(&required, "require", nil, "required check: temp|shell|docker|k8s|dns-wire")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if len(fs.Args()) != 0 {
		return opts, fmt.Errorf("doctor does not accept positional arguments")
	}
	switch output.Format(format) {
	case output.FormatText, output.FormatJSON:
		opts.format = output.Format(format)
	default:
		return opts, fmt.Errorf("invalid output format %q", format)
	}
	for _, item := range required {
		if err := requireDoctorChecks(opts.required, item); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func requireDoctorChecks(required map[string]bool, raw string) error {
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !validDoctorCheck(item) {
			return fmt.Errorf("invalid doctor check %q", item)
		}
		required[item] = true
	}
	return nil
}

func validDoctorCheck(name string) bool {
	switch name {
	case "temp", "shell", "docker", "k8s", "dns-wire":
		return true
	default:
		return false
	}
}

func buildDoctorReport(required map[string]bool) doctorReport {
	version, commit := buildMetadata()
	report := doctorReport{
		Status:  doctorOK,
		Version: version,
		Commit:  commit,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Checks: []doctorCheck{
			tempDirCheck(),
			shellCheck(),
			dockerCheck(),
			kubernetesCheck(),
			dnsWireCheck(),
		},
	}
	for i := range report.Checks {
		report.Checks[i].Required = required[report.Checks[i].Name]
		report.Status = combineDoctorStatus(report.Status, report.Checks[i])
	}
	return report
}

func combineDoctorStatus(current doctorStatus, check doctorCheck) doctorStatus {
	if check.Required && check.Status != doctorOK {
		return doctorFail
	}
	if current == doctorOK && check.Status != doctorOK {
		return doctorWarn
	}
	return current
}

func buildMetadata() (string, string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)", ""
	}
	version := info.Main.Version
	commit := ""
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			commit = setting.Value
		}
	}
	return version, commit
}

func tempDirCheck() doctorCheck {
	dir := os.TempDir()
	file, err := os.CreateTemp(dir, "waitfor-doctor-*")
	if err != nil {
		return doctorCheck{Name: "temp", Status: doctorFail, Detail: err.Error()}
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return doctorCheck{Name: "temp", Status: doctorFail, Detail: err.Error()}
	}
	if err := os.Remove(name); err != nil {
		return doctorCheck{Name: "temp", Status: doctorFail, Detail: err.Error()}
	}
	return doctorCheck{Name: "temp", Status: doctorOK, Detail: "writable " + filepath.Clean(dir)}
}

func shellCheck() doctorCheck {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		return doctorCheck{Name: "shell", Status: doctorWarn, Detail: "not found: " + shell}
	}
	return doctorCheck{Name: "shell", Status: doctorOK, Detail: path}
}

func dockerCheck() doctorCheck {
	if _, err := exec.LookPath("docker"); err != nil {
		return doctorCheck{Name: "docker", Status: doctorWarn, Detail: "docker CLI not found"}
	}
	out, err := runDoctorCommand("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return doctorCheck{Name: "docker", Status: doctorWarn, Detail: err.Error()}
	}
	return doctorCheck{Name: "docker", Status: doctorOK, Detail: "server " + out}
}

func kubernetesCheck() doctorCheck {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return doctorCheck{Name: "k8s", Status: doctorWarn, Detail: "kubectl not found"}
	}
	out, err := runDoctorCommand("kubectl", "config", "current-context")
	if err != nil {
		return doctorCheck{Name: "k8s", Status: doctorWarn, Detail: err.Error()}
	}
	return doctorCheck{Name: "k8s", Status: doctorOK, Detail: "context " + out}
}

func dnsWireCheck() doctorCheck {
	return doctorCheck{Name: "dns-wire", Status: doctorOK, Detail: "compiled"}
}

func runDoctorCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- doctor runs fixed diagnostics, not user-supplied commands.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s: %s", name, detail)
	}
	return out, nil
}

func writeDoctorReport(w io.Writer, format output.Format, report doctorReport) error {
	if format == output.FormatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	_, _ = fmt.Fprintf(w, "waitfor doctor: %s\n", report.Status)
	_, _ = fmt.Fprintf(w, "version: %s\n", report.Version)
	if report.Commit != "" {
		_, _ = fmt.Fprintf(w, "commit: %s\n", report.Commit)
	}
	_, _ = fmt.Fprintf(w, "platform: %s/%s\n", report.GOOS, report.GOARCH)
	for _, check := range report.Checks {
		_, _ = fmt.Fprintf(w, "[%s] %s%s\n", check.Status, check.Name, doctorDetail(check))
	}
	return nil
}

func doctorDetail(check doctorCheck) string {
	if check.Detail == "" {
		return ""
	}
	return ": " + check.Detail
}

func doctorHelpText() string {
	return `waitfor doctor - report local waitfor environment support

Usage:
  waitfor doctor [flags]

Flags:
  --output text|json       Output format (default: text)
  --require check          Required check: temp|shell|docker|k8s|dns-wire
                           Repeatable and comma-separated values are accepted
`
}

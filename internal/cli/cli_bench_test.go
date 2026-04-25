package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"testing"
)

func BenchmarkParseGlobal(b *testing.B) {
	args := []string{"--timeout", "30s", "--interval", "100ms", "--successes", "3", "file", "/tmp/ready"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := parseGlobal(args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseConditionsManyFiles(b *testing.B) {
	for _, count := range []int{1, 10, 100, 1000} {
		b.Run("conditions="+strconv.Itoa(count), func(b *testing.B) {
			args := manyFileConditionArgs(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := parseConditions(args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSplitConditionSegments(b *testing.B) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "many-file-conditions", args: manyFileConditionArgs(1000)},
		{name: "long-exec-command", args: longExecArgs(10000)},
		{name: "literal-separators-as-values", args: literalSeparatorValueArgs(1000)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := splitConditionSegments(tc.args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseHTTPConditionManyHeaders(b *testing.B) {
	args := []string{"http", "https://api.example.test/health", "--status", "2xx"}
	for i := 0; i < 20; i++ {
		args = append(args, "--header", fmt.Sprintf("X-Test-%d=value-%d", i, i))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseHTTPCondition(args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseExecConditionLongCommand(b *testing.B) {
	args := longExecArgs(1000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseExecCondition(args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseKubernetesConditionSelector(b *testing.B) {
	args := []string{"k8s", "pod", "--selector", "app=api,component in (web,worker)", "--for", "ready", "--all", "--namespace", "prod"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseKubernetesCondition(args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExecuteHelp(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var stdout bytes.Buffer
		if code := Execute(context.Background(), []string{"--help"}, nil, &stdout, io.Discard); code != ExitSatisfied {
			b.Fatalf("exit code = %d", code)
		}
	}
}

func manyFileConditionArgs(count int) []string {
	args := make([]string, 0, count*4)
	for i := 0; i < count; i++ {
		if i > 0 {
			args = append(args, "--")
		}
		args = append(args, "file", "/tmp/ready-"+strconv.Itoa(i), "--exists")
	}
	return args
}

func longExecArgs(count int) []string {
	args := []string{"exec", "--", "/bin/echo"}
	for i := 0; i < count; i++ {
		if i%100 == 0 {
			args = append(args, "--", "http")
			continue
		}
		args = append(args, "arg-"+strconv.Itoa(i))
	}
	return args
}

func literalSeparatorValueArgs(count int) []string {
	args := make([]string, 0, count*6)
	for i := 0; i < count; i++ {
		if i > 0 {
			args = append(args, "--")
		}
		args = append(args, "file", "/tmp/ready-"+strconv.Itoa(i), "--contains", "--")
	}
	return args
}

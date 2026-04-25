package condition

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func BenchmarkCheckK8sSelected(b *testing.B) {
	for _, count := range []int{10, 1000, 50000} {
		b.Run(fmt.Sprintf("first-satisfied/N=%d", count), func(b *testing.B) {
			items := benchmarkPods(count, true, -1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if result := checkK8sSelected(items, "ready", false); result.Status != CheckSatisfied {
					b.Fatalf("status = %s", result.Status)
				}
			}
		})
		b.Run(fmt.Sprintf("failed-last/N=%d", count), func(b *testing.B) {
			items := benchmarkPods(count, true, count-1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if result := checkK8sSelected(items, "ready", false); result.Status != CheckFatal {
					b.Fatalf("status = %s", result.Status)
				}
			}
		})
	}
}

func BenchmarkKubernetesGetterList(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run("items="+strconv.Itoa(count), func(b *testing.B) {
			objects := make([]runtime.Object, 0, count)
			for i := 0; i < count; i++ {
				pod := podObject("True")
				pod.SetName("pod-" + strconv.Itoa(i))
				pod.SetLabels(map[string]string{"app": "bench"})
				objects = append(objects, pod)
			}
			getter := NewDynamicKubernetesGetterWithClient(fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				items, err := getter.List(context.Background(), "pod", "default", "app=bench")
				if err != nil {
					b.Fatal(err)
				}
				if len(items) != count {
					b.Fatalf("len(items) = %d, want %d", len(items), count)
				}
			}
		})
	}
}

func BenchmarkHTTPConditionCheckBodySizes(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{name: "1KiB", size: 1 << 10},
		{name: "1MiB", size: 1 << 20},
		{name: "10MiB", size: 10 << 20},
		{name: "100MiB", size: 100 << 20},
	}
	for _, size := range sizes {
		b.Run(size.name+"/status-only", func(b *testing.B) {
			if size.size > 10<<20 && os.Getenv("WAITFOR_LARGE_BENCH") == "" {
				b.Skip("set WAITFOR_LARGE_BENCH=1 to run very large HTTP body benchmark")
			}
			benchmarkHTTPConditionBody(b, size.size, false)
		})
		b.Run(size.name+"/body-contains", func(b *testing.B) {
			if size.size > 10<<20 && os.Getenv("WAITFOR_LARGE_BENCH") == "" {
				b.Skip("set WAITFOR_LARGE_BENCH=1 to run very large HTTP body benchmark")
			}
			benchmarkHTTPConditionBody(b, size.size, true)
		})
	}
}

func benchmarkHTTPConditionBody(b *testing.B, size int, bodyMatcher bool) {
	body := bytes.Repeat([]byte("x"), size)
	if len(body) > 0 {
		body[0] = 'z'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cond := NewHTTP(server.URL)
	if bodyMatcher {
		cond.BodyContains = "z"
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := cond.Check(context.Background())
		if bodyMatcher && result.Status != CheckSatisfied {
			b.Fatalf("status = %s, err = %v", result.Status, result.Err)
		}
		if !bodyMatcher && result.Status != CheckSatisfied {
			b.Fatalf("status = %s, err = %v", result.Status, result.Err)
		}
	}
}

func BenchmarkFileContainsLargeNoMatch(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 10<<20), 0o600); err != nil {
		b.Fatal(err)
	}
	cond := NewFile(path, FileExists)
	cond.Contains = "not-present"
	b.SetBytes(10 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if result := cond.Check(context.Background()); result.Status != CheckUnsatisfied {
			b.Fatalf("status = %s", result.Status)
		}
	}
}

func BenchmarkLogScanLargeChunk(b *testing.B) {
	data := append(bytes.Repeat([]byte("x"), 10<<20), '\n')
	cond := NewLog("bench.log")
	cond.Contains = "not-present"
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cond.matchCount = 0
		if result := cond.scanLines(context.Background(), data); result.Status != CheckUnsatisfied {
			b.Fatalf("status = %s", result.Status)
		}
	}
}

func BenchmarkClassifyDockerInspectErrorLargeOutput(b *testing.B) {
	output := string(bytes.Repeat([]byte("x"), 10<<20))
	b.SetBytes(int64(len(output)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := classifyDockerInspectError(io.ErrUnexpectedEOF, output); err == nil {
			b.Fatal("expected error")
		}
	}
}

func benchmarkPods(count int, firstReady bool, failedIndex int) []map[string]any {
	items := make([]map[string]any, count)
	for i := 0; i < count; i++ {
		ready := "False"
		if firstReady && i == 0 {
			ready = "True"
		}
		pod := podObject(ready)
		pod.SetName("pod-" + strconv.Itoa(i))
		if i == failedIndex {
			pod.Object["status"].(map[string]any)["phase"] = "Failed"
		}
		items[i] = pod.Object
	}
	return items
}

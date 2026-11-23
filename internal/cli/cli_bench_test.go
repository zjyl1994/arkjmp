package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	json "encoding/json/v2"
)

func BenchmarkNodesJSONLargeGroup(b *testing.B) {
	const count = 1000
	members := make([]string, 0, count)
	proxies := make(map[string]any, count+1)
	for i := range count {
		name := fmt.Sprintf("香港-%04d", i)
		members = append(members, name)
		proxies[name] = map[string]any{"type": "Vless", "history": []map[string]int{{"delay": count - i}}}
	}
	proxies["Manual"] = map[string]any{"type": "Selector", "now": members[0], "all": members}
	payload, err := json.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		b.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) })
	mux.HandleFunc("/providers/proxies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"providers":{}}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"mode":"rule"}`)) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"version":"bench"}`)) })
	server := httptest.NewServer(mux)
	b.Cleanup(server.Close)
	setupEnv(b, server.URL)

	discard, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer discard.Close()
	stdout := os.Stdout
	os.Stdout = discard
	defer func() { os.Stdout = stdout }()

	args := []string{"nodes", "--json"}
	b.ReportAllocs()
	for b.Loop() {
		if code := Main(args); code != ExitOK {
			b.Fatalf("unexpected exit code %d", code)
		}
	}
}

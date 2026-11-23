package tui

import (
	"context"
	"fmt"
	"testing"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/zjyl1994/arkjmp/internal/client"
	"github.com/zjyl1994/arkjmp/internal/core"
)

type largeGroupAPI struct {
	emptyAPI
	proxies *client.Object
}

func (f largeGroupAPI) Proxies(context.Context) (*client.Object, error) {
	return f.proxies, nil
}

func BenchmarkRenderLargeGroup(b *testing.B) {
	const count = 1000
	proxies := &client.Object{Items: make(map[string]jsontext.Value, count+1)}
	members := make([]string, 0, count)
	for i := range count {
		name := fmt.Sprintf("香港-%04d", i)
		members = append(members, name)
		proxies.Keys = append(proxies.Keys, name)
		proxies.Items[name] = jsontext.Value(fmt.Sprintf(`{"type":"Vless","history":[{"delay":%d}]}`, count-i))
	}
	group, err := json.Marshal(map[string]any{"type": "Selector", "now": members[0], "all": members})
	if err != nil {
		b.Fatal(err)
	}
	proxies.Keys = append(proxies.Keys, "Manual")
	proxies.Items["Manual"] = group

	snap := core.New(largeGroupAPI{proxies: proxies}, "")
	if err := snap.Refresh(b.Context(), core.FullRefresh); err != nil {
		b.Fatal(err)
	}
	m := newModel(b.Context(), snap)
	m.toggleSort()
	m.applyFilter("香港")

	b.ReportAllocs()
	for b.Loop() {
		_ = m.render()
	}
}

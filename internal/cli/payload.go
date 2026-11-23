package cli

import "github.com/zjyl1994/arkjmp/internal/core"

// 这里的结构体就是 --json 的契约：字段顺序 = JSON 键顺序，指针字段用于输出 null
// 或按需省略（omitzero），改字段等于改 API，注意同步 README。

type statusPayload struct {
	API         string   `json:"api"`
	EnvFile     *string  `json:"env_file"`
	ConfigFile  *string  `json:"config_file"`
	Auth        bool     `json:"auth"`
	Version     string   `json:"version"`
	Mode        string   `json:"mode"`
	Groups      int      `json:"groups"`
	Providers   []string `json:"providers"`
	TestURL     string   `json:"test_url"`
	ToolVersion string   `json:"tool_version"`
	OK          bool     `json:"ok"`
}

type groupSummary struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Now     *string  `json:"now"`
	Members int      `json:"members"`
	Chain   []string `json:"chain"`
}

type groupsPayload struct {
	Groups []groupSummary `json:"groups"`
	OK     bool           `json:"ok"`
}

type groupPayload struct {
	Group string         `json:"group"`
	Type  string         `json:"type"`
	Now   *string        `json:"now"`
	Chain []string       `json:"chain"`
	Nodes []core.NodeRow `json:"nodes"`
	OK    bool           `json:"ok"`
}

type nodesPayload struct {
	Group *string                  `json:"group"`
	Nodes []core.NodeRowWithGroups `json:"nodes"`
	OK    bool                     `json:"ok"`
}

type currentRow struct {
	Group string   `json:"group"`
	Type  string   `json:"type"`
	Now   *string  `json:"now"`
	Chain []string `json:"chain"`
}

type currentPayload struct {
	Current []currentRow `json:"current"`
	OK      bool         `json:"ok"`
}

type chainPayload struct {
	Group string   `json:"group"`
	Chain []string `json:"chain"`
	Leaf  string   `json:"leaf"`
	OK    bool     `json:"ok"`
}

type switchPayload struct {
	Group       string  `json:"group"`
	Node        string  `json:"node"`
	Previous    *string `json:"previous"`
	DryRun      bool    `json:"dry_run"`
	Changed     bool    `json:"changed"`
	WouldChange *bool   `json:"would_change,omitzero"`
	Now         *string `json:"now,omitzero"`
	OK          bool    `json:"ok"`
}

type delayPayload struct {
	Node     string  `json:"node"`
	Provider *string `json:"provider"`
	Delay    int     `json:"delay"`
	Timeout  bool    `json:"timeout"`
	OK       bool    `json:"ok"`
}

type testPayload struct {
	Group     string            `json:"group"`
	Total     int               `json:"total"`
	Available int               `json:"available"`
	Failed    int               `json:"failed"`
	Partial   bool              `json:"partial"`
	Failures  map[string]string `json:"failures"`
	Nodes     []core.NodeRow    `json:"nodes"`
	OK        bool              `json:"ok"`
}

type listGroup struct {
	Name  string         `json:"name"`
	Type  string         `json:"type"`
	Now   *string        `json:"now"`
	Nodes []core.NodeRow `json:"nodes"`
}

type listPayload struct {
	API     string      `json:"api"`
	Version string      `json:"version"`
	Mode    string      `json:"mode"`
	Groups  []listGroup `json:"groups"`
	OK      bool        `json:"ok"`
}

type errorInfo struct {
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
}

type errorPayload struct {
	OK    bool      `json:"ok"`
	Error errorInfo `json:"error"`
}

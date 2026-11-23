package client

import "net/url"

// quote 节点名里有中文/空格/方括号，进 URL 前整段编码。
func quote(text string) string { return url.PathEscape(text) }

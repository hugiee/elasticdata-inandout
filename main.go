// elasticdata-inandout: Elasticsearch 索引数据导入/导出工具(仅 JSON,用法类似 elasticdump)
//
// 导出: elasticdata-inandout --input=https://user:pass@host:9200/myindex --output=myindex.json
// 导入: elasticdata-inandout --input=myindex.json --output=https://user:pass@host:9200/myindex
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// https 一律跳过证书校验(需求指定)
var client = &http.Client{Transport: &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}}

// elasticdump 兼容格式:第 1 行元数据,第 2 行 _source
type metaLine struct {
	Index string `json:"_index"`
	Type  string `json:"_type,omitempty"`
	ID    string `json:"_id,omitempty"`
}

type scrollRes struct {
	ScrollID string `json:"_scroll_id"`
	Hits     struct {
		Hits []struct {
			Index  string          `json:"_index"`
			Type   string          `json:"_type,omitempty"`
			ID     string          `json:"_id"`
			Source json.RawMessage `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// do 向 ES 发请求,baseURL 含用户密码,path 为接口路径
func do(method, baseURL, path, body string) ([]byte, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if i := strings.Index(path, "?"); i >= 0 {
		u.Path, u.RawQuery = path[:i], path[i+1:]
	} else {
		u.Path = path
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.User != nil {
		pw, _ := u.User.Password()
		req.SetBasicAuth(u.User.Username(), pw)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return data, fmt.Errorf("es %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(data, 300))
	}
	return data, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// progress 进度条;total<=0 时降级为仅显示计数
type progress struct {
	w     io.Writer
	label string
	total int64
}

func (p *progress) show(n int64) {
	if p.total <= 0 {
		fmt.Fprintf(p.w, "\r%s %d 条", p.label, n)
		return
	}
	const width = 30
	done := n * width / p.total
	if done > width {
		done = width
	}
	bar := strings.Repeat("#", int(done)) + strings.Repeat("-", int(width-done))
	fmt.Fprintf(p.w, "\r%s [%s] %d/%d (%.1f%%)", p.label, bar, n, p.total, float64(n)/float64(p.total)*100)
}

// countDocs 用 _count 获取待导出总数(带相同过滤条件),失败返回 -1
func countDocs(baseURL, index, searchBody string) int64 {
	q := map[string]any{"query": map[string]any{"match_all": struct{}{}}}
	if searchBody != "" {
		var m map[string]any
		if json.Unmarshal([]byte(searchBody), &m) == nil {
			if query, ok := m["query"]; ok {
				q = map[string]any{"query": query}
			}
		}
	}
	b, _ := json.Marshal(q)
	data, err := do("POST", baseURL, "/"+index+"/_count", string(b))
	if err != nil {
		return -1
	}
	var r struct {
		Count int64 `json:"count"`
	}
	if json.Unmarshal(data, &r) != nil {
		return -1
	}
	return r.Count
}

// countLines 统计文件行数(导入总数 = 行数/2),失败返回 -1
func countLines(file string) int64 {
	f, err := os.Open(file)
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<28)
	var n int64
	for sc.Scan() {
		n++
	}
	return n / 2
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func indexFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return strings.Trim(u.Path, "/"), nil
}

// export 用 scroll API 把索引导出为 elasticdump 两行式 JSON 文件
func export(baseURL, index, file string, limit int, searchBody string) error {
	const scroll = "2m"
	path := "/" + index + "/_search?scroll=" + scroll
	body := fmt.Sprintf(`{"size":%d,"query":{"match_all":{}}}`, limit)
	if searchBody != "" {
		// 与 ES _search 请求体一致的 DSL,如 {"query":{"term":{"status":"active"}}}
		var m map[string]any
		if err := json.Unmarshal([]byte(searchBody), &m); err != nil {
			return fmt.Errorf("--search-body 不是合法的 ES DSL JSON: %w", err)
		}
		if _, ok := m["size"]; !ok {
			m["size"] = limit
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		body = string(b)
	}
	var res scrollRes
	data, err := do("POST", baseURL, path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("解析 search 响应: %w", err)
	}

	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	pb := &progress{w: os.Stderr, label: "导出", total: countDocs(baseURL, index, searchBody)}
	n := 0
	scrollID := res.ScrollID
	for {
		for _, hit := range res.Hits.Hits {
			m, _ := json.Marshal(metaLine{Index: hit.Index, Type: hit.Type, ID: hit.ID})
			w.Write(m)
			w.WriteByte('\n')
			w.Write(hit.Source)
			w.WriteByte('\n')
			n++
		}
		if scrollID == "" || len(res.Hits.Hits) == 0 {
			break
		}
		body, _ := json.Marshal(map[string]string{"scroll": scroll, "scroll_id": scrollID})
		data, err := do("POST", baseURL, "/_search/scroll", string(body))
		if err != nil {
			return err
		}
		res = scrollRes{}
		if err := json.Unmarshal(data, &res); err != nil {
			return fmt.Errorf("解析 scroll 响应: %w", err)
		}
		scrollID = res.ScrollID
		pb.show(int64(n))
	}
	fmt.Fprintf(os.Stderr, "\n导出完成: %d 条 -> %s\n", n, file)
	return nil
}

// importFile 读取 elasticdump 两行式 JSON 文件,通过 _bulk 导入
func importFile(file, baseURL, index string, limit int) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<28) // 单行文档最大 256MB
	pb := &progress{w: os.Stderr, label: "导入", total: countLines(file)}
	var buf bytes.Buffer
	n, failed := 0, 0

	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		body := buf.Bytes()
		buf.Reset()
		body = append(body, '\n')
		data, err := do("POST", baseURL, "/_bulk", string(body))
		if err != nil {
			return err
		}
		var br struct {
			Errors bool `json:"errors"`
			Items  []struct {
				Index struct {
					Status int `json:"status"`
					Error  any `json:"error"`
				} `json:"index"`
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &br); err != nil {
			return fmt.Errorf("解析 bulk 响应: %w", err)
		}
		for _, it := range br.Items {
			if it.Index.Status >= 300 {
				failed++
				if failed <= 3 {
					fmt.Fprintf(os.Stderr, "文档导入失败: %v\n", it.Index.Error)
				}
			}
		}
		return nil
	}

	for sc.Scan() {
		var m metaLine
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return fmt.Errorf("%s 第 %d 行元数据解析失败: %w", file, n*2+1, err)
		}
		if !sc.Scan() {
			return fmt.Errorf("%s 文件行数为奇数,元数据缺少对应文档", file)
		}
		idx := index
		if idx == "" {
			idx = m.Index
		}
		action, _ := json.Marshal(map[string]map[string]string{
			"index": {"_index": idx, "_id": m.ID},
		})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(sc.Bytes())
		buf.WriteByte('\n')
		n++
		if n%limit == 0 {
			pb.show(int64(n))
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n导入完成: %d 条", n)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "(失败 %d 条)", failed)
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

func main() {
	input := flag.String("input", "", "输入: http(s) ES 地址(导出)或 JSON 文件路径(导入)")
	output := flag.String("output", "", "输出: JSON 文件路径(导出)或 http(s) ES 地址(导入)")
	limit := flag.Int("limit", 1000, "每批条数(scroll 分页 / bulk 批量)")
	searchBody := flag.String("search-body", "", "导出过滤条件, ES 检索 DSL JSON, 省略时导出全部, 如 --search-body='{\"query\":{\"term\":{\"status\":\"active\"}}}'")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法: %s --input=<源> --output=<目标> [--limit=N] [--search-body=<DSL>]

参数:
  --input        输入: http(s) ES 地址(导出)或 JSON 文件路径(导入)
  --output       输出: JSON 文件路径(导出)或 http(s) ES 地址(导入)
  --limit        每批条数(scroll 分页 / bulk 批量), 默认 1000
  --search-body  导出过滤条件, ES 检索 DSL JSON, 省略时导出全部

导出示例: %s --input=https://user:pass@es:9200/idx --output=idx.json
导入示例: %s --input=idx.json --output=https://user:pass@es:9200/idx
过滤示例: %s --input=https://user:pass@es:9200/idx --output=out.json --search-body='{"query":{"term":{"status":"active"}}}'

https 自动跳过 SSL 证书校验;输出 URL 不含索引名时使用文件中的 _index
`, os.Args[0], os.Args[0], os.Args[0], os.Args[0])
	}
	flag.Parse()

	inURL, outURL := isURL(*input), isURL(*output)
	switch {
	case *input == "" || *output == "":
		flag.Usage()
		os.Exit(2)
	case inURL && !outURL: // 导出
		idx, err := indexFromURL(*input)
		if err != nil || idx == "" {
			fatal("input URL 需包含索引名, 如 https://user:pass@host:9200/myindex")
		}
		if err := export(*input, idx, *output, *limit, *searchBody); err != nil {
			fatal("导出失败: %v", err)
		}
	case !inURL && outURL: // 导入
		idx, err := indexFromURL(*output)
		if err != nil {
			fatal("output URL 无效: %v", err)
		}
		if err := importFile(*input, *output, idx, *limit); err != nil {
			fatal("导入失败: %v", err)
		}
	default:
		flag.Usage()
		fatal("input/output 必须一个为 http(s) 地址、另一个为文件路径")
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

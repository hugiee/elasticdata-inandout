// elasticdata-inandout: Elasticsearch 索引数据导入/导出工具(仅 JSON,用法类似 elasticdump)
//
// 导出: elasticdata-inandout --input=https://user:pass@host:9200/myindex --output=myindex.json
// 导入: elasticdata-inandout --input=myindex.json --output=https://user:pass@host:9200/myindex
// 结构: --type=mapping,settings 导出/导入索引的 mapping 与 settings
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
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
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

// exportOpts 控制导出内容;数据记录始终导出,结构记录按需
type exportOpts struct {
	limit      int
	maxDocs    int64 // >0 时最多导出该条数据记录;结构记录不受限
	searchBody string
	mapping    bool
	settings   bool
}

// export 按需导出到同一文件:结构记录(mapping/settings)在前,数据记录在后。
// 先写临时文件,全部成功后再替换目标文件,避免失败时销毁已有导出文件
func export(baseURL, index, file string, o exportOpts) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".elasticdata-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // rename 成功后此处为空操作
	w := bufio.NewWriter(tmp)
	fail := func(err error) error {
		tmp.Close()
		return err
	}
	if o.mapping || o.settings {
		parts, err := fetchStructure(baseURL, index, o.mapping, o.settings)
		if err != nil {
			return fail(err)
		}
		if err := writeStructureRecords(w, parts, o.mapping, o.settings); err != nil {
			return fail(err)
		}
	}
	if err := exportData(w, baseURL, index, file, o.limit, o.maxDocs, o.searchBody); err != nil {
		return fail(err)
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// exportData 用 scroll API 写出 elasticdump 两行式文档记录;maxDocs>0 时导出满该条数即停
func exportData(w *bufio.Writer, baseURL, index, file string, limit int, maxDocs int64, searchBody string) error {
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

	pb := &progress{w: os.Stderr, label: "导出", total: countDocs(baseURL, index, searchBody)}
	if maxDocs > 0 && (pb.total < 0 || pb.total > maxDocs) {
		pb.total = maxDocs
	}
	n := 0
	scrollID := res.ScrollID
	capped := false
write:
	for {
		for _, hit := range res.Hits.Hits {
			m, _ := json.Marshal(metaLine{Index: hit.Index, Type: hit.Type, ID: hit.ID})
			w.Write(m)
			w.WriteByte('\n')
			w.Write(hit.Source)
			w.WriteByte('\n')
			n++
			if maxDocs > 0 && int64(n) >= maxDocs {
				capped = true
				break write
			}
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
	if capped {
		fmt.Fprintf(os.Stderr, "\n导出完成: %d 条(达到 --max-docs 上限) -> %s\n", n, file)
		return nil
	}
	fmt.Fprintf(os.Stderr, "\n导出完成: %d 条 -> %s\n", n, file)
	return nil
}

// importFile 读取两行式 JSON 文件: 带 _type=mapping/settings 的记录先建立/更新索引结构,
// 其余记录通过 _bulk 导入数据
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
	n, failed, records := 0, 0, 0
	pending := map[string]*msParts{} // 结构记录先缓存,写数据前统一应用
	structSrc := map[string]bool{}   // 结构记录涉及的源索引名

	flushStructure := func() error {
		if len(pending) == 0 {
			return nil
		}
		names := make([]string, 0, len(pending))
		for name := range pending {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b, err := json.Marshal(pending[name])
			if err != nil {
				return err
			}
			if err := applyMappingSettings(baseURL, name, b); err != nil {
				return err
			}
		}
		pending = map[string]*msParts{}
		return nil
	}

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
			return fmt.Errorf("%s 第 %d 行元数据解析失败: %w", file, records*2+1, err)
		}
		if m.Index == "" && m.ID == "" && m.Type == "" {
			return fmt.Errorf("%s 第 %d 行不像记录元数据(缺 _index/_id);仅含 mapping/settings 的文件请用 --type=mapping,settings", file, records*2+1)
		}
		if !sc.Scan() {
			return fmt.Errorf("%s 文件行数为奇数,元数据缺少对应文档", file)
		}
		idx := index
		if idx == "" {
			idx = m.Index
		}
		records++
		if m.Type == "mapping" || m.Type == "settings" {
			// 指定了目标索引名时,文件中来自不同源索引的结构无法一一对应,必须报错而非静默合并
			if index != "" && m.Index != "" {
				structSrc[m.Index] = true
				if len(structSrc) > 1 {
					names := make([]string, 0, len(structSrc))
					for name := range structSrc {
						names = append(names, name)
					}
					sort.Strings(names)
					return fmt.Errorf("%s 含多个索引(%s)的结构记录,不能用指定索引名的 URL 导入", file, strings.Join(names, "、"))
				}
			}
			p := pending[idx]
			if p == nil {
				p = &msParts{}
				pending[idx] = p
			}
			if m.Type == "mapping" {
				p.Mappings = append(json.RawMessage(nil), sc.Bytes()...)
			} else {
				p.Settings = append(json.RawMessage(nil), sc.Bytes()...)
			}
			pb.show(int64(records))
			continue
		}
		if err := flushStructure(); err != nil {
			return err
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
			pb.show(int64(records))
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flushStructure(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n导入完成: %d 条", n)
	if records > n {
		fmt.Fprintf(os.Stderr, "(含 %d 条结构记录)", records-n)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "(失败 %d 条)", failed)
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// msParts 索引的 mapping 与 settings;用于导出文件结构、建索请求体与文件解析
type msParts struct {
	Mappings json.RawMessage `json:"mappings,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// extractParts 从 _mapping/_settings 响应(以真实索引名为键)中取出 mappings/settings
func extractParts(data []byte, index string) (msParts, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return msParts{}, fmt.Errorf("解析 ES 响应: %w", err)
	}
	inner, ok := m[index]
	if !ok { // URL 可能是别名,响应键为真实索引名;仅唯一时兑底
		if len(m) != 1 {
			return msParts{}, fmt.Errorf("ES 响应含 %d 个索引,无法确定 %q", len(m), index)
		}
		for _, v := range m {
			inner = v
		}
	}
	var p msParts
	if err := json.Unmarshal(inner, &p); err != nil {
		return msParts{}, fmt.Errorf("解析 ES 响应: %w", err)
	}
	return p, nil
}

// typeNames 返回 mapping/settings 的可读组合名
func typeNames(mapping, settings bool) []string {
	var names []string
	if mapping {
		names = append(names, "mapping")
	}
	if settings {
		names = append(names, "settings")
	}
	return names
}

// fetchStructure 获取 index(可为多索引表达式)的 mappings/settings,以真实索引名为键
func fetchStructure(baseURL, index string, wantMapping, wantSettings bool) (map[string]*msParts, error) {
	out := map[string]*msParts{}
	fetch := func(suffix, part string) error {
		data, err := do("GET", baseURL, "/"+index+"/"+suffix, "")
		if err != nil {
			return err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("解析 %s 响应: %w", suffix, err)
		}
		if len(m) == 0 {
			return fmt.Errorf("未匹配到任何索引")
		}
		for name, raw := range m {
			p := out[name]
			if p == nil {
				p = &msParts{}
				out[name] = p
			}
			var got msParts
			if err := json.Unmarshal(raw, &got); err != nil {
				return fmt.Errorf("解析 %s 响应: %w", suffix, err)
			}
			if part == "mappings" {
				p.Mappings = got.Mappings
			} else {
				p.Settings = got.Settings
			}
		}
		return nil
	}
	if wantMapping {
		if err := fetch("_mapping", "mappings"); err != nil {
			return nil, err
		}
	}
	if wantSettings {
		if err := fetch("_settings", "settings"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// writeStructureRecords 以 _type 记录流写出结构: {"_index":"i","_type":"mapping"} + 结构体
func writeStructureRecords(w *bufio.Writer, parts map[string]*msParts, wantMapping, wantSettings bool) error {
	names := make([]string, 0, len(parts))
	for n := range parts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := parts[name]
		if wantMapping && p.Mappings != nil {
			meta, _ := json.Marshal(metaLine{Index: name, Type: "mapping"})
			w.Write(meta)
			w.WriteByte('\n')
			w.Write(p.Mappings)
			w.WriteByte('\n')
		}
		if wantSettings && p.Settings != nil {
			meta, _ := json.Marshal(metaLine{Index: name, Type: "settings"})
			w.Write(meta)
			w.WriteByte('\n')
			w.Write(p.Settings)
			w.WriteByte('\n')
		}
	}
	return nil
}

// exportMappingSettings 单独导出索引结构为单行 JSON 文件: {"索引名":{"mappings":...,"settings":...}}
// index 支持逗号分隔的多个索引或通配符,如 idx1,idx2、logs-*
func exportMappingSettings(baseURL, index, file string, wantMapping, wantSettings bool) error {
	out, err := fetchStructure(baseURL, index, wantMapping, wantSettings)
	if err != nil {
		return err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "导出完成: %d 个索引的 %s -> %s\n", len(out), strings.Join(typeNames(wantMapping, wantSettings), "+"), file)
	return nil
}

// dropPrivateSettings 剔除 ES 自行管理的私有设置(uuid/creation_date 等),避免用于创建/更新时报错
func dropPrivateSettings(raw json.RawMessage) json.RawMessage {
	var m map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	for _, k := range []string{"uuid", "creation_date", "provided_name", "version"} {
		delete(m["index"], k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// keepNewFields 递归保留 existProps 中不存在的字段定义;已有字段定义不同则跳过并记录(ES 不允许修改已有字段)
func keepNewFields(fileProps, existProps map[string]any, prefix string, skipped *[]string) map[string]any {
	out := map[string]any{}
	for name, def := range fileProps {
		p := prefix + name
		edef, ok := existProps[name]
		if !ok { // 新字段,直接保留
			out[name] = def
			continue
		}
		if reflect.DeepEqual(def, edef) { // 与现有一致,无需发送
			continue
		}
		*skipped = append(*skipped, p)
		// 双方都是对象字段时,仍深入应用其下新增子字段
		fd, _ := def.(map[string]any)
		ed, _ := edef.(map[string]any)
		fp, _ := fd["properties"].(map[string]any)
		ep, _ := ed["properties"].(map[string]any)
		if fp != nil && ep != nil {
			if inner := keepNewFields(fp, ep, p+".", skipped); len(inner) > 0 {
				out[name] = map[string]any{"properties": inner}
			}
		}
	}
	return out
}

// diffMapping 对比文件与现有 mapping:properties 只保留新增字段,其余顶层参数(_meta 等)原样保留
func diffMapping(fileRaw, existRaw json.RawMessage) (json.RawMessage, []string, error) {
	type propsOnly struct {
		Properties map[string]any `json:"properties"`
	}
	var f, e propsOnly
	if err := json.Unmarshal(fileRaw, &f); err != nil {
		return nil, nil, fmt.Errorf("解析文件 mapping: %w", err)
	}
	if err := json.Unmarshal(existRaw, &e); err != nil {
		return nil, nil, fmt.Errorf("解析现有 mapping: %w", err)
	}
	var skipped []string
	props := keepNewFields(f.Properties, e.Properties, "", &skipped)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(fileRaw, &top); err != nil {
		return nil, nil, fmt.Errorf("解析文件 mapping: %w", err)
	}
	delete(top, "properties") // properties 由 diff 结果替换
	if len(props) > 0 {
		b, err := json.Marshal(props)
		if err != nil {
			return nil, nil, err
		}
		top["properties"] = b
	}
	if len(top) == 0 { // 无新增字段、也无其他顶层参数
		return nil, skipped, nil
	}
	b, err := json.Marshal(top)
	if err != nil {
		return nil, nil, err
	}
	return b, skipped, nil
}

// stripNonDynamic 从 ES "non dynamic settings [[...]]" 错误中解析被拒绝的静态设置并剔除;
// 返回剔除后的设置与被剔除键名;错误不匹配或无可剔除内容时 ok=false
func stripNonDynamic(raw json.RawMessage, errMsg string) (json.RawMessage, []string, bool) {
	m := regexp.MustCompile(`(?i)dynamic settings\s*\[\[([^\]]+)\]\]`).FindStringSubmatch(errMsg)
	if m == nil {
		return nil, nil, false
	}
	var sm map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &sm) != nil {
		return nil, nil, false
	}
	var removed []string
	for _, k := range strings.Split(m[1], ",") {
		k = strings.TrimSpace(k)
		top, rest, found := strings.Cut(k, ".")
		if !found {
			continue
		}
		if _, ok := sm[top][rest]; ok {
			delete(sm[top], rest)
			removed = append(removed, k)
		}
	}
	if removed == nil {
		return nil, nil, false
	}
	if len(sm["index"]) == 0 {
		delete(sm, "index")
	}
	if len(sm) == 0 { // 全为静态设置,无需再发请求
		return nil, removed, true
	}
	b, err := json.Marshal(sm)
	if err != nil {
		return nil, nil, false
	}
	return b, removed, true
}

// importMappingSettings 从结构文件导入;URL 未含索引名时应用文件中全部索引,
// 指定索引名时仅支持单索引文件(内容应用到该索引)
func importMappingSettings(file, baseURL, index string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("解析 %s: %w(混排文件请用 --type=data,mapping,settings 导入)", file, err)
	}
	if _, ok := m["mappings"]; ok { // 兼容未按索引名包装的建索体,如 {"mappings":...,"settings":...}
		if index == "" {
			return fmt.Errorf("%s 未包含索引名, output URL 需包含索引名", file)
		}
		m = map[string]json.RawMessage{index: data}
	}
	if len(m) == 0 {
		return fmt.Errorf("%s 中未包含索引内容", file)
	}
	if index != "" { // 指定目标索引名: 内容取文件唯一条目
		if len(m) != 1 {
			return fmt.Errorf("%s 包含 %d 个索引, output URL 指定索引名时仅支持单索引文件", file, len(m))
		}
		var raw json.RawMessage
		for _, v := range m {
			raw = v
			break
		}
		m = map[string]json.RawMessage{index: raw}
	}
	for name, raw := range m {
		if err := applyMappingSettings(baseURL, name, raw); err != nil {
			return err
		}
	}
	return nil
}

// indexNameRe 合法 ES 索引名;结构文件里的键直接拼进 URL 路径,必须校验
var indexNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// applyMappingSettings 将单个索引的 mapping/settings 应用到 ES:索引不存在则创建,已存在则只应用 ES 允许的增量
func applyMappingSettings(baseURL, key string, raw json.RawMessage) error {
	if !indexNameRe.MatchString(key) {
		return fmt.Errorf("索引名 %q 非法(不允许 /、?、前导 _ 等),已拒绝", key)
	}
	var p msParts
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("解析索引 %s 的内容: %w", key, err)
	}
	if p.Mappings == nil && p.Settings == nil {
		return fmt.Errorf("索引 %s 中未包含 mappings 或 settings", key)
	}
	p.Settings = dropPrivateSettings(p.Settings)
	// 先尝试创建索引(创建时可写入静态设置);已存在则回退为更新
	cb, err := json.Marshal(&p)
	if err != nil {
		return err
	}
	_, err = do("PUT", baseURL, "/"+key, string(cb))
	if err == nil {
		fmt.Fprintf(os.Stderr, "导入完成: 已创建索引 %s\n", key)
		return nil
	}
	if !strings.Contains(err.Error(), "resource_already_exists_exception") {
		return err
	}
	if p.Mappings != nil {
		// ES 不允许修改已有字段:先对比现有 mapping,只发送新增字段;获取失败则原样发送由 ES 校验
		if data, err := do("GET", baseURL, "/"+key+"/_mapping", ""); err == nil {
			if exist, err := extractParts(data, key); err == nil && exist.Mappings != nil {
				filtered, skipped, derr := diffMapping(p.Mappings, exist.Mappings)
				if derr != nil {
					return derr
				}
				for _, s := range skipped {
					fmt.Fprintf(os.Stderr, "警告: 跳过已有字段 %s 的定义差异(ES 不允许修改已有字段)\n", s)
				}
				p.Mappings = filtered
			}
		}
		if p.Mappings != nil {
			if _, err := do("PUT", baseURL, "/"+key+"/_mapping", string(p.Mappings)); err != nil {
				return fmt.Errorf("更新 mapping 失败: %w", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "mapping 无新增字段,跳过更新\n")
		}
	}
	if p.Settings != nil {
		if _, err := do("PUT", baseURL, "/"+key+"/_settings", string(p.Settings)); err != nil {
			// ES 拒绝静态设置时,剔除其错误中列出的键后重试一次
			stripped, removed, ok := stripNonDynamic(p.Settings, err.Error())
			if !ok {
				return fmt.Errorf("更新 settings 失败: %w", err)
			}
			fmt.Fprintf(os.Stderr, "警告: 跳过静态设置 %s(ES 不允许修改)\n", strings.Join(removed, ", "))
			if stripped != nil {
				if _, err := do("PUT", baseURL, "/"+key+"/_settings", string(stripped)); err != nil {
					return fmt.Errorf("更新 settings 失败(剔除静态设置后): %w", err)
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "导入完成: 索引 %s 已存在,已应用 mapping/settings 中 ES 允许的部分\n", key)
	return nil
}

// parseType 解析 --type 的逗号分隔取值;可任意组合,至少指定一个
func parseType(s string) (data, mapping, settings bool, err error) {
	for _, p := range strings.Split(s, ",") {
		switch strings.TrimSpace(p) {
		case "data":
			data = true
		case "mapping":
			mapping = true
		case "settings":
			settings = true
		default:
			return false, false, false, fmt.Errorf("未知的 --type 值 %q, 可选: data, mapping, settings", p)
		}
	}
	switch {
	case !data && !mapping && !settings:
		return false, false, false, fmt.Errorf("--type 取值不能为空, 可选: data, mapping, settings")
	}
	return data, mapping, settings, nil
}

func main() {
	input := flag.String("input", "", "输入: http(s) ES 地址(导出)或 JSON 文件路径(导入)")
	output := flag.String("output", "", "输出: JSON 文件路径(导出)或 http(s) ES 地址(导入)")
	limit := flag.Int("limit", 1000, "每批条数(scroll 分页 / bulk 批量)")
	maxDocs := flag.Int64("max-docs", 0, "最多导出数据条数(0 表示不限); 只影响数据记录, 结构记录不受限")
	searchBody := flag.String("search-body", "", "导出过滤条件, ES 检索 DSL JSON, 省略时导出全部, 如 --search-body='{\"query\":{\"term\":{\"status\":\"active\"}}}'")
	typeFlag := flag.String("type", "data", "导出/导入内容: data(文档, 默认), mapping, settings, 可逗号组合; data+mapping/settings 时混排为一个文件")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法: %s --input=<源> --output=<目标> [--type=<类型>] [--limit=N] [--max-docs=N] [--search-body=<DSL>]

参数:
  --input        输入: http(s) ES 地址(导出)或 JSON 文件路径(导入)
  --output       输出: JSON 文件路径(导出)或 http(s) ES 地址(导入)
  --type         导出/导入内容: data(文档, 默认), mapping, settings,
                 可逗号组合; 仅 mapping/settings 时写单行 JSON, 含 data 时混排为两行式记录
  --limit        每批条数(scroll 分页 / bulk 批量), 默认 1000
  --max-docs     最多导出数据条数, 0 表示不限(结构记录不受限)
  --search-body  导出过滤条件, ES 检索 DSL JSON, 省略时导出全部(仅 --type=data)

导出示例: %s --input=https://user:pass@es:9200/idx --output=idx.json
导入示例: %s --input=idx.json --output=https://user:pass@es:9200/idx
过滤示例: %s --input=https://user:pass@es:9200/idx --output=out.json --search-body='{"query":{"term":{"status":"active"}}}'
结构示例: %s --input=https://user:pass@es:9200/idx --output=idx.ms.json --type=mapping,settings
限条件例: %s --input=https://user:pass@es:9200/idx --output=head.json --max-docs=100
混合示例: %s --input='https://user:pass@es:9200/i1,i2' --output=all.json --type=data,mapping,settings

https 自动跳过 SSL 证书校验;输出 URL 不含索引名时使用文件中的 _index;
mapping/settings 文件格式为 {"索引名":{"mappings":...,"settings":...}},导出支持多个索引(逗号分隔/通配符);
导入时索引不存在则创建、已存在则只应用 ES 允许的增量;导入 URL 不含索引名时按文件中的索引逐个应用;
混排文件导入时先应用结构记录再写数据
`, os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0])
	}
	flag.Parse()

	wantData, wantMapping, wantSettings, err := parseType(*typeFlag)
	if err != nil {
		fatal("%v", err)
	}
	if !wantData && *searchBody != "" {
		fatal("--search-body 仅在 --type=data 时有效")
	}
	if *limit < 1 {
		fatal("--limit 必须 >= 1")
	}
	if *maxDocs < 0 {
		fatal("--max-docs 不能为负数")
	}

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
		if !wantData {
			if *maxDocs > 0 {
				fatal("--max-docs 仅在导出数据(--type 含 data)时有效")
			}
			if err := exportMappingSettings(*input, idx, *output, wantMapping, wantSettings); err != nil {
				fatal("导出失败: %v", err)
			}
		} else if err := export(*input, idx, *output, exportOpts{
			limit: *limit, maxDocs: *maxDocs, searchBody: *searchBody, mapping: wantMapping, settings: wantSettings,
		}); err != nil {
			fatal("导出失败: %v", err)
		}
	case !inURL && outURL: // 导入
		if *maxDocs > 0 {
			fatal("--max-docs 仅用于导出")
		}
		idx, err := indexFromURL(*output)
		if err != nil {
			fatal("output URL 无效: %v", err)
		}
		if !wantData {
			if err := importMappingSettings(*input, *output, idx); err != nil {
				fatal("导入失败: %v", err)
			}
		} else if err := importFile(*input, *output, idx, *limit); err != nil {
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

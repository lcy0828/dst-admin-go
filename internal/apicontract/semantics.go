package apicontract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ToolSemantics is discovery text, never an authority for parameters, runtime
// requirements or permissions. The original English contract remains available.
type ToolSemantics struct {
	Purpose     string   `json:"purpose"`
	Actions     []string `json:"actions"`
	Queries     []string `json:"queries"`
	Terms       []string `json:"terms"`
	ResultNotes string   `json:"resultNotes,omitempty"`
}

func (op *Operation) CatalogRevision() string { return op.catalog.revision }

func (op *Operation) Semantics() *ToolSemantics { return op.semantics }

func (c *Catalog) loadSemantics(raw []byte) error {
	var source struct {
		Version    int                       `json:"schemaVersion"`
		Locale     string                    `json:"locale"`
		Operations map[string]*ToolSemantics `json:"operations"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return err
	}
	if source.Version != 1 || source.Locale != "zh-CN" {
		return fmt.Errorf("unsupported tool semantics version or locale")
	}
	for id, entry := range source.Operations {
		op, ok := c.operations[id]
		if !ok || !op.Callable() {
			return fmt.Errorf("semantics references unknown/non-callable operation %s", id)
		}
		if entry == nil || utf8.RuneCountInString(entry.Purpose) < 6 || utf8.RuneCountInString(entry.Purpose) > 240 || len(entry.Actions) == 0 || len(entry.Queries) == 0 || len(entry.Terms) == 0 {
			return fmt.Errorf("incomplete tool semantics: %s", id)
		}
		for _, list := range [][]string{entry.Actions, entry.Queries, entry.Terms} {
			if len(list) > 8 {
				return fmt.Errorf("too many semantic phrases: %s", id)
			}
			for _, phrase := range list {
				if strings.TrimSpace(phrase) == "" || utf8.RuneCountInString(phrase) > 100 {
					return fmt.Errorf("invalid semantic phrase: %s", id)
				}
			}
		}
		op.semantics = entry
	}
	for _, op := range c.ordered {
		if op.Callable() && op.semantics == nil {
			if op.Deprecated && op.ReplacedBy != "" {
				op.semantics = c.operations[op.ReplacedBy].semantics
			}
			if op.semantics == nil {
				return fmt.Errorf("missing tool semantics: %s", op.ID)
			}
		}
	}
	c.indexSemantics()
	c.revision = fmt.Sprintf("%x", sha256.Sum256(append([]byte(c.revision), raw...)))
	return nil
}

// Candidate phrases are intersected with the reviewed vocabulary at indexing
// and query time. Arbitrary cross-word fragments (e.g. 线玩家 in 离线玩家) must
// not match 在线玩家. No tokenizer model, network or idle work is needed.
var semanticSynonyms = strings.NewReplacer("再发送", "重发", "重新发送", "重发", "身上的东西", "背包物品", "末尾", "尾部")

func chineseTerms(text string) map[string]bool {
	text = semanticSynonyms.Replace(text)
	terms := map[string]bool{}
	var run []rune
	var latin []rune
	flushLatin := func() {
		word := strings.ToLower(string(latin))
		switch word {
		case "mods", "mod":
			word = "模组"
		case "luajit2":
			word = "luajit"
		}
		if len(word) > 1 {
			terms[word] = true
		}
		latin = latin[:0]
	}
	flush := func() {
		for n := 2; n <= 8; n++ {
			for i := 0; i+n <= len(run); i++ {
				terms[string(run[i:i+n])] = true
			}
		}
		run = run[:0]
	}
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			flushLatin()
			run = append(run, r)
		} else {
			flush()
			if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
				latin = append(latin, r)
			} else {
				flushLatin()
			}
		}
	}
	flush()
	flushLatin()
	return terms
}

func (c *Catalog) indexSemantics() {
	c.lexicon = map[string]bool{}
	for _, word := range strings.Fields("安装 更新 校验 检查 查询 查看 读取 修改 设置 保存 启用 开启 禁用 关闭 暂停 创建 添加 删除 清理 恢复 还原 重试 取消 上传 下载 导入 导出 预览 重启 启动 停止 备份 定时 计划 任务 结果 失败 成功 日志 历史 运行 在线 离线 玩家 房间 世界 分片 节点 机器 服务器 服务端 面板 模组 采集 诊断 版本 兼容 背包 装备 物品 身上 参数 内存 磁盘 通知 邮箱 邮件 代理 端口 名称 权限 管理员 游戏 聊天 事件 退出 状态 连接 文件 影响 存档 配置 列出 自定义 指令 记录 重发 报错 崩服 崩溃 内容 回执 列表 世界状态") {
		c.lexicon[word] = true
	}
	for _, op := range c.ordered {
		if op.semantics == nil {
			continue
		}
		for _, phrase := range op.semantics.Terms {
			for _, part := range strings.FieldsFunc(strings.ToLower(phrase), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
				if utf8.RuneCountInString(part) >= 2 && utf8.RuneCountInString(part) <= 8 {
					c.lexicon[part] = true
				}
			}
		}
	}
	c.lexicon["luajit"] = true
	df := map[string]int{}
	count := 0
	for _, op := range c.ordered {
		if op.semantics == nil {
			continue
		}
		count++
		op.searchTerms = map[string]float64{}
		add := func(values []string, weight float64) {
			for _, value := range values {
				for term := range chineseTerms(value) {
					if c.lexicon[term] {
						op.searchTerms[term] = max(op.searchTerms[term], weight)
					}
				}
			}
		}
		s := op.semantics
		add([]string{s.Purpose}, 2)
		add(s.Queries, 0.5)
		add(s.Actions, 6)
		add(s.Terms, 5)
		for term := range op.searchTerms {
			df[term]++
		}
	}
	c.termWeight = map[string]float64{}
	for term, n := range df {
		c.termWeight[term] = math.Log(1 + float64(count)/float64(n))
	}
}

func (c *Catalog) semanticScore(op *Operation, terms map[string]bool) int {
	score := 0.0
	for term := range terms {
		score += op.searchTerms[term] * c.termWeight[term]
	}
	return min(900, int(math.Round(score)))
}

package main

// 管理页面积分包展示的结构化回归：列数一致、只用 DeductionEndTime、复用快照字段。

import (
	"io/fs"
	"strings"
	"testing"
)

// 从 embed 读取，和实际下发给浏览器的内容完全一致，也不受测试工作目录影响。
func readWebAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(webFS, "web/"+name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n") // 仓库里 web 资源是 CRLF，统一后再做结构断言
}

// 账号表头列数必须与数据行、展开行、空状态的 colspan 一致。
func TestAccountsTableColumnCountMatchesCreditCell(t *testing.T) {
	html := readWebAsset(t, "index.html")
	start := strings.Index(html, `<table id="accountsTable">`)
	if start < 0 {
		t.Fatal("找不到账号表")
	}
	end := strings.Index(html[start:], "</thead>")
	if end < 0 {
		t.Fatal("账号表头结构异常")
	}
	head := html[start : start+end]
	if got := strings.Count(head, "<th>"); got != 17 {
		t.Fatalf("账号表头应为 17 列（含积分到期），实际 %d", got)
	}
	if !strings.Contains(head, "积分到期") {
		t.Fatal("表头缺少「积分到期」列")
	}

	js := readWebAsset(t, "app.js")
	body := functionBody(t, js, "function renderAccounts(list) {")
	// 空状态那一行也有 <td colspan=...>，不计入数据行。
	cells := strings.Count(body, "<td") - strings.Count(body, "<td colspan")
	if cells != 17 {
		t.Fatalf("账号数据行应输出 17 个单元格，实际 %d", cells)
	}
	if !strings.Contains(js, `colspan="17"`) {
		t.Fatal("空状态 colspan 应与列数一致（17）")
	}
	if !strings.Contains(js, "colspan='17'") {
		t.Fatal("展开行 colspan 应与列数一致（17）")
	}
}

// 前端只能按 DeductionEndTime 语义展示，不能把套餐周期结束当成积分到期。
func TestCreditUIOnlyUsesDeductionEndTimeSemantics(t *testing.T) {
	js := readWebAsset(t, "app.js")
	for _, banned := range []string{"CycleEndTime", "cycleEndTime", "cycle_end_time"} {
		if strings.Contains(js, banned) {
			t.Fatalf("前端不应引用 %s（套餐周期边界不是积分作废时间）", banned)
		}
	}
	if !strings.Contains(js, "DeductionEndTime") {
		t.Fatal("明细面板应显式说明到期时间取自 DeductionEndTime")
	}
	if strings.Contains(js, "expiredTime") || strings.Contains(js, "ExpiredTime") {
		t.Fatal("前端不应把 ExpiredTime 当作积分到期依据")
	}
}

// 必须复用既有快照字段，不新增接口。
func TestCreditUIReusesSnapshotFields(t *testing.T) {
	js := readWebAsset(t, "app.js")
	for _, field := range []string{"creditKnown", "creditSoonestAt", "creditSoonestAmt", "creditPackages", "creditFetchedAt", "creditUnknownExpiry", "creditError"} {
		if !strings.Contains(js, field) {
			t.Fatalf("前端未使用快照字段 %s", field)
		}
	}
	if strings.Contains(js, "/admin/api/credit") {
		t.Fatal("不应新增积分相关接口，直接复用 /admin/api/status")
	}
}

// 分级阈值：7 天 / 3 天 / 1 天，以及长期与到期未知。
func TestCreditUILevelsAndStates(t *testing.T) {
	js := readWebAsset(t, "app.js")
	for _, want := range []string{`"≤1 天"`, `"≤3 天"`, `"≤7 天"`, `"长期"`, `"到期未知"`, `"已过期"`, `"无积分包"`} {
		if !strings.Contains(js, want) {
			t.Fatalf("缺少分级文案 %s", want)
		}
	}
	body := functionBody(t, js, "function creditInfo(account) {")
	for _, want := range []string{"days <= 1", "days <= 3", "days <= 7", "CREDIT_LONG_DAYS"} {
		if !strings.Contains(body, want) {
			t.Fatalf("分级判断缺少 %s", want)
		}
	}
}

// 凭据路径要转义后再进 DOM 属性，避免路径里的引号打断标记。
func TestCreditUIToggleEscapesPath(t *testing.T) {
	js := readWebAsset(t, "app.js")
	body := functionBody(t, js, "function creditCell(account) {")
	if !strings.Contains(body, "esc(account.path)") {
		t.Fatal("积分明细按钮必须对账号路径做 HTML 转义")
	}
	detail := functionBody(t, js, "function creditDetailRow(account) {")
	if !strings.Contains(detail, "esc(p.name") {
		t.Fatal("积分包名称必须转义")
	}
}

// functionBody 取函数体（按两空格缩进的收尾括号截断）。
func functionBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("找不到函数 %s", signature)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n  }\n")
	if end < 0 {
		t.Fatalf("函数 %s 结构异常", signature)
	}
	return rest[:end]
}

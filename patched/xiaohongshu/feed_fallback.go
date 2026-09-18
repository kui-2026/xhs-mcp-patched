package xiaohongshu

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
)

// pageDiagnosis is deliberately small and excludes HTML/cookies.  It gives us
// enough evidence to distinguish login/risk-control/page changes without
// leaking session material into logs.
type pageDiagnosis struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	StateKeys string `json:"stateKeys"`
}

func diagnoseFeedPage(page *rod.Page, operation string) error {
	res, err := page.Eval(`() => JSON.stringify({
		url: location.href,
		title: document.title || "",
		body: (document.body?.innerText || "").replace(/\s+/g, " ").slice(0, 500),
		stateKeys: Object.keys(window.__INITIAL_STATE__ || {}).join(",")
	})`)
	if err != nil {
		return fmt.Errorf("%s数据不可用 [PAGE_UNREADABLE]: %w", operation, err)
	}

	var d pageDiagnosis
	if err := json.Unmarshal([]byte(res.Value.Str()), &d); err != nil {
		return fmt.Errorf("%s数据不可用 [DIAGNOSIS_FAILED]: %w", operation, err)
	}
	d.Body = strings.TrimSpace(d.Body)
	code := classifyFeedPage(d)
	logrus.WithFields(logrus.Fields{
		"operation": operation,
		"code":      code,
		"url":       d.URL,
		"title":     d.Title,
		"stateKeys": d.StateKeys,
		"body":      d.Body,
	}).Warn("feed extraction failed")
	return fmt.Errorf("%s数据不可用 [%s]（页面标题：%s）", operation, code, d.Title)
}

func classifyFeedPage(d pageDiagnosis) string {
	text := strings.ToLower(d.URL + " " + d.Title + " " + d.Body)
	switch {
	case strings.Contains(text, "登录") || strings.Contains(text, "login") || strings.Contains(text, "扫码"):
		return "LOGIN_REQUIRED"
	case strings.Contains(text, "验证") || strings.Contains(text, "captcha") || strings.Contains(text, "安全") || strings.Contains(text, "异常") || strings.Contains(text, "访问频繁"):
		return "RISK_CONTROL"
	case strings.Contains(text, "暂无内容") || strings.Contains(text, "没有找到") || strings.Contains(text, "无结果"):
		return "EMPTY_RESULT"
	default:
		return "PAGE_CHANGED"
	}
}

// readFeedsFromDOM is the final local fallback after __INITIAL_STATE__.  It
// intentionally extracts only stable link data, never clicks cards or scrolls.
func readFeedsFromDOM(page *rod.Page) ([]Feed, error) {
	res, err := page.Eval(`() => {
		const seen = new Set();
		const out = [];
		for (const a of document.querySelectorAll('a[href*="/explore/"]')) {
			let u;
			try { u = new URL(a.href, location.href); } catch (_) { continue; }
			const m = u.pathname.match(/\/explore\/([a-zA-Z0-9]+)/);
			if (!m || seen.has(m[1])) continue;
			seen.add(m[1]);
			const card = a.closest('section, article, .note-item') || a;
			const img = card.querySelector('img');
			const title = (card.querySelector('.title, [class*="title"]')?.textContent || a.textContent || '').trim();
			out.push({
				id: m[1], modelType: 'note', xsecToken: u.searchParams.get('xsec_token') || '',
				noteCard: { displayTitle: title.slice(0, 200), cover: { urlDefault: img?.src || '' } }
			});
		}
		return JSON.stringify(out);
	}`)
	if err != nil {
		return nil, err
	}
	var feeds []Feed
	if err := json.Unmarshal([]byte(res.Value.Str()), &feeds); err != nil {
		return nil, err
	}
	return feeds, nil
}

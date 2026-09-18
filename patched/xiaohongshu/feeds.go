package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
)

type FeedsListAction struct{ page *rod.Page }

func NewFeedsListAction(page *rod.Page) *FeedsListAction {
	pp := page.Timeout(60 * time.Second)
	pp.MustNavigate("https://www.xiaohongshu.com")
	return &FeedsListAction{page: pp}
}

func (f *FeedsListAction) GetFeedsList(ctx context.Context) ([]Feed, error) {
	page := f.page.Context(ctx).Timeout(60 * time.Second)
	read := func() (string, error) {
		res, err := page.Eval(`() => {
			const f = window.__INITIAL_STATE__?.feed?.feeds;
			const v = f ? (f.value !== undefined ? f.value : f._value) : null;
			return v ? JSON.stringify(v) : "";
		}`)
		if err != nil {
			return "", err
		}
		return res.Value.Str(), nil
	}

	var result string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if result, err = read(); err == nil && result != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if result == "" {
		if feeds, err := readFeedsFromDOM(page); err == nil && len(feeds) > 0 {
			logrus.Warnf("首页状态数据缺失，已从页面链接恢复 %d 条笔记", len(feeds))
			return feeds, nil
		}
		return nil, diagnoseFeedPage(page, "首页")
	}
	var feeds []Feed
	if err := json.Unmarshal([]byte(result), &feeds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
	}
	return onlyNotes(feeds), nil
}

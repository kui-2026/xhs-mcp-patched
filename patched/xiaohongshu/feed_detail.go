package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// ========== 配置常量 ==========
const (
	defaultMaxAttempts   = 500
	stagnantLimit        = 20
	minScrollDelta       = 10
	maxClickPerRound     = 3
	largeScrollTrigger   = 5 // 停滞多少次后触发大滚动
	buttonClickInterval  = 3 // 每隔多少次尝试点击一次按钮
	finalSprintPushCount = 15

	// 以下三个只用于查找单条评论，与批量加载的 defaultMaxAttempts 不共用
	maxSearchScrolls  = 25               // 最多下滚轮数
	maxExpandRounds   = 5                // 最多连续展开而不下滚的轮数
	maxSearchDuration = 90 * time.Second // 单次查找的墙钟上限
)

// ========== 数据结构 ==========

type CommentLoadConfig struct {
	ClickMoreReplies    bool
	MaxRepliesThreshold int
	MaxCommentItems     int
	ScrollSpeed         string
}

// 未显式指定时的默认值。
const (
	defaultMaxCommentItems     = 20
	defaultMaxRepliesThreshold = 10
	defaultScrollSpeed         = "normal"
)

func DefaultCommentLoadConfig() CommentLoadConfig {
	return CommentLoadConfig{
		ClickMoreReplies:    false,
		MaxRepliesThreshold: defaultMaxRepliesThreshold,
		MaxCommentItems:     defaultMaxCommentItems,
		ScrollSpeed:         defaultScrollSpeed,
	}
}

// normalize 把零值字段填回默认值。零值一律按「未设置」处理，不再按「无上限」。
//
// 之所以必须在这一层做：配置从 MCP 和 HTTP 两条路进来，字段名和结构都不一样，
// 漏传很容易发生。HTTP 侧要的是嵌套的 comment_config，传扁平字段会被
// ShouldBindJSON 静默丢掉，于是 MaxCommentItems=0；而 0 以前表示「无上限」，
// 一次详情请求就会滚满 defaultMaxAttempts(500) 轮。放在 action 层而不是某个
// handler 里，两条路径和以后新增的调用方都能覆盖到。
//
// 真要拉更多评论，显式传一个大的 MaxCommentItems。
func (c CommentLoadConfig) normalize() CommentLoadConfig {
	if c.MaxCommentItems <= 0 {
		c.MaxCommentItems = defaultMaxCommentItems
	}
	if c.MaxRepliesThreshold <= 0 {
		c.MaxRepliesThreshold = defaultMaxRepliesThreshold
	}
	if c.ScrollSpeed == "" {
		c.ScrollSpeed = defaultScrollSpeed
	}
	return c
}

type FeedDetailAction struct {
	page *rod.Page
}

func NewFeedDetailAction(page *rod.Page) *FeedDetailAction {
	return &FeedDetailAction{page: page}
}

// ========== 主要业务逻辑 ==========

func (f *FeedDetailAction) GetFeedDetail(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config CommentLoadConfig) (*FeedDetailResponse, error) {
	return f.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, config)
}

func (f *FeedDetailAction) GetFeedDetailWithConfig(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config CommentLoadConfig) (*FeedDetailResponse, error) {
	config = config.normalize()

	requestCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	// Keep a page rooted in the caller context so a comment timeout cannot make
	// the final note snapshot impossible to read.
	page := f.page.Context(ctx)
	url := makeFeedDetailURL(feedID, xsecToken)

	logrus.Infof("打开 feed 详情页: %s", url)
	logrus.Infof("配置: 点击更多=%v, 回复阈值=%d, 最大评论数=%d, 滚动速度=%s",
		config.ClickMoreReplies, config.MaxRepliesThreshold, config.MaxCommentItems, config.ScrollSpeed)

	// Bound navigation independently; data readiness is checked by extraction,
	// not by waiting for the entire dynamic DOM to stop changing.
	err := retry.Do(
		func() error {
			return page.Context(requestCtx).Timeout(8 * time.Second).Navigate(url)
		},
		retry.Attempts(3),
		retry.Context(requestCtx),
		retry.Delay(500*time.Millisecond),
		retry.MaxJitter(1000*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("页面导航重试 #%d: %v", n, err)
		}),
	)
	if err != nil {
		logrus.Errorf("页面导航失败: %v", err)
		return nil, err
	}
	humanize.Delay(requestCtx, humanize.AfterNavigate)

	if err := rod.Try(func() { err = checkPageAccessible(page.Timeout(3 * time.Second)) }); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	// Snapshot before scrolling: a failed comment browser operation must not
	// discard a note that was already fetched successfully.
	result, err := f.extractFeedDetail(page.Timeout(5*time.Second), feedID)
	if err != nil || !loadAllComments {
		return result, err
	}
	commentCtx, stopComments := context.WithTimeout(requestCtx, 30*time.Second)
	defer stopComments()
	loader := &commentLoader{
		page: page.Context(commentCtx), config: config,
		stats: &loadStats{}, state: &loadState{},
		checkpoint: func() {
			if snapshot, e := f.extractFeedDetail(page.Timeout(2*time.Second), feedID); e == nil {
				result = snapshot
			}
		},
	}
	return finishCommentLoad(ctx, &result, func() error {
		return loader.load(commentCtx)
	}, func() (*FeedDetailResponse, error) {
		return f.extractFeedDetail(page.Timeout(2*time.Second), feedID)
	})
}

// Synchronous recovery avoids leaving a scrolling goroutine alive after timeout.
// result can be advanced by the loader's checkpoints before a later failure.
func finishCommentLoad(ctx context.Context, result **FeedDetailResponse, load func() error, snapshot func() (*FeedDetailResponse, error)) (*FeedDetailResponse, error) {
	var loadErr error
	if panicErr := rod.Try(func() { loadErr = load() }); panicErr != nil {
		loadErr = panicErr
	}
	// Respect caller cancellation; do not turn a cancelled request into success.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The comment deadline does not cancel this independent, bounded read.
	if latest, e := snapshot(); e == nil {
		*result = latest
	} else if loadErr == nil {
		loadErr = e
	}
	if loadErr != nil {
		(*result).CommentLoadWarning = "Comment loading interrupted; returning a saved snapshot. Comments and replies may be incomplete."
		logrus.Warnf("Returning partial feed detail after comment loading failed: %v", loadErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return *result, nil
}

// ========== 评论加载器 ==========

type commentLoader struct {
	checkpoint func()
	page       *rod.Page
	config     CommentLoadConfig
	stats      *loadStats
	state      *loadState
}

type loadStats struct {
	totalClicked int
	totalSkipped int
	attempts     int
}

type loadState struct {
	lastCount      int
	lastScrollTop  int
	stagnantChecks int
}

func (f *FeedDetailAction) loadAllCommentsWithConfig(ctx context.Context, page *rod.Page, config CommentLoadConfig) error {
	loader := &commentLoader{
		page:   page,
		config: config,
		stats:  &loadStats{},
		state:  &loadState{},
	}

	return loader.load(ctx)
}

func (cl *commentLoader) load(ctx context.Context) error {
	maxAttempts := cl.calculateMaxAttempts()

	logrus.Info("开始加载评论...")
	scrollToCommentsArea(cl.page)
	humanize.Delay(ctx, humanize.BetweenScroll)

	// 检查是否没有评论
	if cl.checkNoComments() {
		return nil
	}

	if cl.checkpoint != nil {
		cl.checkpoint()
	}

	for cl.stats.attempts = 0; cl.stats.attempts < maxAttempts; cl.stats.attempts++ {
		// 协作取消点：ctx 取消后干净退出。
		if err := ctx.Err(); err != nil {
			logrus.Infof("上下文已取消，停止加载评论: %v", err)
			return err
		}

		logrus.Debugf("=== 尝试 %d/%d ===", cl.stats.attempts+1, maxAttempts)

		if cl.checkComplete(ctx) {
			return nil
		}

		if cl.shouldClickButtons() {
			cl.clickButtonsWithRetry(ctx)
		}

		currentCount := getCommentCount(cl.page)
		if cl.updateState(currentCount) && cl.checkpoint != nil {
			// 只在评论数真的增长时做快照，避免每轮提取状态把 30s 预算耗掉。
			cl.checkpoint()
		}

		if cl.shouldStopAtTarget(currentCount) {
			return nil
		}

		cl.performScroll(ctx)
		cl.handleStagnation(ctx)

		if err := ctx.Err(); err != nil {
			return err
		}
		humanize.Delay(ctx, humanize.BetweenScroll)
	}

	cl.performFinalSprint(ctx)
	return nil
}

func (cl *commentLoader) calculateMaxAttempts() int {
	if cl.config.MaxCommentItems > 0 {
		return cl.config.MaxCommentItems * 3
	}
	return defaultMaxAttempts
}

func (cl *commentLoader) checkNoComments() bool {
	if checkNoCommentsArea(cl.page) {
		logrus.Infof("✓ 检测到无评论区域（这是一片荒地），跳过加载")
		return true
	}
	return false
}

func (cl *commentLoader) checkComplete(ctx context.Context) bool {
	if !checkEndContainer(cl.page) {
		return false
	}

	// 到底之后再展开一轮：评论区不用怎么滚就能到底时，循环第一轮就走到这里，
	// 主流程里的展开步骤根本没机会执行。点开了就先不算加载完，让下一轮重新判断
	// （展开会露出新的按钮）；一个都没点开就收工，避免被阈值跳过的按钮卡住。
	if cl.config.ClickMoreReplies && cl.clickButtonsWithRetry(ctx) > 0 {
		return false
	}

	currentCount := getCommentCount(cl.page)
	logrus.Infof("✓ 检测到 'THE END' 元素，已滑动到底部")
	humanize.Delay(ctx, humanize.BetweenScroll)
	logrus.Infof("✓ 加载完成: %d 条评论, 尝试次数: %d, 点击: %d, 跳过: %d",
		currentCount, cl.stats.attempts+1, cl.stats.totalClicked, cl.stats.totalSkipped)
	return true
}

func (cl *commentLoader) shouldClickButtons() bool {
	return cl.config.ClickMoreReplies && cl.stats.attempts%buttonClickInterval == 0
}

// clickButtonsWithRetry 展开当前页上的回复按钮，返回本次点开的个数。
func (cl *commentLoader) clickButtonsWithRetry(ctx context.Context) int {
	clicked, skipped := clickShowMoreButtonsSmart(ctx, cl.page, cl.config.MaxRepliesThreshold)
	if clicked == 0 && skipped == 0 {
		return 0
	}

	cl.stats.totalClicked += clicked
	cl.stats.totalSkipped += skipped
	logrus.Infof("点击'更多': %d 个, 跳过: %d 个, 累计点击: %d, 累计跳过: %d",
		clicked, skipped, cl.stats.totalClicked, cl.stats.totalSkipped)

	humanize.Delay(ctx, humanize.Reading)

	// 重试一轮
	clicked2, skipped2 := clickShowMoreButtonsSmart(ctx, cl.page, cl.config.MaxRepliesThreshold)
	if clicked2 > 0 || skipped2 > 0 {
		cl.stats.totalClicked += clicked2
		cl.stats.totalSkipped += skipped2
		logrus.Infof("第 2 轮: 点击 %d, 跳过 %d", clicked2, skipped2)
		humanize.Delay(ctx, humanize.Reading)
	}

	return clicked + clicked2
}

func (cl *commentLoader) updateState(currentCount int) bool {
	totalCount := getTotalCommentCount(cl.page)
	logrus.Debugf("当前评论: %d, 目标: %d", currentCount, totalCount)

	if currentCount != cl.state.lastCount {
		logrus.Infof("✓ 评论增加: %d -> %d (+%d)",
			cl.state.lastCount, currentCount, currentCount-cl.state.lastCount)
		cl.state.lastCount = currentCount
		cl.state.stagnantChecks = 0
		return true
	}

	cl.state.stagnantChecks++
	if cl.state.stagnantChecks%5 == 0 {
		logrus.Debugf("评论停滞 %d 次", cl.state.stagnantChecks)
	}
	return false
}

func (cl *commentLoader) shouldStopAtTarget(currentCount int) bool {
	// 如果未设置最大评论数，或者还未达到目标，继续加载
	if cl.config.MaxCommentItems <= 0 {
		return false
	}

	// 如果已达到或超过目标评论数，立即停止
	if currentCount >= cl.config.MaxCommentItems {
		logrus.Infof("✓ 已达到目标评论数: %d/%d, 停止加载",
			currentCount, cl.config.MaxCommentItems)
		return true
	}

	return false
}

func (cl *commentLoader) performScroll(ctx context.Context) {
	currentCount := getCommentCount(cl.page)
	if currentCount > 0 {
		scrollToLastComment(cl.page)
		time.Sleep(400 * time.Millisecond) // 技术 settle：等 scrollIntoView 动画落位
	}

	largeMode := cl.state.stagnantChecks >= largeScrollTrigger
	pushCount := 1
	if largeMode {
		pushCount = 3 + rand.Intn(3)
	}

	_, scrollDelta, currentScrollTop := humanScroll(ctx, cl.page, cl.config.ScrollSpeed, largeMode, pushCount)

	if scrollDelta < minScrollDelta || currentScrollTop == cl.state.lastScrollTop {
		cl.state.stagnantChecks++
		if cl.state.stagnantChecks%5 == 0 {
			logrus.Debugf("滚动停滞 %d 次", cl.state.stagnantChecks)
		}
	} else {
		cl.state.stagnantChecks = 0
		cl.state.lastScrollTop = currentScrollTop
	}
}

func (cl *commentLoader) handleStagnation(ctx context.Context) {
	if cl.state.stagnantChecks >= stagnantLimit {
		logrus.Infof("停滞过多，尝试大冲刺...")
		humanScroll(ctx, cl.page, cl.config.ScrollSpeed, true, 10)
		cl.state.stagnantChecks = 0

		if checkEndContainer(cl.page) {
			currentCount := getCommentCount(cl.page)
			logrus.Infof("✓ 到达底部，评论数: %d", currentCount)
		}
	}
}

func (cl *commentLoader) performFinalSprint(ctx context.Context) {
	logrus.Infof("达到最大尝试次数，最后冲刺...")
	humanScroll(ctx, cl.page, cl.config.ScrollSpeed, true, finalSprintPushCount)

	currentCount := getCommentCount(cl.page)
	hasEnd := checkEndContainer(cl.page)
	logrus.Infof("✓ 加载结束: %d 条评论, 点击: %d, 跳过: %d, 到达底部: %v",
		currentCount, cl.stats.totalClicked, cl.stats.totalSkipped, hasEnd)
}

// ========== 按钮点击 ==========

func clickShowMoreButtonsSmart(ctx context.Context, page *rod.Page, maxRepliesThreshold int) (clicked, skipped int) {
	elements, err := page.Elements(".show-more")
	if err != nil {
		return 0, 0
	}

	replyCountRegex := regexp.MustCompile(`展开\s*(\d+)\s*条回复`)
	maxClick := maxClickPerRound + rand.Intn(maxClickPerRound)
	clickedInRound := 0

	for _, el := range elements {
		if clickedInRound >= maxClick {
			break
		}

		if !isElementClickable(el) {
			continue
		}

		text, err := el.Text()
		if err != nil {
			continue
		}

		if !isSafeExpandButton(el, text) {
			continue
		}

		if shouldSkipButton(text, maxRepliesThreshold, replyCountRegex) {
			skipped++
			continue
		}

		if clickElementWithHumanBehavior(ctx, page, el, text) {
			clicked++
			clickedInRound++
		}
	}

	return clicked, skipped
}

// expandNearbyReplies 展开视口附近的「展开 N 条回复」，返回本轮点开的个数。
// 限定在视口附近，避免 ScrollIntoView 把页面拽回顶部、与向下滚动互相抵消。
func expandNearbyReplies(ctx context.Context, page *rod.Page) int {
	elements, err := page.Elements(".show-more")
	if err != nil || len(elements) == 0 {
		return 0
	}

	maxClick := maxClickPerRound + rand.Intn(maxClickPerRound)
	clicked := 0

	for _, el := range elements {
		if clicked >= maxClick {
			break
		}

		if !isElementClickable(el) || !isNearViewport(page, el) {
			continue
		}

		text, err := el.Text()
		if err != nil {
			continue
		}

		if !isSafeExpandButton(el, text) {
			continue
		}

		if clickElementWithHumanBehavior(ctx, page, el, text) {
			clicked++
		}
	}

	return clicked
}

// isSafeExpandButton 判断 .show-more 是不是展开回复按钮。
func isSafeExpandButton(el *rod.Element, text string) bool {
	if !isExpandRepliesButton(text) {
		logrus.Debugf("跳过展开按钮：文案不匹配 %q", text)
		return false
	}

	if !hasReadableSize(el) {
		logrus.Debugf("跳过展开按钮：尺寸过小 %q", text)
		return false
	}

	return true
}

// 两种文案：「展开 N 条回复」，以及点开一次后不带数字的「展开更多回复」。
var expandRepliesTextRegex = regexp.MustCompile(`^展开\s*(\d+\s*条|更多)回复$`)

func isExpandRepliesButton(text string) bool {
	return expandRepliesTextRegex.MatchString(strings.TrimSpace(text))
}

// hasReadableSize 判断元素尺寸是否达到按钮的量级。
func hasReadableSize(el *rod.Element) bool {
	const minWidth, minHeight = 24, 10

	shape, err := el.Shape()
	if err != nil || len(shape.Quads) == 0 {
		return false
	}

	q := shape.Quads[0] // 四个角点，左上 (q0,q1) 右下 (q4,q5)
	return q[4]-q[0] >= minWidth && q[5]-q[1] >= minHeight
}

// isNearViewport 判断元素是否落在视口上下各一屏的范围内。上下各留一屏是为了重叠，
// 滚动后刚划出去的元素下一轮还能被捡回来。
func isNearViewport(page *rod.Page, el *rod.Element) bool {
	shape, err := el.Shape()
	if err != nil || len(shape.Quads) == 0 {
		return false
	}

	// quads 是相对视口的 CSS 像素
	top := shape.Quads[0][1]
	height := float64(getViewportHeight(page))
	if height <= 0 {
		height = 800
	}

	return top > -height && top < 2*height
}

func isElementClickable(el *rod.Element) bool {
	visible, err := el.Visible()
	if err != nil || !visible {
		return false
	}

	box, err := el.Shape()
	return err == nil && len(box.Quads) > 0
}

func shouldSkipButton(text string, threshold int, regex *regexp.Regexp) bool {
	if threshold <= 0 {
		return false
	}

	matches := regex.FindStringSubmatch(text)
	if len(matches) > 1 {
		if replyCount, err := strconv.Atoi(matches[1]); err == nil && replyCount > threshold {
			logrus.Debugf("跳过'%s'（回复数 %d > 阈值 %d）", text, replyCount, threshold)
			return true
		}
	}
	return false
}

func clickElementWithHumanBehavior(ctx context.Context, page *rod.Page, el *rod.Element, text string) bool {
	var clickSuccess bool

	// 使用retry-go进行点击操作重试
	err := retry.Do(
		func() error {
			// 滚动到元素
			if err := el.ScrollIntoView(); err != nil {
				return err
			}

			humanize.Delay(ctx, humanize.Reading)

			// 点击（humanize.Click 自己取落点并移动过去）
			if err := humanize.Click(el); err != nil {
				return err // 返回错误以触发重试
			}

			humanize.Delay(ctx, humanize.Reading)
			clickSuccess = true
			return nil
		},
		retry.Attempts(3),
		retry.Delay(100*time.Millisecond),
		retry.MaxJitter(200*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("点击重试 #%d: %s, 错误: %v", n, text, err)
		}),
	)

	if err != nil {
		logrus.Debugf("点击失败 '%s': %v", text, err)
		return false
	}

	if clickSuccess {
		logrus.Debugf("点击了'%s'", text)
	}

	return clickSuccess
}

// ========== 滚动相关 ==========

func humanScroll(ctx context.Context, page *rod.Page, speed string, largeMode bool, pushCount int) (bool, int, int) {
	beforeTop := getScrollTop(page)
	viewportHeight := getViewportHeight(page)
	if viewportHeight <= 0 {
		viewportHeight = 800
	}

	baseRatio := getScrollRatio(speed)
	if largeMode {
		baseRatio *= 2.0
	}

	scrolled := false
	actualDelta := 0
	currentScrollTop := beforeTop

	for i := 0; i < max(1, pushCount); i++ {
		if err := ctx.Err(); err != nil {
			break
		}

		scrollDelta := calculateScrollDelta(viewportHeight, baseRatio)
		smartScroll(page, scrollDelta)

		time.Sleep(220 * time.Millisecond) // 等懒加载把新评论挂到 DOM
		currentScrollTop = getScrollTop(page)
		deltaThisTime := currentScrollTop - beforeTop
		actualDelta += deltaThisTime

		if deltaThisTime > 5 {
			scrolled = true
		}

		beforeTop = currentScrollTop

		if i < pushCount-1 {
			humanize.Delay(ctx, humanize.BetweenScroll)
		}
	}

	// 兜底：常规幅度没推动时，直接对实际评论滚动容器做一次大幅滚动。
	if !scrolled && pushCount > 0 && ctx.Err() == nil {
		smartScroll(page, float64(viewportHeight)*3)
		time.Sleep(450 * time.Millisecond)
		currentScrollTop = getScrollTop(page)
		actualDelta += currentScrollTop - beforeTop
		scrolled = actualDelta > 5
	}

	if scrolled {
		logrus.Debugf("滚动: %d -> %d (Δ%d, large=%v, push=%d)",
			currentScrollTop-actualDelta, currentScrollTop, actualDelta, largeMode, pushCount)
	}

	return scrolled, actualDelta, currentScrollTop
}

func getScrollRatio(speed string) float64 {
	switch speed {
	case "slow":
		return 0.5
	case "fast":
		return 0.9
	default: // normal
		return 0.7
	}
}

func calculateScrollDelta(viewportHeight int, baseRatio float64) float64 {
	scrollDelta := float64(viewportHeight) * (baseRatio + rand.Float64()*0.2)
	if scrollDelta < 400 {
		scrollDelta = 400
	}
	return scrollDelta + float64(rand.Intn(100)-50)
}

func scrollToCommentsArea(page *rod.Page) {
	logrus.Info("滚动到评论区...")

	_, _ = page.Eval(`() => {
		const el = document.querySelector(".comments-container");
		if (el) {
			el.scrollIntoView({block: "start", inline: "nearest", behavior: "instant"});
			return true;
		}
		return false;
	}`)
	time.Sleep(300 * time.Millisecond)

	// 触发一次小滚动，激活懒加载机制
	smartScroll(page, 120)
}

// smartScroll 优先直接滚动真正的评论容器，并显式派发 scroll 事件。
// 如果页面结构变化导致无法定位容器，再退回鼠标滚轮方案。
func smartScroll(page *rod.Page, delta float64) {
	if delta <= 0 {
		return
	}

	if moved, err := page.Eval(`(sels, delta) => {
		const pickScroller = () => {
			const comments = document.querySelector(".comments-container");
			if (comments) {
				let p = comments;
				while (p) {
					const style = getComputedStyle(p);
					const overflowY = style.overflowY;
					if ((overflowY === "auto" || overflowY === "scroll") &&
						p.scrollHeight > p.clientHeight + 4) {
						return p;
					}
					p = p.parentElement;
				}
			}

			for (const sel of sels) {
				const el = document.querySelector(sel);
				if (el && el.scrollHeight > el.clientHeight + 4) return el;
			}

			return document.scrollingElement || document.documentElement || document.body;
		};

		const el = pickScroller();
		if (!el) return false;

		const before = el.scrollTop || 0;
		el.scrollBy({top: delta, left: 0, behavior: "instant"});
		el.dispatchEvent(new Event("scroll", {bubbles: true}));
		const after = el.scrollTop || 0;
		return Math.abs(after - before) > 2;
	}`, commentScrollerSelectors, delta); err == nil && moved.Value.Bool() {
		return
	}

	// 鼠标滚轮 fallback：兼容只监听真实 wheel 的页面实现。
	moveToCommentScroller(page)
	for remain := delta; remain > 0; {
		notch := scrollNotchSize()
		if notch > remain {
			notch = remain
		}

		if err := page.Mouse.Scroll(0, notch, 1); err != nil {
			return
		}
		remain -= notch

		if remain > 0 {
			time.Sleep(scrollNotchInterval())
		}
	}
}

// scrollNotchSize 单格滚轮的幅度，围绕标准的 120px 浮动。
func scrollNotchSize() float64 {
	return 100 + rand.Float64()*40
}

// scrollNotchInterval 连续滚轮格之间的间隔。
func scrollNotchInterval() time.Duration {
	return time.Duration(20+rand.Intn(45)) * time.Millisecond
}

// commentScrollerSelectors 评论区滚动容器，按优先级排列。
// 实际滚动时还会从 comments-container 向上寻找最近的 overflow 容器。
var commentScrollerSelectors = []string{".note-scroller", ".comments-container"}

// moveToCommentScroller 把指针移到评论滚动容器内；找不到则退回视口中心。
func moveToCommentScroller(page *rod.Page) {
	for _, sel := range commentScrollerSelectors {
		el, err := page.Timeout(800 * time.Millisecond).Element(sel)
		if err != nil {
			continue
		}
		shape, err := el.Shape()
		if err != nil || len(shape.Quads) == 0 {
			continue
		}
		q := shape.Quads[0]
		left, top, right, bottom := q[0], q[1], q[4], q[5]

		if pos := page.Mouse.Position(); pos.X > left && pos.X < right && pos.Y > top && pos.Y < bottom {
			return
		}

		cx, cy := (left+right)/2, (top+bottom)/2
		_ = humanize.MoveTo(page, proto.Point{
			X: cx + (rand.Float64()-0.5)*(right-left)*0.3,
			Y: cy + (rand.Float64()-0.5)*(bottom-top)*0.3,
		})
		return
	}

	vw, vh := getViewportSize(page)
	if vw <= 0 {
		vw = 1280
	}
	if vh <= 0 {
		vh = 800
	}
	_ = humanize.MoveTo(page, proto.Point{X: float64(vw) / 2, Y: float64(vh) / 2})
}

func scrollToLastComment(page *rod.Page) {
	_, _ = page.Eval(`() => {
		const els = document.querySelectorAll(".parent-comment");
		if (!els.length) return false;
		els[els.length - 1].scrollIntoView({block: "end", inline: "nearest", behavior: "instant"});
		return true;
	}`)
}

// ========== DOM 查询 ==========

func getViewportHeight(page *rod.Page) int {
	res, err := page.Eval(`() => window.innerHeight || document.documentElement.clientHeight || 0`)
	if err != nil {
		return 0
	}
	return res.Value.Int()
}

func getViewportSize(page *rod.Page) (int, int) {
	wRes, wErr := page.Eval(`() => window.innerWidth || 0`)
	hRes, hErr := page.Eval(`() => window.innerHeight || 0`)
	if wErr != nil || hErr != nil {
		return 0, 0
	}
	return wRes.Value.Int(), hRes.Value.Int()
}

func getScrollTop(page *rod.Page) int {
	var result int

	err := retry.Do(
		func() error {
			evalResult, err := page.Eval(`(sels) => {
				const comments = document.querySelector(".comments-container");
				if (comments) {
					let p = comments;
					while (p) {
						const style = getComputedStyle(p);
						const overflowY = style.overflowY;
						if ((overflowY === "auto" || overflowY === "scroll") &&
							p.scrollHeight > p.clientHeight + 4) {
							return Math.round(p.scrollTop || 0);
						}
						p = p.parentElement;
					}
				}

				for (const sel of sels) {
					const el = document.querySelector(sel);
					if (el && el.scrollHeight > el.clientHeight + 4) {
						return Math.round(el.scrollTop || 0);
					}
				}
				return Math.round(
					window.pageYOffset ||
					document.documentElement.scrollTop ||
					document.body.scrollTop ||
					0
				);
			}`, commentScrollerSelectors)
			if err != nil {
				return err
			}

			result = evalResult.Value.Int()
			return nil
		},
		retry.Attempts(3),
		retry.Delay(80*time.Millisecond),
		retry.MaxJitter(120*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("获取滚动位置重试 #%d: %v", n, err)
		}),
	)

	if err != nil {
		logrus.Warnf("获取滚动位置失败: %v", err)
		return 0
	}

	return result
}

func getCommentCount(page *rod.Page) int {
	var result int

	err := retry.Do(
		func() error {
			res, err := page.Eval(`() => document.querySelectorAll(".parent-comment").length`)
			if err != nil {
				return err
			}
			result = res.Value.Int()
			return nil
		},
		retry.Attempts(3),
		retry.Delay(80*time.Millisecond),
		retry.MaxJitter(120*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("获取评论计数重试 #%d: %v", n, err)
		}),
	)

	if err != nil {
		logrus.Warnf("获取评论计数失败: %v", err)
		return 0
	}

	return result
}

// getTotalCommentCount 取笔记的评论总数，读 __INITIAL_STATE__ 里的
// interactInfo.commentCount，不依赖评论区文案。取不到返回 0。
func getTotalCommentCount(page *rod.Page) int {
	res, err := page.Eval(`() => {
		const m = window.__INITIAL_STATE__?.note?.noteDetailMap;
		if (!m) return "";
		for (const v of Object.values(m)) {
			const c = v?.note?.interactInfo?.commentCount;
			if (c !== undefined && c !== null) return String(c);
		}
		return "";
	}`)
	if err != nil {
		logrus.Debugf("获取总评论计数失败: %v", err)
		return 0
	}

	count, err := strconv.Atoi(strings.TrimSpace(res.Value.Str()))
	if err != nil {
		return 0
	}
	return count
}

func checkNoCommentsArea(page *rod.Page) bool {
	res, err := page.Eval(`() => {
		const el = document.querySelector(".no-comments-text");
		if (!el) return false;
		const text = (el.textContent || "").trim();
		return text.includes("这是一片荒地");
	}`)
	return err == nil && res.Value.Bool()
}

func checkEndContainer(page *rod.Page) bool {
	var result bool

	err := retry.Do(
		func() error {
			res, err := page.Eval(`(sels) => {
				const el = document.querySelector(".end-container");
				if (!el) return false;

				const text = (el.textContent || "").trim().toUpperCase();
				if (!(text.includes("THE END") || text.includes("THEEND"))) return false;

				const style = getComputedStyle(el);
				if (style.display === "none" || style.visibility === "hidden") return false;

				const rect = el.getBoundingClientRect();
				if (rect.width <= 0 || rect.height <= 0) return false;

				let scroller = null;
				const comments = document.querySelector(".comments-container");
				if (comments) {
					let p = comments;
					while (p) {
						const s = getComputedStyle(p);
						if ((s.overflowY === "auto" || s.overflowY === "scroll") &&
							p.scrollHeight > p.clientHeight + 4) {
							scroller = p;
							break;
						}
						p = p.parentElement;
					}
				}

				if (!scroller) {
					for (const sel of sels) {
						const candidate = document.querySelector(sel);
						if (candidate && candidate.scrollHeight > candidate.clientHeight + 4) {
							scroller = candidate;
							break;
						}
					}
				}

				const viewportTop = scroller ? scroller.getBoundingClientRect().top : 0;
				const viewportBottom = scroller ? scroller.getBoundingClientRect().bottom : window.innerHeight;

				// 只有 THE END 真正进入当前评论视口才算加载到底。
				return rect.bottom >= viewportTop - 8 && rect.top <= viewportBottom + 8;
			}`, commentScrollerSelectors)
			if err != nil {
				return err
			}
			result = res.Value.Bool()
			return nil
		},
		retry.Attempts(2),
		retry.Delay(80*time.Millisecond),
		retry.MaxJitter(120*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("检查结束容器重试 #%d: %v", n, err)
		}),
	)

	if err != nil {
		logrus.Warnf("检查结束容器失败: %v", err)
		return false
	}

	return result
}

// ========== 页面检查 ==========

func checkPageAccessible(page *rod.Page) error {
	// 等错误提示 UI 渲染出来再检查
	time.Sleep(500 * time.Millisecond)

	// 查找错误提示容器
	wrapperEl, err := page.Timeout(2 * time.Second).Element(".access-wrapper, .error-wrapper, .not-found-wrapper, .blocked-wrapper")
	if err != nil {
		// 未找到错误容器，说明页面可访问
		return nil
	}

	// 获取文本内容
	text, err := wrapperEl.Text()
	if err != nil {
		// 无法获取文本，假设页面可访问
		return nil
	}

	// 检查关键词
	keywords := []string{
		"当前笔记暂时无法浏览",
		"该内容因违规已被删除",
		"该笔记已被删除",
		"内容不存在",
		"笔记不存在",
		"已失效",
		"私密笔记",
		"仅作者可见",
		"因用户设置，你无法查看",
		"因违规无法查看",
	}

	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			logrus.Warnf("笔记不可访问: %s", kw)
			return fmt.Errorf("笔记不可访问: %s", kw)
		}
	}

	// 如果有文本但不匹配关键词，返回未知错误
	trimmedText := strings.TrimSpace(text)
	if trimmedText != "" {
		logrus.Warnf("笔记不可访问（未知原因）: %s", trimmedText)
		return fmt.Errorf("笔记不可访问: %s", trimmedText)
	}

	return nil
}

// ========== 数据提取 ==========

func (f *FeedDetailAction) extractFeedDetail(page *rod.Page, feedID string) (*FeedDetailResponse, error) {
	var result string

	// 使用retry-go来处理可能的DOM查询失败
	err := retry.Do(
		func() error {
			evaluated, evalErr := page.Eval(`() => {
				if (window.__INITIAL_STATE__ &&
					window.__INITIAL_STATE__.note &&
					window.__INITIAL_STATE__.note.noteDetailMap) {
					const noteDetailMap = window.__INITIAL_STATE__.note.noteDetailMap;
					return JSON.stringify(noteDetailMap);
				}
				return "";
			}`)
			if evalErr != nil {
				return evalErr
			}
			evalResult := evaluated.Value.String()

			if evalResult != "" {
				result = evalResult
				return nil
			}
			return fmt.Errorf("无法获取初始状态数据")
		},
		retry.Attempts(20),
		retry.Delay(200*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.Context(page.GetContext()),
		retry.MaxJitter(300*time.Millisecond),
		retry.OnRetry(func(n uint, err error) {
			logrus.Debugf("提取Feed详情重试 #%d: %v", n, err)
		}),
	)

	if err != nil {
		logrus.Errorf("提取Feed详情失败: %v", err)
		return nil, fmt.Errorf("提取Feed详情失败: %w", err)
	}

	if result == "" {
		return nil, errors.ErrNoFeedDetail
	}

	var noteDetailMap map[string]struct {
		Note     FeedDetail  `json:"note"`
		Comments CommentList `json:"comments"`
	}

	if err := json.Unmarshal([]byte(result), &noteDetailMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal noteDetailMap: %w", err)
	}

	noteDetail, exists := noteDetailMap[feedID]
	if !exists {
		return nil, fmt.Errorf("feed %s not found in noteDetailMap", feedID)
	}

	return &FeedDetailResponse{
		Note:     noteDetail.Note,
		Comments: noteDetail.Comments,
	}, nil
}

func makeFeedDetailURL(feedID, xsecToken string) string {
	return fmt.Sprintf("https://www.xiaohongshu.com/explore/%s?xsec_token=%s&xsec_source=pc_feed", feedID, xsecToken)
}

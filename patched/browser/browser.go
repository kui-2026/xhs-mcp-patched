package browser

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

type browserConfig struct {
	fingerprintSeed int
	proxy           string
	userDataDir     string
}

type Option func(*browserConfig)

func WithProxy(proxy string) Option { return func(c *browserConfig) { c.proxy = proxy } }

func WithFingerprintSeed(seed int) Option {
	return func(c *browserConfig) { c.fingerprintSeed = seed }
}

// WithUserDataDir keeps Chrome's profile (cookies, localStorage and completed
// verification state) across MCP process restarts. Empty preserves upstream's
// temporary-profile behaviour.
func WithUserDataDir(dir string) Option {
	return func(c *browserConfig) { c.userDataDir = strings.TrimSpace(dir) }
}

func maskProxyCredentials(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return proxyURL
	}
	cred := "***"
	if _, hasPassword := u.User.Password(); hasPassword {
		cred = "***:***"
	}
	return strings.Replace(proxyURL, u.User.String()+"@", cred+"@", 1)
}

func NewBrowser(headless bool, options ...Option) *headless_browser.Browser {
	cfg := &browserConfig{}
	for _, opt := range options {
		opt(cfg)
	}

	binPath, err := EnsureBrowser()
	if err != nil {
		panic(fmt.Sprintf("内置浏览器不可用，拒绝启动: %v", err))
	}

	extraFlags := map[string]string{"fingerprint-brand": "Chrome"}
	if cfg.userDataDir != "" {
		extraFlags["user-data-dir"] = cfg.userDataDir
		logrus.Infof("using persistent Chrome profile: %s", cfg.userDataDir)
	}
	opts := []headless_browser.Option{
		headless_browser.WithHeadless(headless),
		headless_browser.WithFingerprint(""),
		headless_browser.WithStealthJS(false),
		headless_browser.WithLanguage("zh-CN"),
		headless_browser.WithExtraFlags(extraFlags),
		headless_browser.WithChromeBinPath(binPath),
	}
	if cfg.proxy != "" {
		opts = append(opts, headless_browser.WithProxy(cfg.proxy))
		logrus.Infof("Using proxy: %s", maskProxyCredentials(cfg.proxy))
	}
	if cfg.fingerprintSeed > 0 {
		opts = append(opts, headless_browser.WithFingerprintSeed(cfg.fingerprintSeed))
		logrus.Infof("fingerprint seed pinned: %d", cfg.fingerprintSeed)
	}
	if data, err := cookies.NewLoadCookie(cookies.GetCookiesFilePath()).LoadCookies(); err == nil {
		opts = append(opts, headless_browser.WithCookies(string(data)))
		logrus.Debugf("loaded cookies from file successfully")
	} else {
		logrus.Warnf("failed to load cookies: %v", err)
	}
	return headless_browser.New(opts...)
}

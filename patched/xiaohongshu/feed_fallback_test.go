package xiaohongshu

import "testing"

func TestClassifyFeedPage(t *testing.T) {
	tests := []struct{ body, want string }{
		{"请扫码登录后继续", "LOGIN_REQUIRED"},
		{"访问频繁，请完成安全验证", "RISK_CONTROL"},
		{"没有找到相关内容", "EMPTY_RESULT"},
		{"ordinary page", "PAGE_CHANGED"},
	}
	for _, tt := range tests {
		if got := classifyFeedPage(pageDiagnosis{Body: tt.body}); got != tt.want {
			t.Fatalf("classify %q: got %s, want %s", tt.body, got, tt.want)
		}
	}
}

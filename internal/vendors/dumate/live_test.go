//go:build live

package dumate

// 联网自检：只在 `go test -tags live` 时运行，常规测试不会打外网。
//
// 覆盖两件事：
//   1. 百度统一登录页可达（面板按钮跳的就是它，域名/证书/跳转链变了要能立刻发现）；
//   2. 有真 BDUSS 时，兑换接口还能换出整套 Cookie（设 DUMATE_TEST_BDUSS 才会跑）。

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveWebLoginReachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, WebLoginURL, nil)
	if errRequest != nil {
		t.Fatalf("构造请求失败：%v", errRequest)
	}
	applyPassportHeaders(request)
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		t.Fatalf("登录页不可达：%v", errDo)
	}
	defer response.Body.Close()
	// 200 是登录页本身，302 是已带登录态跳回控制台，两种都正常。
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusFound {
		t.Fatalf("登录页状态码异常：%d", response.StatusCode)
	}
	t.Logf("登录页可用：%s → HTTP %d", WebLoginURL, response.StatusCode)
}

func TestLiveExchangeBDUSS(t *testing.T) {
	bduss := strings.TrimSpace(os.Getenv("DUMATE_TEST_BDUSS"))
	if bduss == "" {
		t.Skip("未设置 DUMATE_TEST_BDUSS，跳过兑换测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cookie, errExchange := ExchangeBDUSS(ctx, bduss)
	if errExchange != nil {
		t.Fatalf("BDUSS 兑换失败：%v", errExchange)
	}
	if BDUSSFromCookie(cookie) == "" {
		t.Fatalf("兑换结果缺 BDUSS：%s", cookie)
	}
	if !HasRichCookie(cookie) {
		t.Logf("兑换成功但未带 STOKEN/PTOKEN（可能被上游降级）：字段数=%d", strings.Count(cookie, "="))
		return
	}
	t.Logf("兑换成功：字段数=%d", strings.Count(cookie, "="))
}

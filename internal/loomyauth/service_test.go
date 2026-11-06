package loomyauth

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/**
 * @brief 验证短信签名、挑战绑定和登录身份均符合账号接口契约。
 * @param t 当前测试上下文。
 * @note 仅使用本地模拟服务，不发送真实短信。
 */
func TestSMSWireAndIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var payload struct {
			Base  map[string]string `json:"base"`  ///< 客户端公开身份及追踪字段。
			Param map[string]any    `json:"param"` ///< 手机号、验证码及挑战等账号业务参数。
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
			return
		}
		if r.Method != "POST" || payload.Param["phone"] != "13800138000" || payload.Base["appid"] != applicationID || payload.Base["traceid"] == "" {
			t.Errorf("unexpected login request: %+v", payload)
		}
		digest := md5.Sum(body)
		digest64 := base64.StdEncoding.EncodeToString(digest[:])
		if r.Header.Get("Content-MD5") != digest64 || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != ClientUA || r.Header.Get("token") != "" {
			t.Errorf("invalid signed headers")
		}
		canonical := "POST\n" + r.URL.EscapedPath() + "\n\n" + digest64 + "\napplication/json\n" + r.Header.Get("Date") + "\n" + r.Header.Get("Nonce") + "\n\n"
		mac := hmac.New(sha1.New, []byte(signKeySecret))
		_, _ = mac.Write([]byte(canonical))
		if r.Header.Get("Authorization") != "account "+signKeyID+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Error("signature mismatch")
		}
		switch r.URL.Path {
		case "/login/phone/sendMsgCode":
			_, _ = io.WriteString(w, `{"code":"000000","data":{"data":{"msgId":"challenge"}}}`)
		case "/login/phone/checkCode":
			if payload.Param["msgid"] != "challenge" || payload.Param["mcode"] != "123456" {
				t.Error("challenge binding lost")
			}
			_, _ = io.WriteString(w, `{"code":"000000","data":{"session":"session","userid":"user"}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	service := NewService(server.Client())
	service.AccountURL = server.URL
	msg, err := service.SendCode(context.Background(), "+86 138-0013-8000")
	if err != nil {
		t.Fatal(err)
	}
	c, err := service.Login(context.Background(), "13800138000", "123456", msg)
	if err != nil || c.UserID != "user" || c.Session != "session" {
		t.Fatalf("credential=%+v err=%v", c, err)
	}
}

/**
 * @brief 验证模型与积分响应格式、缺失值、业务拒绝和会话失效分类。
 * @param t 当前测试上下文。
 * @details 同时覆盖业务码去空白、WAF 不误判失效及错误文本不泄漏上游正文。
 */
func TestBusinessContracts(t *testing.T) {
	cases := []struct {
		name, body         string ///< name：子用例名称；body：模拟的上游 JSON 正文。
		status             int    ///< 模拟的上游 HTTP 状态。
		operation          string ///< 要调用的模型、余额、签到或任务操作。
		wantError, expired bool   ///< wantError：是否预期协议调用失败；expired：是否预期错误链包含 ErrSessionExpired。
	}{
		{"models without code", `{"data":[{"id":"chat"},{"id":"image","type":"image"}]}`, 200, "models", false, false},
		{"model array", `[{"id":"chat","type":"chat"}]`, 200, "models", false, false},
		{"missing balance stays unknown", `{"code":"000000","data":{"balance":12}}`, 200, "balance", false, false},
		{"missing balance", `{"code":"000000","data":{}}`, 200, "balance", true, false},
		{"missing checkin flag", `{"code":"000000","data":{}}`, 200, "checkin", true, false},
		{"already checked", `{"code":"000000","data":{"alreadyProcessed":true,"dailyCycleDate":"2026-11-04"}}`, 200, "checkin", false, false},
		{"missing tasks", `{"code":"000000","data":{"tasks":{}}}`, 200, "tasks", true, false},
		{"wrong task type", `{"code":"000000","data":{"tasks":{"first_message":"true"}}}`, 200, "tasks", true, false},
		{"expired business code", `{"code":"020002","desc":"private"}`, 200, "balance", true, true},
		{"padded expired code", `{"code":" 020002 ","desc":"private"}`, 200, "balance", true, true},
		{"padded success code", `{"code":" 000000 ","data":{"balance":12}}`, 200, "balance", false, false},
		{"unauthorized", `{}`, 401, "balance", true, true},
		{"WAF is not expiry", `{"code":"TM.00000001"}`, 403, "balance", true, false},
		{"missing business code", `{"data":{"balance":0}}`, 200, "balance", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("token") != "session" || r.Header.Get("Authorization") != "Bearer session" || !strings.Contains(r.Header.Get("Content-Type"), "utf-8") {
					t.Error("invalid service authentication")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			service := NewService(server.Client())
			service.BaseURL = server.URL
			var err error
			switch tc.operation {
			case "models":
				var names []string
				names, err = service.FetchModels(context.Background(), "session")
				if err == nil && (len(names) != 1 || names[0] != "chat") {
					t.Errorf("models=%v", names)
				}
			case "balance":
				var balance *Balance
				balance, err = service.FetchBalance(context.Background(), "session")
				if err == nil && (balance.Permanent == nil || *balance.Permanent != 12 || balance.Daily != nil || balance.Available != nil) {
					t.Errorf("balance=%+v", balance)
				}
			case "checkin":
				_, err = service.DailyCheckin(context.Background(), "session")
			case "tasks":
				_, err = service.FetchTasks(context.Background(), "session")
			}
			if (err != nil) != tc.wantError || errors.Is(err, ErrSessionExpired) != tc.expired {
				t.Fatalf("error=%v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

/**
 * @brief 验证每日积分汇总必须读取完整流水且只累加指定日期的 daily debit。
 * @param t 当前测试上下文。
 * @details 覆盖跨页、空流水、字段缺失、截断和六页上限。
 */
func TestDailyLedgerConsumption(t *testing.T) {
	for _, tc := range []struct {
		name        string   ///< 子用例名称。
		pages       []string ///< 按分页次序返回的模拟流水数据。
		want        float64  ///< 预期汇总的每日消费积分。
		found, fail bool     ///< found：是否预期发现目标业务日的 daily 流水；fail：是否预期分页或记录校验失败。
	}{
		{"complete pages", []string{
			`{"total":5,"list":[{"direction":"debit","consumeSource":"daily","dailyCycleDate":"2026-11-04","pointsActual":10},{"direction":"debit","consumeSource":"permanent","dailyCycleDate":"2026-11-04","pointsActual":500}]}`,
			`{"total":5,"list":[{"direction":"debit","consumeSource":"daily","dailyCycleDate":"2026-11-03","pointsActual":999},{"direction":"credit","consumeSource":"daily","dailyCycleDate":"2026-11-04","pointsActual":100},{"direction":"debit","consumeSource":"daily","dailyCycleDate":"2026-11-04","pointsActual":2.5}]}`,
		}, 12.5, true, false},
		{"empty ledger", []string{`{"total":0,"list":[]}`}, 0, false, false},
		{"missing date", []string{`{"total":1,"list":[{"direction":"debit","consumeSource":"daily","pointsActual":100}]}`}, 0, false, false},
		{"missing total", []string{`{"list":[]}`}, 0, false, true},
		{"truncated ledger", []string{`{"total":1,"list":[]}`}, 0, false, true},
		{"missing points", []string{`{"total":1,"list":[{"direction":"debit","consumeSource":"daily","dailyCycleDate":"2026-11-04"}]}`}, 0, false, true},
		{"page limit", []string{`{"total":1000,"list":[{"direction":"debit","consumeSource":"daily","dailyCycleDate":"2026-11-04","pointsActual":10}]}`}, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/points/records" ||
					r.URL.Query().Get("pageNo") != fmt.Sprint(calls) || r.URL.Query().Get("pageSize") != "100" ||
					r.URL.Query().Get("recordType") != "all" || r.Header.Get("token") != "session" {
					t.Error("invalid ledger request")
				}
				index := min(calls-1, len(tc.pages)-1)
				_, _ = fmt.Fprintf(w, `{"code":"000000","data":%s}`, tc.pages[index])
			}))
			defer server.Close()
			service := NewService(server.Client())
			service.BaseURL = server.URL
			consumed, found, err := service.FetchDailyConsumed(context.Background(), "session", "2026-11-04")
			if (err != nil) != tc.fail || consumed != tc.want || found != tc.found || calls > 6 {
				t.Fatalf("consumed=%v found=%v calls=%d err=%v", consumed, found, calls, err)
			}
		})
	}
}

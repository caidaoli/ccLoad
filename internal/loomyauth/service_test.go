package loomyauth

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
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

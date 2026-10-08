package main

import (
	"net/http"
	"testing"
)

func TestIsCloudflareChallenge(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		text   string
		want   bool
	}{
		{"cf-mitigated header", http.Header{"Cf-Mitigated": []string{"challenge"}}, `{"error":"x"}`, true},
		{"interstitial title", nil, "<html><title>Just a moment...</title></html>", true},
		{"attention required", nil, "<title>Attention Required! | Cloudflare</title>", true},
		{"managed challenge js", nil, `<script src="/cdn-cgi/challenge-platform/h/b/orchestrate">`, true},
		{"turnstile verify text", nil, "<div>Verifying you are human. This may take a few seconds.</div>", true},
		{"legacy browser verification", nil, "cf-browser-verification token", true},
		{"case-insensitive", nil, "PLEASE STAND BY, CHECKING YOUR BROWSER BEFORE ACCESSING", true},
		{"plain api error", nil, `{"error":{"message":"rate limited"}}`, false},
		{"aliyun captcha message", nil, `{"msg":"human verification required"}`, false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		if got := isCloudflareChallenge(c.header, c.text); got != c.want {
			t.Errorf("%s: isCloudflareChallenge = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsCaptchaError(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`{"msg":"captcha required"}`, true},
		{`{"msg":"invalid verify token"}`, true},
		{`{"msg":"HUMAN VERIFICATION NEEDED"}`, true},
		{"请完成人机验证后重试", true},
		{"安全验证失败", true},
		{`{"msg":"quota exhausted"}`, false},
		{"", false},
	}
	for _, c := range cases {
		if got := isCaptchaError(c.text); got != c.want {
			t.Errorf("isCaptchaError(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

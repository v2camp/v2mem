package scrub

import (
	"strings"
	"testing"
)

func TestApplyMatrix(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMIICdwIBADANBgkqhkiG9w0BAQ\n-----END PRIVATE KEY-----"
	cases := []struct {
		name string
		in   string
		want string // 空表示只断言不含秘密片段
		drop string
		keep string
	}{
		{"api_key", "note api_key=supersecret tail", "[redacted]", "supersecret", "note"},
		{"api-key colon", "API-KEY: abcdefgh tail", "[redacted]", "abcdefgh", ""},
		{"token assign", "token=abcd1234", "[redacted]", "abcd1234", ""},
		{"password spaces", "password = hunter2", "[redacted]", "hunter2", ""},
		{"authorization", "authorization: letmein", "[redacted]", "letmein", ""},
		{"sk", "key sk-12345678 end", "[redacted]", "sk-12345678", "key"},
		{"akia", "AKIAIOSFODNN7EXAMPLE", "[redacted]", "AKIAIOSFODNN7EXAMPLE", ""},
		{"bearer", "Bearer abcdefghij", "[redacted]", "abcdefghij", ""},
		{"pem", "before " + pem + " after", "[redacted]", "MIICdwIBADANBgkqhkiG9w0BAQ", "before"},
		{"url creds", "see https://user:secret@example.test/path", "https://[redacted]@example.test/path", "user:secret", "example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(tc.in, false)
			if tc.want != "" && tc.want != got && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want containing %q", got, tc.want)
			}
			if tc.drop != "" && strings.Contains(got, tc.drop) {
				t.Fatalf("秘密片段仍在: %q", got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Fatalf("不该被打掉的片段丢了: %q", got)
			}
		})
	}
}

func TestApplyLeavesBenignText(t *testing.T) {
	in := "讨论 token 预算与 password 策略，不赋值；链接 https://example.test/path ；短键 sk-short"
	if got := Text(in); got != in {
		t.Fatalf("良性文本被改写: %q", got)
	}
}

func TestApplyOffIsIdentity(t *testing.T) {
	in := "api_key=supersecret"
	if got := Apply(in, true); got != in {
		t.Fatalf("--no-scrub 应原样返回，got %q", got)
	}
	if got := Apply("", false); got != "" {
		t.Fatalf("空串应保持空，got %q", got)
	}
}

func TestTextIsIdempotent(t *testing.T) {
	in := "api_key=supersecret https://user:secret@example.test/a"
	once := Text(in)
	twice := Text(once)
	if once != twice {
		t.Fatalf("二次打码应幂等\n%s\n%s", once, twice)
	}
	if strings.Contains(twice, "supersecret") || strings.Contains(twice, "user:secret") {
		t.Fatalf("幂等后仍有秘密: %s", twice)
	}
}

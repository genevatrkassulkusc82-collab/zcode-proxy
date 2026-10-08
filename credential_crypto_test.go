package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestEncV1Roundtrip enc:v1 加解密自洽
func TestEncV1Roundtrip(t *testing.T) {
	secret := "zcode-credential-fallback:win32:C:\\Users\\test:test"
	for _, plain := range []string{"hello", "中文内容测试", `{"a":1}`, strings.Repeat("x", 500)} {
		enc, err := EncryptCredential(plain, secret)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if !strings.HasPrefix(enc, "enc:v1:") {
			t.Fatalf("missing prefix: %s", enc)
		}
		if strings.Count(enc, ".") != 2 {
			t.Fatalf("bad segment count: %s", enc)
		}
		got, err := DecryptCredential(enc, secret)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != plain {
			t.Fatalf("roundtrip mismatch")
		}
	}
}

// TestEncV1CrossLanguageVector 与 zcode-switch 测试向量互操作
// 向量来自 refs/zcode-switch/src-tauri/test-vectors/node-enc-v1.json（Node 客户端加密，Rust 解密验证）
func TestEncV1CrossLanguageVector(t *testing.T) {
	data, err := os.ReadFile(`refs/zcode-switch/src-tauri/test-vectors/node-enc-v1.json`)
	if err != nil {
		// 向量文件可能不存在于浅克隆，跳过
		t.Skipf("vector file missing: %v", err)
	}
	var v struct {
		Secret string `json:"secret"`
		Enc    string `json:"enc"`
		Plain  string `json:"plain"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	got, err := DecryptCredential(v.Enc, v.Secret)
	if err != nil {
		t.Fatalf("decrypt vector: %v", err)
	}
	if got != v.Plain {
		t.Fatalf("vector mismatch: got %q want %q", got, v.Plain)
	}
}

// TestDefaultSecretFormat 回退密钥格式
func TestDefaultSecretFormat(t *testing.T) {
	os.Unsetenv("ZCODE_CREDENTIAL_SECRET")
	s := DefaultCredentialSecret(`C:\Users\john`)
	if !strings.HasPrefix(s, "zcode-credential-fallback:") {
		t.Fatalf("bad prefix: %s", s)
	}
	if !strings.Contains(s, `C:\Users\john`) {
		t.Fatalf("home not embedded: %s", s)
	}
}

// TestLooksLikeJWT JWT 形状判定
func TestLooksLikeJWT(t *testing.T) {
	if !LooksLikeJWT("eyJhbGci.eyJzdWIi.c2ln") {
		t.Fatal("should be jwt")
	}
	if LooksLikeJWT("sk-abc123") {
		t.Fatal("api key misdetected as jwt")
	}
	if LooksLikeJWT("a.b.") {
		t.Fatal("empty segment accepted")
	}
}

// TestDecodeJWTPayload JWT payload 解析
func TestDecodeJWTPayload(t *testing.T) {
	// payload = {"user_id":"123456","sub":"s"}
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoiMTIzNDU2Iiwic3ViIjoicyJ9.c2ln"
	claims, err := DecodeJWTPayload(jwt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims["user_id"] != "123456" {
		t.Fatalf("bad claim: %v", claims)
	}
}

// TestImportBundleLegacy120k 旧版 120k 轮次的包经 legacy 路径兼容导入
func TestImportBundleLegacy120k(t *testing.T) {
	// 条目缺 user_id，导入循环跳过，不触数据库
	plain, _ := json.Marshal(map[string]interface{}{"version": 1, "accounts": []bundleAccount{
		{Email: "legacy@test"},
	}})
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("salt: %v", err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	gcm, err := bundleGCM("legacy-pw", salt, legacyPBKDF2Iterations)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	raw := append(append(append([]byte{}, salt...), nonce...), ct...)
	bundle := bundlePrefix + base64.StdEncoding.EncodeToString(raw)

	m := &AccountManager{}
	n, err := m.ImportBundle("legacy-pw", bundle)
	if err != nil {
		t.Fatalf("legacy 120k import failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 imported, got %d", n)
	}
	if _, err := m.ImportBundle("wrong-pw", bundle); err == nil {
		t.Fatal("wrong password should be rejected")
	}
}

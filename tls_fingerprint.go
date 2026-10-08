package main

import (
	"context"
	stdtls "crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"

	tls "github.com/refraction-networking/utls"
)

// ---- TLS 指纹伪装（utls）----
// ZCode 桌面端是 Electron(Chromium)，Go 默认 crypto/tls 的 ClientHello 指纹差异明显，
// 易被上游 ESA WAF 识别为机器程序。用 utls 模拟浏览器 TLS 指纹，支持预置与 JA3 自定义。
// TLS 指纹伪装（utls）：模拟浏览器 ClientHello，降低机器特征评分。

// TLSFingerprint 当前生效的指纹配置
type TLSFingerprint struct {
	Mode string `json:"mode"` // off|chrome|firefox|...|custom
	JA3  string `json:"ja3"`  // mode=custom 时的 JA3 字符串
}

// tlsFingerprintPresets 可选指纹模式（供前端下拉）
var tlsFingerprintPresets = []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
}{
	{"off", "关闭（Go 默认指纹）", "基础"},
	{"randomized", "随机化指纹", "基础"},
	{"chrome", "Chrome 浏览器（自动最新）", "浏览器"},
	{"chrome_100", "Chrome 100", "浏览器"},
	{"chrome_83", "Chrome 83（旧版 Electron 常见）", "浏览器"},
	{"firefox", "Firefox 浏览器（120）", "浏览器"},
	{"safari", "Safari 浏览器（16.0）", "浏览器"},
	{"edge", "Edge 浏览器（85）", "浏览器"},
	{"qq", "QQ 浏览器（11.1）", "浏览器"},
	{"q360", "360 浏览器（7.5）", "浏览器"},
	{"bun", "Bun.js（BoringSSL）", "运行时与工具"},
	{"node", "Node.js（OpenSSL）", "运行时与工具"},
	{"deno", "Deno（rustls）", "运行时与工具"},
	{"curl", "curl（OpenSSL）", "运行时与工具"},
	{"python", "Python requests（OpenSSL）", "运行时与工具"},
	{"okhttp", "OkHttp 3（Android）", "运行时与工具"},
	{"golang", "Go 标准库（crypto/tls）", "运行时与工具"},
	{"ios", "iOS Safari（14）", "移动端"},
	{"android", "Android OkHttp（11）", "移动端"},
	{"custom", "自定义 JA3 指纹", "自定义"},
}

// builtinJA3 非浏览器客户端的公开 JA3 指纹（无 utls 官方预设，转 ClientHelloSpec 还原）
var builtinJA3 = map[string]string{
	"bun":    "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513,29-23-24,0",
	"node":   "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513,29-23-24,0",
	"deno":   "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27,29-23-24,0",
	"curl":   "769,49195-49196-49199-49200-52393-52392-158-159-49161-49162-49171-49172-51-57-47-53,0-11-10-13,23-24-25,0",
	"python": "769,49195-49196-49199-49200-52393-52392-158-159-49161-49162-49171-49172-51-57-47-53,0-11-10-16-13,23-24-25,0",
	"okhttp": "771,49199-49195-52393-49196-49200-49162-49161-52392-49171-49172-156-157-47-53,0-11-10-35-16-5-13-18-51-45-43-27-23-17,29-23-24,0",
}

// fingerprintHook 由 main 注入：返回当前指纹配置
var fingerprintHook = func() TLSFingerprint { return TLSFingerprint{Mode: "chrome"} }

// isValidFingerprint 判断模式是否为合法预置（custom 亦合法）
func isValidFingerprint(mode string) bool {
	if mode == "custom" || mode == "off" {
		return true
	}
	for _, p := range tlsFingerprintPresets {
		if p.ID == mode {
			return true
		}
	}
	if _, ok := builtinJA3[mode]; ok {
		return true
	}
	return false
}

func presetHelloID(mode string) (tls.ClientHelloID, bool) {
	switch mode {
	case "chrome":
		return tls.HelloChrome_Auto, true
	case "chrome_100":
		return tls.HelloChrome_100, true
	case "chrome_83":
		return tls.HelloChrome_83, true
	case "firefox":
		return tls.HelloFirefox_Auto, true
	case "safari":
		return tls.HelloSafari_Auto, true
	case "edge":
		return tls.HelloEdge_Auto, true
	case "qq":
		return tls.HelloQQ_Auto, true
	case "q360":
		return tls.Hello360_Auto, true
	case "ios":
		return tls.HelloIOS_Auto, true
	case "android":
		return tls.HelloAndroid_11_OkHttp, true
	case "randomized":
		return tls.HelloRandomized, true
	case "randomized_alpn":
		return tls.HelloRandomizedALPN, true
	case "golang":
		return tls.HelloGolang, true
	}
	return tls.HelloChrome_Auto, false
}

// utlsHandshake 对已建立的原始连接按指纹配置做 TLS 握手。
// rawConn 可以是直连 TCP，也可以是经 SOCKS5/HTTP 代理隧道后的连接。
func utlsHandshake(ctx context.Context, rawConn net.Conn, serverName string, fp TLSFingerprint) (net.Conn, error) {
	if fp.Mode == "" || fp.Mode == "off" {
		c := stdtls.Client(rawConn, &stdtls.Config{ServerName: serverName, MinVersion: stdtls.VersionTLS12})
		if err := c.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		return c, nil
	}
	helloID, _ := presetHelloID(fp.Mode)
	uconn := tls.UClient(rawConn, &tls.Config{ServerName: serverName}, helloID)
	ja3Str := ""
	if fp.Mode == "custom" {
		ja3Str = strings.TrimSpace(fp.JA3)
	} else if builtin, ok := builtinJA3[fp.Mode]; ok {
		ja3Str = builtin
	}
	if ja3Str != "" {
		spec, err := ja3ToClientHelloSpec(ja3Str)
		if err != nil {
			rawConn.Close()
			return nil, err
		}
		uconn.ClientHelloID = tls.HelloCustom
		if err := uconn.ApplyPreset(spec); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("指纹无效: %w", err)
		}
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	// 指纹传输层禁用了 h2（TLSNextProto 置空），而 utls 的 ConnectionState
	// 类型 net/http 无法识别——若服务器据 ALPN 选中 h2，HTTP/1.1 字节写进
	// h2 流只会得到难以排查的畸形响应，这里显式快速失败
	if proto := uconn.ConnectionState().NegotiatedProtocol; proto == "h2" {
		rawConn.Close()
		return nil, fmt.Errorf("上游协商了 h2，指纹传输层仅支持 http/1.1")
	}
	return uconn, nil
}

// ---- JA3 解析：JA3 字符串 → utls.ClientHelloSpec ----

func ja3ToClientHelloSpec(ja3 string) (*tls.ClientHelloSpec, error) {
	ja3 = strings.TrimSpace(ja3)
	parts := strings.Split(ja3, ",")
	if len(parts) != 5 {
		return nil, fmt.Errorf("JA3 需为 5 段（版本,密码套件,扩展,曲线,点格式）")
	}
	ciphers, err := parseU16List(parts[1])
	if err != nil {
		return nil, fmt.Errorf("密码套件段: %w", err)
	}
	if len(ciphers) == 0 {
		return nil, fmt.Errorf("密码套件列表为空")
	}
	extIDs, err := parseU16List(parts[2])
	if err != nil {
		return nil, fmt.Errorf("扩展段: %w", err)
	}
	curves, err := parseU16List(parts[3])
	if err != nil {
		return nil, fmt.Errorf("曲线段: %w", err)
	}
	pointFmts, err := parseU8List(parts[4])
	if err != nil {
		return nil, fmt.Errorf("点格式段: %w", err)
	}

	spec := &tls.ClientHelloSpec{
		CipherSuites:       ciphers,
		CompressionMethods: []byte{0x00},
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
	}
	hasExt := func(id uint16) bool {
		for _, x := range extIDs {
			if x == id {
				return true
			}
		}
		return false
	}
	var exts []tls.TLSExtension
	exts = append(exts, &tls.SNIExtension{})
	for _, id := range extIDs {
		switch id {
		case 0:
		case 5:
			exts = append(exts, &tls.StatusRequestExtension{})
		case 10:
			exts = append(exts, &tls.SupportedCurvesExtension{Curves: toCurveIDs(curves)})
		case 11:
			exts = append(exts, &tls.SupportedPointsExtension{SupportedPoints: pointFmts})
		case 13:
			exts = append(exts, &tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: defaultSigAlgs()})
		case 16:
			exts = append(exts, &tls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}})
		case 18:
			exts = append(exts, &tls.SCTExtension{})
		case 23:
			exts = append(exts, &tls.ExtendedMasterSecretExtension{})
		case 27:
			exts = append(exts, &tls.UtlsCompressCertExtension{Algorithms: []tls.CertCompressionAlgo{tls.CertCompressionBrotli}})
		case 35:
			exts = append(exts, &tls.SessionTicketExtension{})
		case 43:
			exts = append(exts, &tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}})
		case 45:
			exts = append(exts, &tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}})
		case 51:
		case 65281:
			exts = append(exts, &tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient})
		}
	}
	if hasExt(51) {
		exts = append(exts, &tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}})
	}
	spec.Extensions = exts
	return spec, nil
}

func toCurveIDs(v []uint16) []tls.CurveID {
	out := make([]tls.CurveID, len(v))
	for i, x := range v {
		out[i] = tls.CurveID(x)
	}
	return out
}

func defaultSigAlgs() []tls.SignatureScheme {
	return []tls.SignatureScheme{
		tls.ECDSAWithP256AndSHA256, tls.ECDSAWithP384AndSHA384, tls.ECDSAWithP521AndSHA512,
		tls.PSSWithSHA256, tls.PSSWithSHA384, tls.PSSWithSHA512,
		tls.PKCS1WithSHA256, tls.PKCS1WithSHA384, tls.PKCS1WithSHA512,
		tls.ECDSAWithSHA1, tls.PKCS1WithSHA1,
	}
}

// parseU16List 解析 JA3 数值列表；任何非法 token 都报错——静默跳过会产出
// 缺字段的 ClientHello（如空 cipher 套件），握手必败且难以排查
func parseU16List(s string) ([]uint16, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []uint16
	for _, p := range strings.Split(s, "-") {
		n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 16)
		if err != nil {
			return nil, fmt.Errorf("非法数值 %q（应为 0-65535）", p)
		}
		out = append(out, uint16(n))
	}
	return out, nil
}

func parseU8List(s string) ([]uint8, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []uint8
	for _, p := range strings.Split(s, "-") {
		n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("非法数值 %q（应为 0-255）", p)
		}
		out = append(out, uint8(n))
	}
	return out, nil
}

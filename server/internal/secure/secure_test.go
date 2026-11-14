package secure

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 向量由 scripts/gen_parity.py 用 Python 版实现生成：Go 侧必须逐字节对上，
// 否则线上表现就是"接口返回 success=false，msg 为空"，从日志里根本看不出原因。
type aesVectors struct {
	CBCParam []struct {
		JSON  string `json:"json"`
		Plain string `json:"plain"`
		B64   string `json:"b64"`
	} `json:"cbc_param"`
	ECBPassword []struct {
		CroyptoB64 string `json:"croypto_b64"`
		Password   string `json:"password"`
		OutB64     string `json:"out_b64"`
	} `json:"ecb_password"`
}

func loadAES(t *testing.T) aesVectors {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "aes_vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取向量失败 %s: %v", path, err)
	}
	var v aesVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解析向量失败: %v", err)
	}
	if len(v.CBCParam) == 0 || len(v.ECBPassword) == 0 {
		t.Fatal("向量为空，先运行 scripts/gen_parity.py")
	}
	return v
}

// Python 加密的密文，Go 必须能解回同一个明文（key/IV/PKCS7/Base64 全对）。
func TestDecryptPythonVectors(t *testing.T) {
	for i, item := range loadAES(t).CBCParam {
		got, err := DecryptParams(item.B64)
		if err != nil {
			t.Fatalf("第 %d 组解密失败: %v", i, err)
		}
		if got != item.Plain {
			t.Fatalf("第 %d 组明文不一致:\n got=%q\nwant=%q", i, got, item.Plain)
		}
	}
}

// Go 加密的密文必须与 Python 完全相同（含字段顺序与 ensure_ascii 转义）。
// 这里只用 struct 构造载荷：Go 的 map 会按键名排序序列化，字段顺序天然不同，
// 而最终提交给对端的就是这段明文，所以生产代码里 payload 一律用 struct。
func TestEncryptByteParityWithPython(t *testing.T) {
	vectors := loadAES(t).CBCParam

	type checkPayload struct {
		MapType   string  `json:"mapType"`
		Longitude float64 `json:"longitude"`
		Latitude  float64 `json:"latitude"`
		Range     string  `json:"range"`
	}
	// 与 src/checkin.py 的 check.action payload 同序
	check := checkPayload{
		MapType:   "amap",
		Longitude: 119.20463,
		Latitude:  26.0492,
		Range:     `{"range":[[[119.2,26.0],[119.3,26.0],[119.3,26.1]]]}`,
	}
	if got, err := EncryptParams(check); err != nil {
		t.Fatalf("加密 check payload 失败: %v", err)
	} else if got != vectors[1].B64 {
		t.Fatalf("check payload 密文不一致:\n got=%s\nwant=%s", got, vectors[1].B64)
	}

	type clockInPayload struct {
		Campus         string  `json:"campus"`
		Lon            float64 `json:"lon"`
		Lat            float64 `json:"lat"`
		StartTime      string  `json:"startTime"`
		EndTime        string  `json:"endTime"`
		ActualLocation string  `json:"actualLocation"`
		CaIsNo         string  `json:"caIsNo"`
		CollUnit       string  `json:"collUnit"`
		Way            int     `json:"way"`
		IsLate         int     `json:"isLate"`
	}
	// 与 src/checkin.py 的 clock_in payload 同序；含中文地址，验证 ensure_ascii 行为
	clockIn := clockInPayload{
		Campus:         "c-1",
		Lon:            119.20471,
		Lat:            26.04912,
		StartTime:      "21:30",
		EndTime:        "23:59",
		ActualLocation: "福州大学旗山校区生活区3号楼",
		CaIsNo:         "0",
		CollUnit:       "u-9",
		Way:            1,
		IsLate:         0,
	}
	if got, err := EncryptParams(clockIn); err != nil {
		t.Fatalf("加密 clockIn payload 失败: %v", err)
	} else if got != vectors[2].B64 {
		t.Fatalf("clockIn payload 密文不一致:\n got=%s\nwant=%s", got, vectors[2].B64)
	}
}

// 非 ASCII 必须被转成 \uXXXX：Python 默认 ensure_ascii=True，前端也是同样的转义，
// 直接输出 UTF-8 会让"两边密文一致"这条回归断言失效（语义仍等价，但排查成本高）。
func TestPyMarshalEscapesNonASCII(t *testing.T) {
	got, err := PyMarshal(map[string]string{"t": "中文 ✅"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	want := `{"t":"\u4e2d\u6587 \u2705"}`
	if string(got) != want {
		t.Fatalf("转义结果不一致:\n got=%s\nwant=%s", got, want)
	}
	if strings.ContainsAny(string(got), "中文") {
		t.Fatal("非 ASCII 字符没有被转义")
	}

	// BMP 之外（emoji 之外的四字节字符）要拆成代理对
	got2, err := PyMarshal("𠮷")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(got2) != `"\ud842\udfb7"` {
		t.Fatalf("代理对处理不正确: %s", got2)
	}

	// HTML 特殊字符不能转义（Python 的 json.dumps 不转义 <>&）
	got3, err := PyMarshal("<a>&b</a>")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(got3) != `"<a>&b</a>"` {
		t.Fatalf("HTML 字符被错误转义: %s", got3)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	payload := map[string]any{
		"lon": 119.20463, "lat": 26.0492,
		"note": "往返一致性", "n": 42, "ok": true,
	}
	cipherText, err := EncryptParams(payload)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	plain, err := DecryptParams(cipherText)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(plain), &back); err != nil {
		t.Fatalf("明文不是合法 JSON: %v (%s)", err, plain)
	}
	if back["note"] != "往返一致性" {
		t.Fatalf("往返后内容变化: %+v", back)
	}
}

func TestEncryptECBVectors(t *testing.T) {
	for i, item := range loadAES(t).ECBPassword {
		key, err := base64.StdEncoding.DecodeString(item.CroyptoB64)
		if err != nil {
			t.Fatalf("第 %d 组 croypto 解码失败: %v", i, err)
		}
		out, err := EncryptECBBlock(key, []byte(item.Password))
		if err != nil {
			t.Fatalf("第 %d 组 ECB 加密失败: %v", i, err)
		}
		if got := base64.StdEncoding.EncodeToString(out); got != item.OutB64 {
			t.Fatalf("第 %d 组 ECB 密文不一致:\n got=%s\nwant=%s", i, got, item.OutB64)
		}
	}
}

func TestEncryptECBRejectsBadKey(t *testing.T) {
	if _, err := EncryptECBBlock([]byte("short"), []byte("x")); err == nil {
		t.Fatal("非法长度密钥应当报错")
	}
}

// RFC 定型的 PBKDF2-HMAC-SHA256 向量（password/salt，迭代 1/2/4096）。
func TestPBKDF2KnownVectors(t *testing.T) {
	cases := []struct {
		iter int
		want string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(PBKDF2SHA256([]byte("password"), []byte("salt"), c.iter, 32))
		if got != c.want {
			t.Fatalf("迭代 %d 结果不一致:\n got=%s\nwant=%s", c.iter, got, c.want)
		}
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	if !strings.HasPrefix(encoded, hashPrefix) {
		t.Fatalf("哈希前缀不对: %s", encoded)
	}
	if !VerifyPassword(encoded, "hunter2") {
		t.Fatal("正确密码校验失败")
	}
	if VerifyPassword(encoded, "hunter3") {
		t.Fatal("错误密码竟然通过")
	}
	for _, broken := range []string{"", "pbkdf2$sha256$", "pbkdf2$sha256$x$y$z", "md5$1$2$3"} {
		if VerifyPassword(broken, "hunter2") {
			t.Fatalf("损坏的哈希 %q 竟然通过校验", broken)
		}
	}
	// 同一密码两次生成的盐不同
	other, _ := HashPassword("hunter2")
	if other == encoded {
		t.Fatal("两次生成的哈希不应相同（盐没随机）")
	}
}

func TestSigner(t *testing.T) {
	s := Signer{Key: []byte("0123456789abcdef0123456789abcdef")}
	msg := []byte(`{"sub":"admin","exp":1763000000}`)
	sig := s.Sign(msg)
	if !s.Verify(msg, sig) {
		t.Fatal("签名校验失败")
	}
	if s.Verify([]byte(`{"sub":"admin","exp":1763000001}`), sig) {
		t.Fatal("篡改载荷后仍通过校验")
	}
	if s.Verify(msg, sig[:len(sig)-2]+"AA") {
		t.Fatal("篡改签名后仍通过校验")
	}
}

func TestPKCS7(t *testing.T) {
	for n := 0; n <= 32; n++ {
		data := make([]byte, n)
		padded := PKCS7Pad(data, 16)
		if len(padded)%16 != 0 || len(padded) <= n {
			t.Fatalf("长度 %d 补位后异常: %d", n, len(padded))
		}
		back, err := PKCS7Unpad(padded, 16)
		if err != nil || len(back) != n {
			t.Fatalf("长度 %d 去补位失败: %v (len=%d)", n, err, len(back))
		}
	}
	if _, err := PKCS7Unpad([]byte{}, 16); err == nil {
		t.Fatal("空数据去补位应当报错")
	}
	if _, err := PKCS7Unpad([]byte{1, 2, 3}, 16); err == nil {
		t.Fatal("非整块数据去补位应当报错")
	}
}

func TestRandomHex(t *testing.T) {
	a, b := RandomHex(16), RandomHex(16)
	if len(a) != 32 {
		t.Fatalf("长度不对: %d", len(a))
	}
	if a == b {
		t.Fatal("两次随机数相同")
	}
}

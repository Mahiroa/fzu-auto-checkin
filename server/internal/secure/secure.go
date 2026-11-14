// Package secure 提供本服务需要的全部密码学原语，只用标准库。
//
// 为什么集中在一个包里：AES 参数加密必须与前端 JS 的 Encrypt() **逐字节一致**，
// 一旦有人在下游自己拼 JSON 或自己 base64，加密结果就悄悄变了、服务端返回一个
// 语焉不详的失败。把"明文怎么序列化 → 怎么补位 → 怎么加密 → 怎么编码"这条链
// 封死在这里，下游只允许调用 EncryptParams。
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// paramsKey 是晚点名 H5 前端硬编码的 AES 密钥，KEY == IV（源码里就是这么写的）。
// 这不是"我们自己的密钥"，而是对端协议的一部分，改一个字节服务端就解不开。
var paramsKey = []byte("apexinfoapexinfo")

// ErrBadCiphertext 密文长度不合法（不是 16 的倍数）。
var ErrBadCiphertext = errors.New("密文长度不是块大小的整数倍")

// ---------- PKCS7 ----------

// PKCS7Pad 按 block 字节补位；已经对齐时补满一整块（与 PyCryptodome 一致）。
func PKCS7Pad(data []byte, block int) []byte {
	if block <= 0 {
		block = aes.BlockSize
	}
	pad := block - len(data)%block
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// PKCS7Unpad 校验并去掉补位；补位非法一律报错，绝不"尽力而为"地返回半截明文。
func PKCS7Unpad(data []byte, block int) ([]byte, error) {
	if block <= 0 {
		block = aes.BlockSize
	}
	if len(data) == 0 || len(data)%block != 0 {
		return nil, ErrBadCiphertext
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > block || pad > len(data) {
		return nil, errors.New("PKCS7 补位非法")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS7 补位非法")
		}
	}
	return data[:len(data)-pad], nil
}

// ---------- 参数加密（AES-128-CBC, KEY == IV）----------

// EncryptParams 把对象加密成前端 Encrypt() 的等价 Base64 密文。
//
// 输入必须是"能被 json 序列化成对端期望形状"的值；需要字节级对齐时请传 struct，
// 因为 Go 的 map 会按 key 排序序列化，而前端是按源码里的书写顺序拼的（对 JSON 语义
// 无影响，但会破坏"与 Python 输出逐字节相同"的回归断言）。
func EncryptParams(v any) (string, error) {
	plain, err := PyMarshal(v)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(paramsKey)
	if err != nil {
		return "", err
	}
	padded := PKCS7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, paramsKey).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// EncryptParamsRaw 加密一段**已经序列化好的** JSON 文本（调用方自己保证格式）。
func EncryptParamsRaw(plain []byte) (string, error) {
	block, err := aes.NewCipher(paramsKey)
	if err != nil {
		return "", err
	}
	padded := PKCS7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, paramsKey).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// DecryptParams 反向操作，主要给测试与排查用。
func DecryptParams(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(raw)%aes.BlockSize != 0 || len(raw) == 0 {
		return "", ErrBadCiphertext
	}
	block, err := aes.NewCipher(paramsKey)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, paramsKey).CryptBlocks(out, raw)
	plain, err := PKCS7Unpad(out, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ---------- AES-ECB（SSO 密码）----------

// EncryptECBBlock 实现 AES-128-ECB + PKCS7。
//
// Go 1.22 的 crypto/cipher 没有 ECB 模式（ECB 本身不被推荐），但学校 SSO 就是这么
// 加密密码的，只能按 16 字节分块自己循环。key 必须是 16/24/32 字节。
func EncryptECBBlock(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("AES 密钥非法（需 16/24/32 字节，croypto 解出来是 %d 字节）: %w", len(key), err)
	}
	padded := PKCS7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	size := block.BlockSize()
	for i := 0; i < len(padded); i += size {
		block.Encrypt(out[i:i+size], padded[i:i+size])
	}
	return out, nil
}

// ---------- 与 Python json.dumps 对齐的序列化 ----------

// PyMarshal 复刻 Python `json.dumps(obj, separators=(",", ":"))` 的字节形态。
//
// 两处差异必须抹平，否则密文不同、对端解出来的字符串不同：
//  1. Python 默认 ensure_ascii=True —— 中文/emoji 会写成 \uXXXX（Go 默认输出 UTF-8）；
//  2. Python 不转义 < > &，而 Go 的 json.Marshal 默认转义成 \u003c 之类。
//
// 只处理"字符串里的非 ASCII 字符"是安全的：JSON 结构字符与数字全是 ASCII，
// 非 ASCII 字节只可能出现在被引号包住的字符串内部。
func PyMarshal(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("JSON 序列化失败: %w", err)
	}
	raw := strings.TrimRight(sb.String(), "\n")
	return []byte(escapeNonASCII(raw)), nil
}

// escapeNonASCII 把非 ASCII 字符改写成 \uXXXX（BMP 外按代理对拆成两个）。
// 已经是 ASCII 的内容原样保留 —— 因此对同一段文本重复调用是幂等的。
func escapeNonASCII(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 16)
	for _, r := range s {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			sb.WriteString(fmt.Sprintf(`\u%04x\u%04x`, hi, lo))
		default:
			sb.WriteString(fmt.Sprintf(`\u%04x`, r))
		}
	}
	return sb.String()
}

// ---------- 密码哈希（PBKDF2-HMAC-SHA256）----------

const (
	// PBKDF2Iterations 选 12 万：单次校验在普通机器上约 60–120ms，
	// 足以让离线爆破变贵，又不会让登录明显变慢。
	PBKDF2Iterations = 120000
	saltLen          = 16
	hashLen          = 32
	hashPrefix       = "pbkdf2$sha256$"
)

// PBKDF2SHA256 是 RFC 8018 定义的 PBKDF2，伪随机函数固定 HMAC-SHA256。
// 标准库不提供，实现只有二十来行，比引入 x/crypto 更符合"零依赖"约束。
func PBKDF2SHA256(password, salt []byte, iter, keyLen int) []byte {
	if iter < 1 {
		iter = 1
	}
	prf := hmac.New(sha256.New, password)
	hashSize := prf.Size()
	numBlocks := (keyLen + hashSize - 1) / hashSize
	out := make([]byte, 0, numBlocks*hashSize)
	u := make([]byte, hashSize)
	t := make([]byte, hashSize)

	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		prf.Write(idx[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// HashPassword 生成可落盘的密码串：pbkdf2$sha256$<iter>$<b64 salt>$<b64 hash>。
// 迭代次数与盐都写进字符串，将来调高参数不会让旧密码失效。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	derived := PBKDF2SHA256([]byte(pw), salt, PBKDF2Iterations, hashLen)
	return fmt.Sprintf("%s%d$%s$%s", hashPrefix, PBKDF2Iterations,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(derived)), nil
}

// VerifyPassword 用常量时间比较，避免按字节比较泄漏前缀信息。
func VerifyPassword(encoded, pw string) bool {
	iter, salt, want, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got := PBKDF2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func parseHash(encoded string) (iter int, salt, hash []byte, err error) {
	if !strings.HasPrefix(encoded, hashPrefix) {
		return 0, nil, nil, errors.New("密码哈希格式不识别")
	}
	parts := strings.Split(strings.TrimPrefix(encoded, hashPrefix), "$")
	if len(parts) != 3 {
		return 0, nil, nil, errors.New("密码哈希段数不对")
	}
	iter, err = strconv.Atoi(parts[0])
	if err != nil || iter < 1 {
		return 0, nil, nil, errors.New("密码哈希迭代次数非法")
	}
	salt, err = base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, nil, nil, errors.New("密码哈希盐非法")
	}
	hash, err = base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, nil, nil, errors.New("密码哈希非法")
	}
	return iter, salt, hash, nil
}

// ---------- 随机与签名 ----------

// RandomHex 返回 n 字节密码学随机数的十六进制串（长度 2n），用于 Session 密钥。
func RandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败意味着系统熵源不可用，继续跑只会生成可预测的密钥。
		panic("系统随机源不可用: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// Signer 对 Session 载荷做 HMAC-SHA256 签名（无状态会话，进程重启不失效）。
type Signer struct{ Key []byte }

// Sign 返回 base64url 编码的签名（无填充，方便直接放进 Cookie）。
func (s Signer) Sign(msg []byte) string {
	mac := hmac.New(sha256.New, s.Key)
	mac.Write(msg)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 常量时间比较签名。
func (s Signer) Verify(msg []byte, sig string) bool {
	want := s.Sign(msg)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

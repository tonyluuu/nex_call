package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// shareKeyMaterial 是分享链接的固定密钥。只放在服务端，网页拿不到。
const shareKeyMaterial = "nexcall-share-link-v1"

// MaxShareDays 是单次分享允许的最长天数。0 表示不限制，不计入这个上限。
const MaxShareDays = 3650

var (
	errBadShare         = errors.New("invalid share")
	ErrShareExpired     = errors.New("share expired")
	ErrInvalidShareDays = errors.New("invalid share days")
)

type sharePayload struct {
	U string `json:"u"`
	G *int64 `json:"g"`
	E *int64 `json:"e"`
}

func shareAEAD() (cipher.AEAD, error) {
	sum := sha256.Sum256([]byte(shareKeyMaterial))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func ValidShareDays(days int) bool {
	return days >= 0 && days <= MaxShareDays
}

// SealShare 用固定密钥加密用户名、签发序号和过期时间。days 为 0 表示不限制。
// 每次使用新的随机 IV，所以同一账号的密文不同。
func SealShare(username string, gen int64, days int) (string, error) {
	if !ValidShareDays(days) {
		return "", ErrInvalidShareDays
	}
	var exp int64
	if days > 0 {
		exp = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	}
	return SealShareUntil(username, gen, exp)
}

// SealShareUntil 使用绝对过期时间。expiry 为 0 表示不限制。
func SealShareUntil(username string, gen int64, expiry int64) (string, error) {
	if username == "" || gen < 1 || expiry < 0 {
		return "", errBadShare
	}
	exp := expiry
	plain, err := json.Marshal(sharePayload{U: username, G: &gen, E: &exp})
	if err != nil {
		return "", err
	}
	return sealPlain(plain)
}

func sealPlain(plain []byte) (string, error) {
	aead, err := shareAEAD()
	if err != nil {
		return "", err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, iv, plain, nil)
	return base64.RawURLEncoding.EncodeToString(append(iv, sealed...)), nil
}

// OpenShare 解密分享链接。用户名、签发序号、过期时间缺一不可。已过期时返回 ErrShareExpired。
func OpenShare(token string) (username string, gen int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", 0, errBadShare
	}
	aead, err := shareAEAD()
	if err != nil {
		return "", 0, err
	}
	ns := aead.NonceSize()
	if len(raw) < ns+aead.Overhead() {
		return "", 0, errBadShare
	}
	plain, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", 0, errBadShare
	}
	var payload sharePayload
	if err := json.Unmarshal(plain, &payload); err != nil || payload.U == "" || payload.G == nil || *payload.G < 1 || payload.E == nil {
		return "", 0, errBadShare
	}
	if *payload.E != 0 && time.Now().Unix() >= *payload.E {
		return "", 0, ErrShareExpired
	}
	return payload.U, *payload.G, nil
}

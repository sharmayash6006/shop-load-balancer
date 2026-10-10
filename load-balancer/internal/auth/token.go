// Package auth implements a minimal HS256 JWT. The same file is duplicated in the
// load balancer so it can identify users without calling the backend.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Claims struct {
	Sub  int64  `json:"sub"`
	Name string `json:"name"`
	SID  string `json:"sid"`
	Exp  int64  `json:"exp"`
}

var enc = base64.RawURLEncoding

const header = `{"alg":"HS256","typ":"JWT"}`

func sign(data string, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(data))
	return enc.EncodeToString(m.Sum(nil))
}

func Sign(c Claims, secret []byte) string {
	body, _ := json.Marshal(c)
	data := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(body)
	return data + "." + sign(data, secret)
}

var ErrInvalid = errors.New("invalid token")

func Verify(token string, secret []byte) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, ErrInvalid
	}
	want := sign(parts[0]+"."+parts[1], secret)
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return c, ErrInvalid
	}
	raw, err := enc.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, ErrInvalid
	}
	if c.Exp < time.Now().Unix() {
		return c, errors.New("token expired")
	}
	return c, nil
}

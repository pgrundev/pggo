package postgres

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// scram implements the client side of SCRAM-SHA-256 (RFC 7677) without
// channel binding, as used by PostgreSQL.
type scram struct {
	password    string
	nonce       string
	clientBare  string
	authMessage string
	saltedPass  []byte
}

func newScram(password string) *scram {
	b := make([]byte, 18)
	rand.Read(b)
	return &scram{password: password, nonce: base64.RawStdEncoding.EncodeToString(b)}
}

func (s *scram) clientFirst() []byte {
	s.clientBare = "n=,r=" + s.nonce
	return []byte("n,," + s.clientBare)
}

func (s *scram) clientFinal(serverFirst []byte) ([]byte, error) {
	var nonce, salt string
	iters := 0
	for _, attr := range strings.Split(string(serverFirst), ",") {
		if len(attr) < 2 || attr[1] != '=' {
			continue
		}
		switch attr[0] {
		case 'r':
			nonce = attr[2:]
		case 's':
			salt = attr[2:]
		case 'i':
			iters, _ = strconv.Atoi(attr[2:])
		}
	}
	if !strings.HasPrefix(nonce, s.nonce) || len(nonce) == len(s.nonce) {
		return nil, fmt.Errorf("invalid server nonce")
	}
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || iters < 1 {
		return nil, fmt.Errorf("invalid server-first-message")
	}
	s.saltedPass = pbkdf2SHA256([]byte(s.password), saltBytes, iters)
	withoutProof := "c=biws,r=" + nonce
	s.authMessage = s.clientBare + "," + string(serverFirst) + "," + withoutProof
	clientKey := hmacSHA256(s.saltedPass, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	sig := hmacSHA256(storedKey[:], s.authMessage)
	for i := range clientKey {
		clientKey[i] ^= sig[i]
	}
	return []byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientKey)), nil
}

func (s *scram) verifyServer(serverFinal []byte) bool {
	v, ok := strings.CutPrefix(string(serverFinal), "v=")
	if !ok || s.saltedPass == nil {
		return false
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	got, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return false
	}
	want := hmacSHA256(hmacSHA256(s.saltedPass, "Server Key"), s.authMessage)
	return hmac.Equal(got, want)
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// pbkdf2SHA256 derives a single 32-byte block, which is all SCRAM-SHA-256 needs.
func pbkdf2SHA256(password, salt []byte, iters int) []byte {
	h := hmac.New(sha256.New, password)
	h.Write(salt)
	h.Write(binary.BigEndian.AppendUint32(nil, 1))
	u := h.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < iters; i++ {
		h.Reset()
		h.Write(u)
		u = h.Sum(u[:0])
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

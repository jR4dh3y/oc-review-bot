package gh

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// parseRSAKey reads a PKCS#1 or PKCS#8 private key from PEM text or a file
// path (detected by a leading "-----BEGIN" marker).
func parseRSAKey(pemTextOrPath string) (*rsa.PrivateKey, error) {
	text := pemTextOrPath
	if !strings.HasPrefix(strings.TrimSpace(pemTextOrPath), "-----BEGIN") {
		b, err := os.ReadFile(pemTextOrPath)
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

// SignAppJWT builds a short-lived RS256 JWT identifying the App, which GitHub
// exchanges for installation tokens.
func (a *App) SignAppJWT() (string, error) {
	now := time.Now().Add(-30 * time.Second)
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": a.appID,
		"iat": now.Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc(sig), nil
}

func decodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd hex length")
	}
	return hex.DecodeString(s)
}

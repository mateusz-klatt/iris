// Package config holds local hub configuration and credential file handling.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Listen, Database, TokenFile, TLSCert, TLSKey string
	AllowedHosts                                 []string
}

func Default() (Config, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, err
	}
	dir = filepath.Join(dir, "iris")
	return Config{Listen: "127.0.0.1:7878", Database: filepath.Join(dir, "hub.db"), TokenFile: filepath.Join(dir, "hub.token")}, nil
}
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("both TLS certificate and key are required")
	}
	if host != "127.0.0.1" && host != "::1" && c.TLSCert == "" {
		return errors.New("a non-loopback listen address requires TLS")
	}
	return nil
}
func Token(filename string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err = secureToken(filename); err != nil {
			f.Close()
			os.Remove(filename)
			return "", err
		}
		var b [32]byte
		if _, err = rand.Read(b[:]); err != nil {
			f.Close()
			os.Remove(filename)
			return "", err
		}
		token := base64.RawURLEncoding.EncodeToString(b[:])
		_, err = f.WriteString(token + "\n")
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(filename)
			return "", err
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("token must be a regular file")
	}
	if err = secureToken(filename); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) < 32 {
		return "", fmt.Errorf("invalid token file")
	}
	return token, nil
}

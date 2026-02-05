package store

import (
	"crypto/md5"
	"encoding/hex"

	"golang.org/x/crypto/bcrypt"
)

const digestRealm = "CUPS-Golang"

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func checkPassword(hash string, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func digestHA1(username, password string) string {
	sum := md5.Sum([]byte(username + ":" + digestRealm + ":" + password))
	return hex.EncodeToString(sum[:])
}

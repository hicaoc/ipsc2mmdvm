package web

import (
	"errors"
	"net/mail"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func validateRegistration(username, callsign, email, password string) error {
	if err := validateUserProfile(username, callsign, email); err != nil {
		return err
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

func validateUserProfile(username, callsign, email string) error {
	if len(strings.TrimSpace(username)) < 3 {
		return errors.New("username must be at least 3 characters")
	}
	if strings.TrimSpace(callsign) == "" {
		return errors.New("callsign is required")
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(email)); err != nil {
		return errors.New("invalid email")
	}
	return nil
}

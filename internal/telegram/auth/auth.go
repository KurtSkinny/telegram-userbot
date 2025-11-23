// Package core предоставляет интерактивный слой авторизации для userbot на базе gotd.
// Файл auth.go описывает терминальный аутентификатор (auth.UserAuthenticator):
// чтение номера телефона/кода/2FA из консоли, согласие с ToS и первичную регистрацию (SignUp).
// Этот слой связывает CLI и gotd, не меняя сетевую логику клиента.

package auth

import (
	"context"
	"strings"
	"syscall"
	"telegram-userbot/internal/pr"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// readLine выводит приглашение, читает строку из общего readline и обрезает пробелы по краям.
// Возвращает введённое значение или ошибку чтения (включая EOF при закрытом stdin).
func readLine(prompt string) (string, error) {
	// Устанавливаем приглашение в общий readline; будет действовать до следующей смены.
	pr.SetPrompt(prompt)
	line, err := pr.Rl().Readline()
	return strings.TrimSpace(line), err
}

// TerminalAuthenticator реализует auth.UserAuthenticator и собирает ввод из терминала.
// Предназначен для интерактивного входа пользователя: номер телефона, код подтверждения,
// пароль 2FA, принятие ToS и первичная регистрация. Не валидирует формат номера.
type TerminalAuthenticator struct {
	// PhoneNumber хранит телефон, с которым будет выполняться вход.
	PhoneNumber string
}

// Phone возвращает заранее известный номер телефона. Формат не проверяется; ожидается E.164.
func (t TerminalAuthenticator) Phone(_ context.Context) (string, error) {
	return t.PhoneNumber, nil
}

// Code запрашивает код подтверждения у пользователя и возвращает его без пробелов по краям.
// sentCode содержит метаданные от Telegram и здесь не используется.
func (t TerminalAuthenticator) Code(_ context.Context, sentCode *tg.AuthSentCode) (string, error) {
	return readLine("Enter the code from Telegram: ")
}

// Password считывает пароль двухфакторной аутентификации без отображения вводимых символов.
// Используется term.ReadPassword; на некоторых ОС может потребоваться явное приведение fd к int.
func (t TerminalAuthenticator) Password(_ context.Context) (string, error) {
	// Сообщение без перевода строки, чтобы ввод шёл в той же строке.
	pr.Print("Enter 2FA password: ")
	// Безэховый ввод пароля из stdin; драйвер сам вернёт управление после Enter.
	passwordBytes, err := term.ReadPassword(syscall.Stdin)
	// Возвращаем курсор на новую строку после скрытого ввода.
	pr.Println()
	if err != nil {
		return "", err
	}
	return string(passwordBytes), nil
}

// AcceptTermsOfService выводит текст условий использования и запрашивает согласие пользователя.
// Принимаются только ответы "y"/"Y"; любой другой ответ трактуется как отказ.
func (t TerminalAuthenticator) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	// Текст ToS может быть длинным и содержать разметку; выводим как есть.
	pr.Printf("Telegram Terms of Service: %s\n", tos.Text)
	resp, err := readLine("Do you accept? (y/n): ")
	if err != nil {
		return err
	}
	if resp != "y" && resp != "Y" {
		return errors.New("user did not accept terms of service")
	}
	return nil
}

// SignUp вызывается для незарегистрированного номера: собирает имя и (опциональную) фамилию.
// Возвращает auth.UserInfo для отправки в Telegram.
func (t TerminalAuthenticator) SignUp(_ context.Context) (auth.UserInfo, error) {
	firstName, err := readLine("Enter your first name: ")
	if err != nil {
		return auth.UserInfo{}, err
	}
	// Фамилия опциональна; ошибку чтения игнорируем, чтобы не блокировать регистрацию.
	lastName, _ := readLine("Enter your last name (optional): ")
	return auth.UserInfo{
		FirstName: firstName,
		LastName:  lastName,
	}, nil
}

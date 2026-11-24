package app

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func NewRecoverySender(address, from, user, password string) (func(context.Context, string, string, string) error, error) {
	if address == "" && from == "" && user == "" && password == "" {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(address)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	sender, mailErr := mail.ParseAddress(from)
	if err != nil || portErr != nil || portNumber == 0 || host == "" || mailErr != nil ||
		sender.Address != from || (user == "") != (password == "") {
		return nil, errors.New("invalid account recovery SMTP settings")
	}
	return func(ctx context.Context, recipient, subject, body string) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		defer conn.Close()
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
		if user != "" {
			if err := client.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
				return err
			}
		}
		if err := client.Mail(from); err != nil {
			return err
		}
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
		data, err := client.Data()
		if err != nil {
			return err
		}
		_, err = io.WriteString(data, strings.Join([]string{
			"From: " + from, "To: " + recipient, "Subject: " + subject,
			"Content-Type: text/plain; charset=utf-8", "", body,
		}, "\r\n"))
		if err != nil {
			return err
		}
		return data.Close()
	}, nil
}

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type smtpNotifier struct{}

func (smtpNotifier) validate(n *NotifierConfig) error {
	if n.Host == "" || n.From == "" || len(n.To) == 0 {
		return errors.New("host, from and to are required")
	}
	ports := map[string]int{"": 587, "starttls": 587, "tls": 465, "none": 25}
	def, ok := ports[n.TLS]
	if !ok {
		return fmt.Errorf("tls must be starttls, tls or none, got %q", n.TLS)
	}
	if n.TLS == "" {
		n.TLS = "starttls"
	}
	if n.Port == 0 {
		n.Port = def
	}
	if n.SubjectPrefix == "" {
		n.SubjectPrefix = "[up]"
	}
	return nil
}

func (smtpNotifier) send(a *App, n *NotifierConfig, msg message) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := a.dial(ctx, "tcp", net.JoinHostPort(n.Host, strconv.Itoa(n.Port)))
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if n.TLS == "tls" {
		conn = tls.Client(conn, &tls.Config{ServerName: n.Host})
	}
	c, err := smtp.NewClient(conn, n.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if n.TLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: n.Host}); err != nil {
			return err
		}
	}
	if n.User != "" {
		if err := c.Auth(smtp.PlainAuth("", n.User, n.Password, n.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(n.From); err != nil {
		return err
	}
	for _, to := range n.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		n.From, strings.Join(n.To, ", "), n.SubjectPrefix+" "+msg.subject(), time.Now().Format(time.RFC1123Z), strings.ReplaceAll(msg.text(), "\n", "\r\n"))
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

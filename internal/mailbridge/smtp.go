package mailbridge

import (
	"context"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/peterretief/yggstore/internal/mail"
)

// ServeSMTP takes messages to send on addr until ctx ends. Mail programs
// keep their own copy in Sent (over IMAP), so none is kept here.
func (b *Bridge) ServeSMTP(ctx context.Context, addr string) error {
	if err := Loopback(addr); err != nil {
		return err
	}
	s := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &session{b: b}, nil
	}))
	s.Addr = addr
	s.Domain = "localhost"
	s.AllowInsecureAuth = true // on this machine only (see Loopback)
	s.MaxMessageBytes = mail.MaxSize
	s.MaxRecipients = mail.MaxRecipients
	s.ReadTimeout = 5 * time.Minute
	s.WriteTimeout = 5 * time.Minute
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	err = s.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type session struct {
	b      *Bridge
	authed bool
	rcpts  []string
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if err := s.b.login(username, password); err != nil {
			s.b.logf("mail bridge: SMTP login refused for %q: wrong password (yggstore mail bridge prints it)", username)
			return smtp.ErrAuthFailed
		}
		s.authed = true
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := s.b.Send(ctx, raw, s.rcpts); err != nil {
		s.b.logf("mail bridge: not sent: %v", err)
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 0, 0}, Message: errorLine(err)}
	}
	s.b.logf("mail bridge: sent to %d recipient(s)", len(s.rcpts))
	return nil
}

// errorLine fits an error on one SMTP reply line.
func errorLine(err error) string {
	msg := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg
}

func (s *session) Reset()        { s.rcpts = nil }
func (s *session) Logout() error { return nil }

var _ smtp.AuthSession = (*session)(nil)

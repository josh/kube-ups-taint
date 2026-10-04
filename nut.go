package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const defaultNUTPort = "3493"

const nutTimeout = 5 * time.Second

var errVarNotSupported = errors.New("variable not supported")

type upsAddress struct {
	ups  string
	host string
}

func (a upsAddress) String() string {
	return a.ups + "@" + a.host
}

func parseUPSAddress(s string) (upsAddress, error) {
	name, host, ok := strings.Cut(s, "@")
	if !ok || name == "" || host == "" {
		return upsAddress{}, fmt.Errorf("expected upsname@host[:port], got %q", s)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), defaultNUTPort)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		return upsAddress{}, fmt.Errorf("invalid host %q: %w", host, err)
	}
	return upsAddress{ups: name, host: host}, nil
}

type nutClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialNUT(ctx context.Context, host string) (*nutClient, error) {
	d := net.Dialer{Timeout: nutTimeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial nut server: %w", err)
	}
	return &nutClient{conn: conn, r: bufio.NewReader(conn)}, nil
}

func (c *nutClient) command(line string) ([]string, error) {
	if err := c.conn.SetDeadline(time.Now().Add(nutTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := fmt.Fprintf(c.conn, "%s\n", line); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}
	reply, err := c.r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	fields, err := splitNUTLine(strings.TrimRight(reply, "\r\n"))
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty reply")
	}
	if fields[0] == "ERR" {
		if len(fields) > 1 && fields[1] == "VAR-NOT-SUPPORTED" {
			return nil, errVarNotSupported
		}
		if len(fields) == 1 {
			return nil, fmt.Errorf("nut error (no detail)")
		}
		return nil, fmt.Errorf("nut error: %s", strings.Join(fields[1:], " "))
	}
	return fields, nil
}

func (c *nutClient) login(username, password string) error {
	if _, err := c.command("USERNAME " + quoteNUT(username)); err != nil {
		return fmt.Errorf("username: %w", err)
	}
	if _, err := c.command("PASSWORD " + quoteNUT(password)); err != nil {
		return fmt.Errorf("password: %w", err)
	}
	return nil
}

func (c *nutClient) getVar(ups, name string) (string, error) {
	fields, err := c.command("GET VAR " + quoteNUT(ups) + " " + quoteNUT(name))
	if err != nil {
		return "", err
	}
	if len(fields) != 4 || fields[0] != "VAR" || fields[1] != ups || fields[2] != name {
		return "", fmt.Errorf("unexpected reply: %q", strings.Join(fields, " "))
	}
	return fields[3], nil
}

func (c *nutClient) close() {
	_, _ = c.command("LOGOUT")
	_ = c.conn.Close()
}

func quoteNUT(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func splitNUTLine(line string) ([]string, error) {
	var fields []string
	var b strings.Builder
	inQuote, escaped, inField := false, false, false
	for _, ch := range line {
		switch {
		case escaped:
			b.WriteRune(ch)
			escaped = false
		case ch == '\\':
			escaped = true
			inField = true
		case ch == '"':
			inQuote = !inQuote
			inField = true
		case ch == ' ' && !inQuote:
			if inField {
				fields = append(fields, b.String())
				b.Reset()
				inField = false
			}
		default:
			b.WriteRune(ch)
			inField = true
		}
	}
	if inQuote || escaped {
		return nil, fmt.Errorf("malformed reply: %q", line)
	}
	if inField {
		fields = append(fields, b.String())
	}
	return fields, nil
}

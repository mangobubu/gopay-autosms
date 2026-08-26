// Package proxy parses and tracks the proxy slots entered in the workbench.
// Each input line is a slot: duplicate addresses are intentionally retained.
package proxy

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("invalid proxy address")
var ErrExhausted = errors.New("proxy pool exhausted")

type Entry struct {
	ID     int    `json:"id"`
	Raw    string `json:"raw"`
	URL    string `json:"url"`
	Status string `json:"status"` // available or used
	Error  string `json:"error,omitempty"`
}

func Mask(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return "proxy"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

func ParseLines(raw string) ([]Entry, error) {
	lines := strings.FieldsFunc(strings.ReplaceAll(raw, "\r", "\n"), func(r rune) bool {
		return r == '\n' || r == ',' || r == ';'
	})
	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(strings.ReplaceAll(line, `\@`, "@"))
		if line == "" {
			continue
		}
		normalized, err := Normalize(line)
		if err != nil {
			return nil, fmt.Errorf("proxy line %q: %w", line, err)
		}
		entries = append(entries, Entry{ID: len(entries) + 1, Raw: line, URL: normalized, Status: "available"})
	}
	return entries, nil
}

func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, `\@`, "@"))
	if raw == "" {
		return "", ErrInvalid
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Hostname() == "" || u.Port() == "" {
			return "", ErrInvalid
		}
		return u.String(), nil
	}

	// hostname:port:username:password (password may contain colons)
	parts := strings.SplitN(raw, ":", 4)
	if len(parts) == 4 && !strings.Contains(parts[1], "@") {
		return build(parts[0], parts[1], parts[2], parts[3], "http")
	}
	if strings.Count(raw, "@") != 1 {
		return "", ErrInvalid
	}
	left, right, _ := strings.Cut(raw, "@")
	// username:password@hostname:port
	if strings.Count(left, ":") >= 1 {
		credentials := strings.SplitN(left, ":", 2)
		hostPort := strings.Split(right, ":")
		if len(hostPort) == 2 && isPort(hostPort[1]) {
			return build(hostPort[0], hostPort[1], credentials[0], credentials[1], "http")
		}
	}
	// hostname:port@username:password
	hostPort := strings.Split(left, ":")
	credentials := strings.SplitN(right, ":", 2)
	if len(hostPort) == 2 && len(credentials) == 2 {
		return build(hostPort[0], hostPort[1], credentials[0], credentials[1], "http")
	}
	return "", ErrInvalid
}

func isPort(value string) bool {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && port >= 1 && port <= 65535
}

func build(host, port, username, password, scheme string) (string, error) {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	username = strings.TrimSpace(username)
	if host == "" || username == "" || password == "" {
		return "", ErrInvalid
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", ErrInvalid
	}
	u := &url.URL{Scheme: scheme, Host: fmt.Sprintf("%s:%d", host, portNumber)}
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}

func Counts(entries []Entry) (available, total int) {
	for _, entry := range entries {
		total++
		if entry.Status == "available" || entry.Status == "" {
			available++
		}
	}
	return available, total
}

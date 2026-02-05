package cupsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"
)

type Client struct {
	Host     string
	Port     int
	UseTLS   bool
	User     string
	Password string
}

func NewFromEnv() *Client {
	host := os.Getenv("CUPS_SERVER")
	if host == "" {
		host = "localhost"
	}
	useTLS := false
	port := 631
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			if u.Host != "" {
				host = u.Hostname()
				if p := u.Port(); p != "" {
					if n, err := strconv.Atoi(p); err == nil {
						port = n
					}
				}
			}
			if strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "ipps") {
				useTLS = true
			}
		}
	} else if strings.Contains(host, ":") {
		parts := strings.Split(host, ":")
		host = parts[0]
		if n, err := strconv.Atoi(parts[1]); err == nil {
			port = n
		}
	}
	if enc := strings.ToLower(strings.TrimSpace(os.Getenv("CUPS_ENCRYPTION"))); enc != "" {
		if enc == "required" || enc == "always" || enc == "on" || enc == "true" {
			useTLS = true
		}
		if enc == "never" || enc == "off" || enc == "false" {
			useTLS = false
		}
	}
	user := os.Getenv("CUPS_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	return &Client{
		Host:     host,
		Port:     port,
		UseTLS:   useTLS,
		User:     user,
		Password: os.Getenv("CUPS_PASSWORD"),
	}
}

func (c *Client) PrinterURI(name string) string {
	scheme := "ipp"
	if c.UseTLS {
		scheme = "ipps"
	}
	return scheme + "://" + c.Host + ":" + strconv.Itoa(c.Port) + "/printers/" + name
}

func (c *Client) IppURL() string {
	scheme := "http"
	if c.UseTLS {
		scheme = "https"
	}
	return scheme + "://" + c.Host + ":" + strconv.Itoa(c.Port) + "/ipp/print"
}

func (c *Client) Send(ctx context.Context, msg *goipp.Message, data io.Reader) (*goipp.Message, error) {
	if msg == nil {
		return nil, errors.New("missing ipp message")
	}
	payload, err := msg.EncodeBytes()
	if err != nil {
		return nil, err
	}
	body := io.Reader(bytes.NewBuffer(payload))
	if data != nil {
		body = io.MultiReader(bytes.NewBuffer(payload), data)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IppURL(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	req.Header.Set("Accept", goipp.ContentType)
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig(),
		},
	}
	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, errors.New(resp.Status)
	}
	out := &goipp.Message{}
	if err := out.Decode(resp.Body); err != nil {
		return nil, err
	}
	return out, nil
}

func tlsConfig() *tls.Config {
	insecure := strings.ToLower(os.Getenv("CUPS_IPP_INSECURE"))
	skipVerify := insecure == "1" || insecure == "true" || insecure == "yes" || insecure == "on"
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skipVerify}
}

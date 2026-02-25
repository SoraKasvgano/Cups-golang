package cupsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	goipp "github.com/OpenPrinting/goipp"
)

type Client struct {
	Host               string
	Port               int
	UseTLS             bool
	User               string
	Password           string
	InsecureSkipVerify bool
}

type ClientOption func(*Client)

func WithServer(server string) ClientOption {
	return func(c *Client) {
		if strings.TrimSpace(server) == "" {
			return
		}
		host, port, useTLS := parseServer(server)
		if host != "" {
			c.Host = host
		}
		if port > 0 {
			c.Port = port
		}
		if useTLS {
			c.UseTLS = true
		}
	}
}

func WithTLS(enable bool) ClientOption {
	return func(c *Client) {
		if enable {
			c.UseTLS = true
		}
	}
}

func WithUser(user string) ClientOption {
	return func(c *Client) {
		if strings.TrimSpace(user) != "" {
			c.User = user
		}
	}
}

func WithPassword(password string) ClientOption {
	return func(c *Client) {
		if password != "" {
			c.Password = password
		}
	}
}

func NewFromConfig(opts ...ClientOption) *Client {
	settings := loadClientSettings()
	client := &Client{
		Host:               settings.host,
		Port:               settings.port,
		UseTLS:             settings.useTLS,
		User:               settings.user,
		Password:           settings.password,
		InsecureSkipVerify: settings.insecureSkipVerify,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	if client.Host == "" {
		client.Host = "localhost"
	}
	if client.Port == 0 {
		client.Port = defaultIPPPort()
	}
	return client
}

func NewFromEnv() *Client {
	return NewFromConfig()
}

func (c *Client) PrinterURI(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "ipp://localhost/printers/"
	}
	return "ipp://localhost/printers/" + url.PathEscape(name)
}

func (c *Client) IppURL() string {
	return c.ippURLForPath("/ipp/print")
}

func (c *Client) ippURLForPath(path string) string {
	scheme := "http"
	if c.UseTLS {
		scheme = "https"
	}
	if path == "" {
		path = "/ipp/print"
	}
	return scheme + "://" + c.Host + ":" + strconv.Itoa(c.Port) + path
}

func ippPathForOp(op goipp.Op) string {
	switch op {
	case goipp.OpCancelJobs,
		goipp.OpPurgeJobs,
		goipp.OpCupsAddModifyPrinter,
		goipp.OpCupsDeletePrinter,
		goipp.OpCupsAddModifyClass,
		goipp.OpCupsDeleteClass,
		goipp.OpCupsSetDefault,
		goipp.OpCupsAcceptJobs,
		goipp.OpCupsRejectJobs,
		goipp.OpPausePrinter,
		goipp.OpPausePrinterAfterCurrentJob,
		goipp.OpResumePrinter,
		goipp.OpEnablePrinter,
		goipp.OpDisablePrinter,
		goipp.OpHoldNewJobs,
		goipp.OpReleaseHeldNewJobs,
		goipp.OpRestartPrinter,
		goipp.OpPauseAllPrinters,
		goipp.OpPauseAllPrintersAfterCurrentJob,
		goipp.OpResumeAllPrinters,
		goipp.OpRestartSystem:
		return "/admin/"

	case goipp.OpCancelJob,
		goipp.OpCancelMyJobs,
		goipp.OpGetJobs,
		goipp.OpGetJobAttributes,
		goipp.OpSetJobAttributes,
		goipp.OpHoldJob,
		goipp.OpReleaseJob,
		goipp.OpRestartJob,
		goipp.OpResumeJob,
		goipp.OpCloseJob,
		goipp.OpCreateJobSubscriptions,
		goipp.OpGetNotifications,
		goipp.OpGetDocuments,
		goipp.OpGetDocumentAttributes,
		goipp.OpCupsAuthenticateJob,
		goipp.OpCupsMoveJob,
		goipp.OpCupsGetDocument:
		return "/jobs/"
	case goipp.OpPrintJob,
		goipp.OpCreateJob,
		goipp.OpSendDocument,
		goipp.OpValidateJob,
		goipp.OpValidateDocument:
		return "/ipp/print"
	default:
		return "/"
	}
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ippURLForPath(ippPathForMessage(msg)), body)
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
			TLSClientConfig: tlsConfig(c),
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

func (c *Client) SendWithPayload(ctx context.Context, msg *goipp.Message, data io.Reader) (*goipp.Message, []byte, error) {
	if msg == nil {
		return nil, nil, errors.New("missing ipp message")
	}
	payload, err := msg.EncodeBytes()
	if err != nil {
		return nil, nil, err
	}
	body := io.Reader(bytes.NewBuffer(payload))
	if data != nil {
		body = io.MultiReader(bytes.NewBuffer(payload), data)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ippURLForPath(ippPathForMessage(msg)), body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	req.Header.Set("Accept", goipp.ContentType)
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig(c),
		},
	}
	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, nil, errors.New(resp.Status)
	}
	out := &goipp.Message{}
	if err := out.Decode(resp.Body); err != nil {
		return nil, nil, err
	}
	rest, _ := io.ReadAll(resp.Body)
	return out, rest, nil
}

func tlsConfig(c *Client) *tls.Config {
	skipVerify := false
	if c != nil {
		skipVerify = c.InsecureSkipVerify
	}
	if insecure, ok := parseBoolEnv("CUPS_IPP_INSECURE"); ok {
		skipVerify = insecure
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skipVerify}
}

func ippPathForMessage(msg *goipp.Message) string {
	if msg == nil {
		return "/"
	}
	op := goipp.Op(msg.Code)
	defaultPath := ippPathForOp(op)
	if defaultPath == "/admin/" || defaultPath == "/jobs/" {
		return defaultPath
	}
	if ippPathPinnedToRoot(op) {
		return "/"
	}
	if p, ok := ippResourcePathFromURI(attrString(msg.Operation, "printer-uri")); ok {
		return p
	}
	if p, ok := ippResourcePathFromURI(attrString(msg.Operation, "job-uri")); ok {
		return p
	}
	return defaultPath
}

func ippPathPinnedToRoot(op goipp.Op) bool {
	switch op {
	case goipp.OpCupsGetDevices,
		goipp.OpCupsGetPpd,
		goipp.OpCupsGetPpds,
		goipp.OpCupsGetPrinters,
		goipp.OpCupsGetClasses,
		goipp.OpCupsGetDefault:
		return true
	default:
		return false
	}
}

func ippResourcePathFromURI(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	path := strings.TrimSpace(u.Path)
	if path == "" {
		return "", false
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path, true
}

func attrString(attrs goipp.Attributes, name string) string {
	for _, attr := range attrs {
		if !strings.EqualFold(strings.TrimSpace(attr.Name), strings.TrimSpace(name)) {
			continue
		}
		if len(attr.Values) == 0 {
			return ""
		}
		return strings.TrimSpace(attr.Values[0].V.String())
	}
	return ""
}

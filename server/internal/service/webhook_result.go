package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	WebhookConnectorType       = "webhook"
	WebhookResultCapability    = "result_delivery"
	WebhookEventHeader         = "X-Multica-Event"
	WebhookDeliveryIDHeader    = "X-Multica-Delivery-ID"
	WebhookIdempotencyHeader   = "Idempotency-Key"
	WebhookTimestampHeader     = "X-Multica-Timestamp"
	WebhookSignatureHeader     = "X-Multica-Signature"
	WebhookResultEvent         = "workbench.result.v1"
	maxWebhookResponseBodySize = 64 << 10
)

var (
	ErrWebhookTerminal          = errors.New("webhook delivery terminal failure")
	ErrUnsafeWebhookDestination = errors.New("unsafe webhook destination")
)

type WebhookResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type netResolver struct{ resolver *net.Resolver }

func (r netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return r.resolver.LookupNetIP(ctx, network, host)
}

type WebhookDialer func(context.Context, string, string) (net.Conn, error)

type WebhookTransportOptions struct {
	Resolver  WebhookResolver
	Dialer    WebhookDialer
	TLSConfig *tls.Config
	Now       func() time.Time
}

type WebhookResultDeliverer struct {
	queries *db.Queries
	box     *secretbox.Box
	options WebhookTransportOptions
}

func NewWebhookResultDeliverer(queries *db.Queries, box *secretbox.Box, options WebhookTransportOptions) (*WebhookResultDeliverer, error) {
	if queries == nil || box == nil {
		return nil, errors.New("webhook deliverer requires queries and secretbox")
	}
	if options.Resolver == nil {
		options.Resolver = netResolver{net.DefaultResolver}
	}
	if options.Dialer == nil {
		d := &net.Dialer{}
		options.Dialer = d.DialContext
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &WebhookResultDeliverer{queries: queries, box: box, options: options}, nil
}

func (d *WebhookResultDeliverer) DeliverResult(ctx context.Context, result ResultDelivery) (ResultDeliveryResponse, error) {
	row, err := d.queries.GetWorkbenchWebhookConnectorForDelivery(ctx, db.GetWorkbenchWebhookConnectorForDeliveryParams{ID: result.ConnectorID, WorkspaceID: result.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return terminalResponse(0, "connector not found")
	}
	if err != nil {
		return ResultDeliveryResponse{}, fmt.Errorf("load webhook connector: %w", err)
	}
	if row.ConnectorType != WebhookConnectorType || !row.Enabled || !hasResultCapability(row.Capabilities) || !row.ConfigVersion.Valid || row.ConfigVersion.Int32 != 1 || !row.EndpointUrl.Valid || !row.TimeoutMs.Valid || len(row.SigningSecretEncrypted) == 0 {
		return terminalResponse(0, "connector is not an enabled configured result webhook")
	}
	if _, err := ValidateWebhookConfig(WebhookConfig{Version: 1, URL: row.EndpointUrl.String, TimeoutMS: row.TimeoutMs.Int32}); err != nil {
		return terminalResponse(0, "invalid webhook configuration")
	}
	secret, err := d.box.Open(row.SigningSecretEncrypted)
	if err != nil {
		return terminalResponse(0, "webhook signing secret cannot be decrypted")
	}
	defer clear(secret)
	return d.send(ctx, row.EndpointUrl.String, time.Duration(row.TimeoutMs.Int32)*time.Millisecond, secret, result)
}

func terminalResponse(status int, message string) (ResultDeliveryResponse, error) {
	retry := false
	return ResultDeliveryResponse{StatusCode: status, Retryable: &retry}, fmt.Errorf("%w: %s", ErrWebhookTerminal, message)
}

func (d *WebhookResultDeliverer) send(ctx context.Context, rawURL string, timeout time.Duration, secret []byte, result ResultDelivery) (ResultDeliveryResponse, error) {
	u, _ := url.Parse(rawURL)
	ips, err := resolvePublicWebhookHost(ctx, d.options.Resolver, u.Hostname())
	if errors.Is(err, ErrUnsafeWebhookDestination) {
		return terminalResponse(0, "unsafe webhook destination")
	}
	if err != nil {
		return ResultDeliveryResponse{}, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
	if d.options.TLSConfig != nil {
		tlsConfig = d.options.TLSConfig.Clone()
		tlsConfig.ServerName = u.Hostname()
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := d.options.Dialer(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("webhook destination has no validated addresses")
		}
		return nil, lastErr
	}}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	timestamp := strconv.FormatInt(d.options.Now().UTC().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(result.Payload))
	if err != nil {
		return terminalResponse(0, "invalid webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(WebhookEventHeader, WebhookResultEvent)
	req.Header.Set(WebhookDeliveryIDHeader, result.IdempotencyKey)
	req.Header.Set(WebhookIdempotencyHeader, result.IdempotencyKey)
	req.Header.Set(WebhookTimestampHeader, timestamp)
	req.Header.Set(WebhookSignatureHeader, webhookSignature(secret, timestamp, result.Payload))
	resp, err := client.Do(req)
	if err != nil {
		return ResultDeliveryResponse{}, err
	}
	defer resp.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxWebhookResponseBodySize))
	if readErr != nil {
		return ResultDeliveryResponse{StatusCode: resp.StatusCode}, readErr
	}
	return ResultDeliveryResponse{StatusCode: resp.StatusCode}, nil
}

func webhookSignature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type WebhookConfig struct {
	Version   int32  `json:"version"`
	URL       string `json:"url"`
	TimeoutMS int32  `json:"timeout_ms"`
}

func ValidateWebhookConfig(config WebhookConfig) (*url.URL, error) {
	if config.Version != 1 {
		return nil, errors.New("version must be 1")
	}
	if config.TimeoutMS < 1000 || config.TimeoutMS > 30000 {
		return nil, errors.New("timeout_ms must be between 1000 and 30000")
	}
	u, err := url.Parse(config.URL)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("url must be an absolute HTTPS URL")
	}
	if u.User != nil || u.Fragment != "" {
		return nil, errors.New("url must not contain userinfo or a fragment")
	}
	if net.ParseIP(u.Hostname()) != nil {
		return nil, errors.New("url host must not be an IP literal")
	}
	if isSpecialHostname(u.Hostname()) {
		return nil, errors.New("url host is not allowed")
	}
	return u, nil
}

func hasResultCapability(raw []byte) bool {
	var caps map[string]any
	if json.Unmarshal(raw, &caps) != nil {
		return false
	}
	value, ok := caps[WebhookResultCapability].(bool)
	return ok && value
}

func isSpecialHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home", ".lan", ".invalid", ".test", ".example"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func resolvePublicWebhookHost(ctx context.Context, resolver WebhookResolver, host string) ([]netip.Addr, error) {
	if isSpecialHostname(host) {
		return nil, ErrUnsafeWebhookDestination
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook host: %w", err)
	}
	if len(ips) == 0 {
		return nil, errors.New("resolve webhook host: no addresses")
	}
	for _, ip := range ips {
		if !isPublicWebhookIP(ip) {
			return nil, ErrUnsafeWebhookDestination
		}
	}
	return ips, nil
}

var forbiddenWebhookPrefixes = func() []netip.Prefix {
	raw := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:2::/48", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8"}
	out := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}()

func isPublicWebhookIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range forbiddenWebhookPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

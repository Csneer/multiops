package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type errorResolver struct{}

func (errorResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, fmt.Errorf("temporary DNS failure")
}

type staticResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   int
}

func (r *staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.calls
	r.calls++
	if i >= len(r.answers) {
		i = len(r.answers) - 1
	}
	return r.answers[i], nil
}

func TestValidateWebhookConfig(t *testing.T) {
	valid := WebhookConfig{Version: 1, URL: "https://hooks.example.com/result", TimeoutMS: 5000}
	if _, err := ValidateWebhookConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []WebhookConfig{
		{Version: 2, URL: valid.URL, TimeoutMS: 5000}, {Version: 1, URL: "http://hooks.example.com", TimeoutMS: 5000},
		{Version: 1, URL: "https://user@hooks.example.com", TimeoutMS: 5000}, {Version: 1, URL: "https://hooks.example.com/#x", TimeoutMS: 5000},
		{Version: 1, URL: "https://127.0.0.1", TimeoutMS: 5000}, {Version: 1, URL: "https://localhost", TimeoutMS: 5000},
		{Version: 1, URL: valid.URL, TimeoutMS: 999}, {Version: 1, URL: valid.URL, TimeoutMS: 30001},
	} {
		if _, err := ValidateWebhookConfig(tc); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestPublicWebhookIPPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "192.0.2.1", "198.18.0.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		if isPublicWebhookIP(netip.MustParseAddr(raw)) {
			t.Fatalf("allowed %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicWebhookIP(netip.MustParseAddr(raw)) {
			t.Fatalf("rejected %s", raw)
		}
	}
	mixed := &staticResolver{answers: [][]netip.Addr{{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}}}
	if _, err := resolvePublicWebhookHost(context.Background(), mixed, "hooks.example.com"); err == nil {
		t.Fatal("mixed DNS accepted")
	}
}

func TestOversizedWebhookResponsePreservesStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusTooEarly, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(bytes.Repeat([]byte("x"), maxWebhookResponseBodySize+1024))
			}))
			defer server.Close()
			addr := server.Listener.Addr().String()
			d := &WebhookResultDeliverer{options: WebhookTransportOptions{
				Resolver: &staticResolver{answers: [][]netip.Addr{{netip.MustParseAddr("8.8.8.8")}}},
				Dialer: func(ctx context.Context, network, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, addr)
				},
				TLSConfig: &tls.Config{InsecureSkipVerify: true}, Now: time.Now,
			}}
			response, err := d.send(context.Background(), "https://hooks.example.com/result", time.Second, []byte("secret"), ResultDelivery{Payload: []byte(`{}`), IdempotencyKey: "oversized"})
			if err != nil {
				t.Fatalf("send error=%v", err)
			}
			if response.StatusCode != tc.status || retryableDelivery(response, nil) != tc.retryable {
				t.Fatalf("response=%+v retryable=%v", response, retryableDelivery(response, nil))
			}
		})
	}
}

func TestWebhookDNSFailureIsRetryable(t *testing.T) {
	d := &WebhookResultDeliverer{options: WebhookTransportOptions{Resolver: errorResolver{}, Dialer: func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("must not dial") }, Now: time.Now}}
	response, err := d.send(context.Background(), "https://hooks.example.com/result", time.Second, []byte("secret"), ResultDelivery{Payload: []byte(`{}`), IdempotencyKey: "delivery"})
	if err == nil || response.Retryable != nil {
		t.Fatalf("DNS failure should use default retryable error classification: %+v %v", response, err)
	}
}

func TestWebhookSendHMACPinnedDialRedirectAndRebinding(t *testing.T) {
	var gotBody, gotSignature, gotTimestamp, gotDelivery string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody = string(body)
		gotSignature = r.Header.Get(WebhookSignatureHeader)
		gotTimestamp = r.Header.Get(WebhookTimestampHeader)
		gotDelivery = r.Header.Get(WebhookDeliveryIDHeader)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://other.example.com/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	addr := server.Listener.Addr().String()
	resolver := &staticResolver{answers: [][]netip.Addr{{netip.MustParseAddr("8.8.8.8")}, {netip.MustParseAddr("127.0.0.1")}}}
	var dialAddress string
	d := &WebhookResultDeliverer{options: WebhookTransportOptions{Resolver: resolver, Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
		dialAddress = address
		var nd net.Dialer
		return nd.DialContext(ctx, network, addr)
	}, TLSConfig: &tls.Config{InsecureSkipVerify: true}, Now: func() time.Time { return time.Unix(1700000000, 0) }}}
	payload := []byte(`{"ok":true}`)
	delivery := ResultDelivery{Payload: payload, IdempotencyKey: "delivery-1"}
	resp, err := d.send(context.Background(), "https://hooks.example.com/result", time.Second, []byte("secret"), delivery)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("send resp=%+v err=%v", resp, err)
	}
	if !strings.HasPrefix(dialAddress, "8.8.8.8:") {
		t.Fatalf("dial not pinned: %s", dialAddress)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(gotTimestamp + "."))
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotBody != string(payload) || gotSignature != want || gotDelivery != "delivery-1" {
		t.Fatalf("body/signature/headers mismatch")
	}
	resp, err = d.send(context.Background(), "https://hooks.example.com/result", time.Second, []byte("secret"), delivery)
	if err == nil || resp.Retryable == nil || *resp.Retryable {
		t.Fatalf("rebinding must be terminal: %+v %v", resp, err)
	}

	resolver.answers = [][]netip.Addr{{netip.MustParseAddr("8.8.8.8")}}
	resolver.calls = 0
	resp, err = d.send(context.Background(), "https://hooks.example.com/redirect", time.Second, []byte("secret"), delivery)
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("redirect followed or failed: %+v %v", resp, err)
	}
}

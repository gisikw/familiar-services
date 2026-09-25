package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, path
}

func configuredService(t *testing.T, host string) (*Service, *ecdsa.PrivateKey) {
	t.Helper()
	key, path := testKey(t)
	t.Setenv("FAMILIAR_APNS_KEY_FILE", path)
	t.Setenv("FAMILIAR_APNS_KEY_ID", "KEY123")
	t.Setenv("FAMILIAR_APNS_TEAM_ID", "TEAM123")
	t.Setenv("FAMILIAR_APNS_TOPIC", "network.gisi.familiar")
	t.Setenv("FAMILIAR_APNS_HOST", host)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, key
}

func TestJWTShapeSignatureAndCache(t *testing.T) {
	s, key := configuredService(t, "https://api.push.apple.com")
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	token, err := s.bearerToken()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	decode := func(part string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var header map[string]any
	var claims map[string]any
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" || header["kid"] != "KEY123" || claims["iss"] != "TEAM123" || claims["iat"] != float64(now.Unix()) {
		t.Fatalf("unexpected JWT: header=%#v claims=%#v", header, claims)
	}
	sig := decode(parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw 64-byte r||s", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], newBigInt(sig[:32]), newBigInt(sig[32:])) {
		t.Fatal("JWT signature did not verify")
	}
	s.now = func() time.Time { return now.Add(49 * time.Minute) }
	cached, err := s.bearerToken()
	if err != nil || cached != token {
		t.Fatalf("token was not cached: err=%v", err)
	}
}

func newBigInt(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

func TestSendHTTP2HeadersAndPayload(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- r.Clone(context.Background())
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	s, _ := configuredService(t, server.URL)
	s.client = server.Client()
	device := strings.Repeat("ab", 32)
	if _, err := s.Handle(context.Background(), "push.register", map[string]any{"token": device, "platform": "ios"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Handle(context.Background(), "push.send", map[string]any{"title": "Hello", "body": "Kevin", "threadId": "familiar"})
	if err != nil {
		t.Fatal(err)
	}
	counts := result.(map[string]any)
	if counts["sent"] != 1 || counts["failed"] != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	r := <-requests
	if r.ProtoMajor != 2 || r.Method != http.MethodPost || r.URL.Path != "/3/device/"+device {
		t.Fatalf("unexpected request: %s %s %s", r.Proto, r.Method, r.URL.Path)
	}
	if r.Header.Get("apns-topic") != "network.gisi.familiar" || r.Header.Get("apns-push-type") != "alert" || r.Header.Get("apns-priority") != "10" || !strings.HasPrefix(r.Header.Get("authorization"), "bearer ") {
		t.Fatalf("unexpected headers: %#v", r.Header)
	}
	var got map[string]any
	if err := json.Unmarshal(<-bodies, &got); err != nil {
		t.Fatal(err)
	}
	aps := got["aps"].(map[string]any)
	alert := aps["alert"].(map[string]any)
	if alert["title"] != "Hello" || alert["body"] != "Kevin" || aps["sound"] != "default" || aps["thread-id"] != "familiar" {
		t.Fatalf("unexpected payload: %#v", got)
	}
}

func TestGoneRemovesDeviceAndRegisterIsIdempotent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
	}))
	defer server.Close()
	s, _ := configuredService(t, server.URL)
	s.client = server.Client()
	device := strings.Repeat("cd", 32)
	s.now = func() time.Time { return time.UnixMilli(100) }
	for _, label := range []string{"old", "Kevin's iPhone"} {
		s.now = func() time.Time { return time.UnixMilli(100 + int64(len(label))) }
		if _, err := s.Handle(context.Background(), "push.register", map[string]any{"token": device, "platform": "ios", "label": label}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var label string
	var createdAt, lastSeen int64
	if err := s.db.QueryRow(`SELECT count(*),max(label),min(created_at),max(last_seen) FROM push_devices`).Scan(&count, &label, &createdAt, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if count != 1 || label != "Kevin's iPhone" || createdAt != 103 || lastSeen != 114 {
		t.Fatalf("idempotent register: count=%d label=%q created=%d lastSeen=%d", count, label, createdAt, lastSeen)
	}
	result, err := s.Handle(context.Background(), "push.send", map[string]any{"body": "test", "sound": false})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["failed"] != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM push_devices`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("410 left %d devices", count)
	}
}

func TestOperationsUnavailableWithoutConfiguration(t *testing.T) {
	for _, name := range []string{"FAMILIAR_APNS_KEY_FILE", "FAMILIAR_APNS_KEY_ID", "FAMILIAR_APNS_TEAM_ID", "FAMILIAR_APNS_TOPIC", "FAMILIAR_APNS_HOST"} {
		t.Setenv(name, "")
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("configuration must not prevent startup: %v", err)
	}
	defer s.Close()
	for _, op := range []string{"push.register", "push.send"} {
		_, err := s.Handle(context.Background(), op, map[string]any{})
		var codedErr *CodedError
		if !errors.As(err, &codedErr) || codedErr.Code != "unavailable" {
			t.Fatalf("%s error = %#v, want unavailable", op, err)
		}
	}
}

// Package push stores iOS devices and sends Apple Push Notification service messages.
package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const defaultHost = "https://api.push.apple.com"

// CodedError is an error suitable for the service wire protocol.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }

func coded(code, message string) error { return &CodedError{Code: code, Err: errors.New(message)} }

type config struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	topic  string
	host   string
}

// Service owns the durable device registry and APNs client.
type Service struct {
	db        *sql.DB
	config    *config
	configErr error
	client    *http.Client
	now       func() time.Time

	jwtMu     sync.Mutex
	jwt       string
	jwtIssued time.Time
}

// Open opens the push registry in the service state directory. APNs
// configuration errors are retained and returned by push operations, rather
// than preventing the rest of familiar-services from starting.
func Open(stateDir string) (*Service, error) {
	if stateDir == "" {
		return nil, errors.New("state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(stateDir, "scheduler.sqlite")+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS push_devices (
		token TEXT PRIMARY KEY,
		platform TEXT NOT NULL CHECK(platform = 'ios'),
		label TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	cfg, cfgErr := configFromEnv()
	return &Service{
		db: db, config: cfg, configErr: cfgErr, now: time.Now,
		client: &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true}, Timeout: 30 * time.Second},
	}, nil
}

func (s *Service) Close() error { return s.db.Close() }

func configFromEnv() (*config, error) {
	keyFile := os.Getenv("FAMILIAR_APNS_KEY_FILE")
	keyID := os.Getenv("FAMILIAR_APNS_KEY_ID")
	teamID := os.Getenv("FAMILIAR_APNS_TEAM_ID")
	topic := os.Getenv("FAMILIAR_APNS_TOPIC")
	if keyFile == "" || keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("APNs configuration is incomplete")
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read APNs key: %w", err)
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("APNs key is not a PEM-encoded PKCS#8 key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse APNs PKCS#8 key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("APNs key must be an EC P-256 private key")
	}
	host := os.Getenv("FAMILIAR_APNS_HOST")
	if host == "" {
		host = defaultHost
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("FAMILIAR_APNS_HOST must be an HTTPS URL")
	}
	return &config{key: key, keyID: keyID, teamID: teamID, topic: topic, host: strings.TrimRight(host, "/")}, nil
}

func (s *Service) available() error {
	if s == nil || s.config == nil {
		message := "APNs is unavailable"
		if s != nil && s.configErr != nil {
			message += ": " + s.configErr.Error()
		}
		return coded("unavailable", message)
	}
	return nil
}

// Handle applies a push.register or push.send operation.
func (s *Service) Handle(ctx context.Context, op string, args map[string]any) (any, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	switch op {
	case "push.register":
		return s.register(args)
	case "push.send":
		return s.send(ctx, args)
	default:
		return nil, coded("invalid_request", "unknown push operation")
	}
}

func only(args map[string]any, names ...string) bool {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	for name := range args {
		if !allowed[name] {
			return false
		}
	}
	return true
}

func (s *Service) register(args map[string]any) (any, error) {
	if !only(args, "token", "platform", "label") {
		return nil, coded("invalid_request", "invalid arguments")
	}
	token, tokenOK := args["token"].(string)
	platform, platformOK := args["platform"].(string)
	var label *string
	if value, exists := args["label"]; exists {
		x, ok := value.(string)
		if !ok || len(x) > 200 || strings.ContainsAny(x, "\x00\n\r") {
			return nil, coded("invalid_request", "invalid label")
		}
		label = &x
	}
	if !tokenOK || len(token) < 64 || len(token) > 200 || len(token)%2 != 0 {
		return nil, coded("invalid_request", "token must be a 64..200 character hex string")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return nil, coded("invalid_request", "token must be a 64..200 character hex string")
	}
	if !platformOK || platform != "ios" {
		return nil, coded("invalid_request", "platform must be ios")
	}
	token = strings.ToLower(token)
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO push_devices(token,platform,label,created_at,last_seen)
		VALUES(?,?,COALESCE(?,''),?,?) ON CONFLICT(token) DO UPDATE SET
		platform=excluded.platform,label=COALESCE(?,push_devices.label),last_seen=excluded.last_seen`, token, platform, label, now, now, label)
	if err != nil {
		return nil, err
	}
	return map[string]any{"registered": true}, nil
}

type sendInput struct {
	Title    string
	Body     string
	ThreadID string
	Sound    bool
}

func parseSend(args map[string]any) (sendInput, error) {
	if !only(args, "title", "body", "threadId", "sound") {
		return sendInput{}, coded("invalid_request", "invalid arguments")
	}
	var in sendInput
	in.Sound = true
	body, ok := args["body"].(string)
	if !ok || body == "" || len(body) > 1024 {
		return in, coded("invalid_request", "body is required and must be at most 1024 characters")
	}
	in.Body = body
	for name, destination := range map[string]*string{"title": &in.Title, "threadId": &in.ThreadID} {
		if value, exists := args[name]; exists {
			x, ok := value.(string)
			if !ok {
				return in, coded("invalid_request", "invalid "+name)
			}
			*destination = x
		}
	}
	if value, exists := args["sound"]; exists {
		var ok bool
		in.Sound, ok = value.(bool)
		if !ok {
			return in, coded("invalid_request", "invalid sound")
		}
	}
	return in, nil
}

type payload struct {
	APS aps `json:"aps"`
}
type aps struct {
	Alert    alert  `json:"alert"`
	Sound    string `json:"sound,omitempty"`
	ThreadID string `json:"thread-id,omitempty"`
}
type alert struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

func (s *Service) send(ctx context.Context, args map[string]any) (any, error) {
	in, err := parseSend(args)
	if err != nil {
		return nil, err
	}
	token, err := s.bearerToken()
	if err != nil {
		return nil, coded("unavailable", "create APNs authorization token: "+err.Error())
	}
	rows, err := s.db.Query(`SELECT token FROM push_devices ORDER BY token`)
	if err != nil {
		return nil, err
	}
	var devices []string
	for rows.Next() {
		var device string
		if err := rows.Scan(&device); err != nil {
			rows.Close()
			return nil, err
		}
		devices = append(devices, device)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	message := payload{APS: aps{Alert: alert{Title: in.Title, Body: in.Body}, ThreadID: in.ThreadID}}
	if in.Sound {
		message.APS.Sound = "default"
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	sent, failed := 0, 0
	for _, device := range devices {
		status, reason, requestErr := s.post(ctx, device, token, body)
		suffix := device
		if len(suffix) > 6 {
			suffix = suffix[len(suffix)-6:]
		}
		if requestErr != nil {
			failed++
			log.Printf("APNs send device=…%s failed: %v", suffix, requestErr)
			continue
		}
		if status >= 200 && status < 300 {
			sent++
			log.Printf("APNs send device=…%s status=%d", suffix, status)
			continue
		}
		failed++
		log.Printf("APNs send device=…%s status=%d reason=%s", suffix, status, reason)
		if status == http.StatusGone || (status == http.StatusBadRequest && (reason == "BadDeviceToken" || reason == "Unregistered")) {
			if _, deleteErr := s.db.Exec(`DELETE FROM push_devices WHERE token=?`, device); deleteErr != nil {
				log.Printf("APNs remove device=…%s: %v", suffix, deleteErr)
			}
		}
	}
	return map[string]any{"sent": sent, "failed": failed}, nil
}

func (s *Service) post(ctx context.Context, device, token string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.host+"/3/device/"+device, strings.NewReader(string(body)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("apns-topic", s.config.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("authorization", "bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return resp.StatusCode, "", err
	}
	var apnsError struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(responseBody, &apnsError)
	return resp.StatusCode, apnsError.Reason, nil
}

func (s *Service) bearerToken() (string, error) {
	s.jwtMu.Lock()
	defer s.jwtMu.Unlock()
	now := s.now()
	if s.jwt != "" && now.Sub(s.jwtIssued) < 50*time.Minute && now.Sub(s.jwtIssued) >= 0 {
		return s.jwt, nil
	}
	encode := base64.RawURLEncoding.EncodeToString
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}{Alg: "ES256", Kid: s.config.keyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
	}{Iss: s.config.teamID, Iat: now.Unix()})
	if err != nil {
		return "", err
	}
	signingInput := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(rand.Reader, s.config.key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	s.jwt = signingInput + "." + encode(signature)
	s.jwtIssued = now
	return s.jwt, nil
}

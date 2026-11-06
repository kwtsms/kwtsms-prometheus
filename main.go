// kwtsms-prometheus receives Prometheus Alertmanager webhooks and sends each alert group as one SMS through kwtSMS.
//
// Settings come from the environment; see README.md.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	kwtsms "github.com/kwtsms/kwtsms-go"
)

const version = "1.0.0"

// Config is read once at start.
type Config struct {
	Username, Password, Sender string
	TestMode                   bool
	AuthToken                  string
	ListenAddr                 string
	MaxPages                   int
	MaxNumbers                 int
	MaxPerMinute               int
}

// Payload is the part of the Alertmanager webhook (version 4) that is used.
type Payload struct {
	Status            string            `json:"status"`
	GroupKey          string            `json:"groupKey"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	Alerts            []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// Sender is the part of the kwtSMS client used here (an interface so tests can replace it).
type Sender interface {
	SendMulti(mobiles []string, message string, sender string) (*kwtsms.SendResult, error)
}

// Server handles webhook calls.
type Server struct {
	cfg    Config
	client Sender
	now    func() time.Time

	mu     sync.Mutex
	seen   map[string]time.Time // dedupe key -> first send, kept 5 minutes
	window time.Time            // start of the current rate-limit minute
	count  int                  // SMS sent in the current minute
}

// transient kwtSMS answers are retried by Alertmanager (HTTP 500); all other errors are permanent (HTTP 400).
var transient = map[string]bool{"NETWORK": true, "ERR013": true, "ERR028": true}

func env(key, def string) string {
	if v := os.Getenv(key + "_FILE"); v != "" {
		b, err := os.ReadFile(v)
		if err != nil {
			log.Fatalf("cannot read %s_FILE: %v", key, err)
		}
		return strings.TrimSpace(string(b))
	}
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(env(key, strconv.Itoa(def)))
	if err != nil || n < 1 {
		log.Fatalf("%s must be a positive number", key)
	}
	return n
}

// LoadConfig reads and checks the settings; it refuses to start without credentials or an auth token.
func LoadConfig() Config {
	c := Config{
		Username:     env("KWTSMS_USERNAME", ""),
		Password:     env("KWTSMS_PASSWORD", ""),
		Sender:       env("KWTSMS_SENDER", "KWT-SMS"),
		TestMode:     env("KWTSMS_TEST_MODE", "0") == "1",
		AuthToken:    env("AUTH_TOKEN", ""),
		ListenAddr:   env("LISTEN_ADDR", ":9097"),
		MaxPages:     envInt("MAX_PAGES", 2),
		MaxNumbers:   envInt("MAX_NUMBERS", 10),
		MaxPerMinute: envInt("MAX_SMS_PER_MINUTE", 30),
	}
	if c.Username == "" || c.Password == "" {
		log.Fatal("KWTSMS_USERNAME and KWTSMS_PASSWORD (or KWTSMS_PASSWORD_FILE) are required")
	}
	if len(c.AuthToken) < 16 {
		log.Fatal("AUTH_TOKEN (or AUTH_TOKEN_FILE) of at least 16 characters is required, so nobody else can spend your credits")
	}
	return c
}

// BuildMessage turns one alert group into SMS text: status line, then summaries, cut to maxPages SMS pages.
func BuildMessage(p Payload, maxPages int) string {
	firing := 0
	for _, a := range p.Alerts {
		if a.Status == "firing" {
			firing++
		}
	}
	head := "[RESOLVED]"
	if p.Status == "firing" {
		head = fmt.Sprintf("[FIRING:%d]", firing)
	}
	name := p.CommonLabels["alertname"]
	if sev := p.CommonLabels["severity"]; sev != "" {
		name += " " + sev
	}
	lines := []string{strings.TrimSpace(head + " " + name)}

	if s := firstNonEmpty(p.CommonAnnotations["summary"], p.CommonAnnotations["description"]); s != "" {
		lines = append(lines, s)
	} else {
		for _, a := range p.Alerts {
			s := firstNonEmpty(a.Annotations["summary"], a.Annotations["description"], a.Labels["instance"])
			if s != "" {
				lines = append(lines, s)
			}
		}
	}
	return Truncate(strings.Join(lines, "\n"), maxPages)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// Truncate cuts text to maxPages SMS pages (153 GSM characters or 67 Unicode characters per page, 160 or 70 for
// one page), on a rune boundary, ending with "...".
func Truncate(text string, maxPages int) string {
	unicode := false
	for _, r := range text {
		if r > 0x7E {
			unicode = true
			break
		}
	}
	single, multi := 160, 153
	if unicode {
		single, multi = 70, 67
	}
	limit := single
	if maxPages > 1 {
		limit = multi * maxPages
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	r := []rune(text)
	return strings.TrimRight(string(r[:limit-3]), " \n") + "..."
}

// dedupeKey identifies one notification: Alertmanager sends the same group, status and alerts again on a retry.
func dedupeKey(p Payload, to string) string {
	fps := make([]string, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		fps = append(fps, a.Fingerprint+a.Status)
	}
	sort.Strings(fps)
	h := sha256.Sum256([]byte(p.GroupKey + "|" + p.Status + "|" + strings.Join(fps, ",") + "|" + to))
	return hex.EncodeToString(h[:])
}

func (s *Server) authorized(r *http.Request) bool {
	want := "Bearer " + s.cfg.AuthToken
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// reserve records a send for dedupe and the per-minute cap. It returns "dup", "limit" or "".
func (s *Server) reserve(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, t := range s.seen {
		if now.Sub(t) > 5*time.Minute {
			delete(s.seen, k)
		}
	}
	if _, ok := s.seen[key]; ok {
		return "dup"
	}
	if now.Sub(s.window) >= time.Minute {
		s.window, s.count = now, 0
	}
	if s.count >= s.cfg.MaxPerMinute {
		return "limit"
	}
	s.count++
	s.seen[key] = now
	return ""
}

func (s *Server) release(key string) {
	s.mu.Lock()
	delete(s.seen, key)
	s.mu.Unlock()
}

// ServeHTTP handles POST /alert?to=965XXXXXXXX,965XXXXXXXX.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		fmt.Fprintln(w, "ok")
		return
	}
	if r.URL.Path != "/alert" || r.Method != http.MethodPost {
		http.Error(w, "use POST /alert?to=...", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "missing or wrong Authorization: Bearer token", http.StatusUnauthorized)
		return
	}
	var numbers []string
	for _, n := range strings.Split(r.URL.Query().Get("to"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			numbers = append(numbers, n)
		}
	}
	if len(numbers) == 0 || len(numbers) > s.cfg.MaxNumbers {
		http.Error(w, fmt.Sprintf("to= needs 1 to %d phone numbers", s.cfg.MaxNumbers), http.StatusBadRequest)
		return
	}
	var p Payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil || len(p.Alerts) == 0 {
		http.Error(w, "body is not an Alertmanager webhook", http.StatusBadRequest)
		return
	}

	key := dedupeKey(p, strings.Join(numbers, ","))
	switch s.reserve(key) {
	case "dup":
		log.Printf("skip duplicate group=%q status=%s", p.GroupKey, p.Status)
		fmt.Fprintln(w, "duplicate, already sent")
		return
	case "limit":
		log.Printf("rate limit: %d SMS per minute reached, group=%q not sent", s.cfg.MaxPerMinute, p.GroupKey)
		http.Error(w, "MAX_SMS_PER_MINUTE reached", http.StatusTooManyRequests)
		return
	}

	res, err := s.client.SendMulti(numbers, BuildMessage(p, s.cfg.MaxPages), s.cfg.Sender)
	if err == nil && res != nil && res.Result == "OK" {
		log.Printf("sent status=%s numbers=%d points=%d msg-id=%s balance=%v test=%v",
			p.Status, res.Numbers, res.PointsCharged, res.MsgID, res.BalanceAfter, s.cfg.TestMode)
		fmt.Fprintln(w, "sent")
		return
	}
	s.release(key)
	code, desc := "ERROR", ""
	if err != nil {
		code, desc = "NETWORK", err.Error()
	} else if res != nil {
		code, desc = res.Code, res.Description
	}
	log.Printf("kwtSMS %s: %s group=%q", code, desc, p.GroupKey)
	status := http.StatusBadRequest
	if transient[code] {
		status = http.StatusInternalServerError
	}
	http.Error(w, "kwtSMS "+code+": "+desc, status)
}

func main() {
	cfg := LoadConfig()
	client, err := kwtsms.New(cfg.Username, cfg.Password,
		kwtsms.WithSenderID(cfg.Sender), kwtsms.WithTestMode(cfg.TestMode), kwtsms.WithLogFile(""),
		// 2026-11-06 20:20 +03: renamed from kwtsms-alertmanager (Boss: the name people search for is Prometheus).
		kwtsms.WithAppName("kwtsms-prometheus/"+version),
		kwtsms.WithHTTPClient(&http.Client{Timeout: 8 * time.Second}))
	if err != nil {
		log.Fatal(err)
	}
	s := &Server{cfg: cfg, client: client, now: time.Now, seen: map[string]time.Time{}}
	log.Printf("kwtsms-prometheus %s listening on %s (test mode %v)", version, cfg.ListenAddr, cfg.TestMode)
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

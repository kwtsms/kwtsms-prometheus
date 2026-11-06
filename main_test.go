package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kwtsms "github.com/kwtsms/kwtsms-go"
)

// Numbers use country code 999 (unassigned): nobody can receive them, and the client is a fake.
type fakeSender struct {
	calls  [][]string
	msgs   []string
	answer *kwtsms.SendResult
}

func (f *fakeSender) SendMulti(m []string, msg, sender string) (*kwtsms.SendResult, error) {
	f.calls = append(f.calls, m)
	f.msgs = append(f.msgs, msg)
	return f.answer, nil
}

const token = "test-token-0123456789"

const body = `{"version":"4","status":"firing","groupKey":"{}:{alertname=\"HighCPU\"}",
"commonLabels":{"alertname":"HighCPU","severity":"critical"},
"commonAnnotations":{"summary":"CPU over 90% on web01"},
"alerts":[{"status":"firing","labels":{"instance":"web01"},"fingerprint":"a1"},
          {"status":"firing","labels":{"instance":"web02"},"fingerprint":"a2"}]}`

func newServer(answer *kwtsms.SendResult) (*Server, *fakeSender, *time.Time) {
	if answer == nil {
		answer = &kwtsms.SendResult{Result: "OK", Numbers: 1, PointsCharged: 1, MsgID: "m1"}
	}
	f := &fakeSender{answer: answer}
	now := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	s := &Server{cfg: Config{AuthToken: token, Sender: "KWT-SMS", MaxPages: 2, MaxNumbers: 10, MaxPerMinute: 30},
		client: f, seen: map[string]time.Time{}}
	s.now = func() time.Time { return now }
	return s, f, &now
}

func post(s *Server, url, auth, b string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(b))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestSendsOneSMSPerGroup(t *testing.T) {
	s, f, _ := newServer(nil)
	w := post(s, "/alert?to=99912345678,%2B999%201234%205679", "Bearer "+token, body)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if len(f.calls) != 1 || len(f.calls[0]) != 2 {
		t.Fatalf("calls %v", f.calls)
	}
	want := "[FIRING:2] HighCPU critical\nCPU over 90% on web01"
	if f.msgs[0] != want {
		t.Fatalf("message %q", f.msgs[0])
	}
}

func TestAuthRequired(t *testing.T) {
	s, f, _ := newServer(nil)
	if w := post(s, "/alert?to=99912345678", "", body); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := post(s, "/alert?to=99912345678", "Bearer wrong", body); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if len(f.calls) != 0 {
		t.Fatal("sent without auth")
	}
}

func TestBadRequests(t *testing.T) {
	s, f, _ := newServer(nil)
	many := strings.Repeat("99912345678,", 11)
	for name, url := range map[string]string{"no to": "/alert", "too many": "/alert?to=" + many} {
		if w := post(s, url, "Bearer "+token, body); w.Code != 400 {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, "not json"); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
	if len(f.calls) != 0 {
		t.Fatal("sent on a bad request")
	}
}

func TestRetryOfSameGroupIsNotSentTwice(t *testing.T) {
	s, f, now := newServer(nil)
	post(s, "/alert?to=99912345678", "Bearer "+token, body)
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, body); w.Code != 200 {
		t.Fatalf("dup code %d", w.Code)
	}
	if len(f.calls) != 1 {
		t.Fatalf("sent %d times", len(f.calls))
	}
	*now = now.Add(6 * time.Minute)
	post(s, "/alert?to=99912345678", "Bearer "+token, body)
	if len(f.calls) != 2 {
		t.Fatal("not sent again after the dedupe window")
	}
}

func TestResolvedIsADifferentNotification(t *testing.T) {
	s, f, _ := newServer(nil)
	post(s, "/alert?to=99912345678", "Bearer "+token, body)
	resolved := strings.ReplaceAll(body, `"firing"`, `"resolved"`)
	post(s, "/alert?to=99912345678", "Bearer "+token, resolved)
	if len(f.calls) != 2 || !strings.HasPrefix(f.msgs[1], "[RESOLVED] HighCPU") {
		t.Fatalf("calls %d msg %q", len(f.calls), f.msgs)
	}
}

func TestPerMinuteCap(t *testing.T) {
	s, f, now := newServer(nil)
	s.cfg.MaxPerMinute = 2
	for i, k := range []string{"g1", "g2", "g3"} {
		w := post(s, "/alert?to=99912345678", "Bearer "+token, strings.Replace(body, "HighCPU\\\"}", k+"\\\"}", 1))
		if i < 2 && w.Code != 200 || i == 2 && w.Code != 429 {
			t.Fatalf("send %d: %d", i, w.Code)
		}
	}
	*now = now.Add(61 * time.Second)
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, strings.Replace(body, "HighCPU\\\"}", "g3\\\"}", 1)); w.Code != 200 {
		t.Fatalf("after a minute: %d", w.Code)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls %d", len(f.calls))
	}
}

func TestPermanentErrorIs400TransientIs500AndRetryAllowed(t *testing.T) {
	s, f, _ := newServer(&kwtsms.SendResult{Result: "ERROR", Code: "ERR003", Description: "auth"})
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, body); w.Code != 400 || !strings.Contains(w.Body.String(), "ERR003") {
		t.Fatalf("ERR003: %d %s", w.Code, w.Body)
	}
	f.answer = &kwtsms.SendResult{Result: "ERROR", Code: "ERR013", Description: "queue full"}
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, body); w.Code != 500 {
		t.Fatalf("ERR013: %d", w.Code)
	}
	f.answer = &kwtsms.SendResult{Result: "OK"}
	if w := post(s, "/alert?to=99912345678", "Bearer "+token, body); w.Code != 200 {
		t.Fatalf("retry after failure: %d", w.Code)
	}
	if len(f.calls) != 3 {
		t.Fatalf("a failed send must not be marked as sent: %d calls", len(f.calls))
	}
}

func TestFallsBackToPerAlertSummaries(t *testing.T) {
	p := Payload{Status: "firing", CommonLabels: map[string]string{"alertname": "DiskFull"}}
	p.Alerts = append(p.Alerts, struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Fingerprint string            `json:"fingerprint"`
	}{Status: "firing", Labels: map[string]string{"instance": "db01"}})
	if got := BuildMessage(p, 2); got != "[FIRING:1] DiskFull\ndb01" {
		t.Fatalf("%q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate(strings.Repeat("a", 400), 2); len(got) != 306 || !strings.HasSuffix(got, "...") {
		t.Fatalf("english %d", len(got))
	}
	ar := Truncate(strings.Repeat("ب", 400), 2)
	if n := len([]rune(ar)); n != 134 {
		t.Fatalf("arabic runes %d", n)
	}
	if got := Truncate(strings.Repeat("a", 200), 1); len([]rune(got)) != 160 {
		t.Fatalf("one page %d", len(got))
	}
	if got := Truncate("short", 2); got != "short" {
		t.Fatal(got)
	}
}

func TestHealthz(t *testing.T) {
	s, _, _ := newServer(nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

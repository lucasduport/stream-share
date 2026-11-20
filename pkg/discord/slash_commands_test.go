package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type recordedReq struct{ method, path, body string }

// fakeDiscord returns a bot whose REST calls hit a test server; every request is recorded.
// Paths listed in fail get a 500.
func fakeDiscord(t *testing.T, guilds []string, fail ...string) (*Bot, func() []recordedReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedReq{r.Method, r.URL.Path, string(body)})
		mu.Unlock()
		for _, f := range fail {
			if strings.HasSuffix(r.URL.Path, f) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(body) == 0 || string(body) == "[]" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write(body) // echo the commands back
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)

	s, _ := discordgo.New("Bot test")
	s.MaxRestRetries = 0
	s.Client = &http.Client{Transport: rewriteTransport{target}}
	s.State.User = &discordgo.User{ID: "app1"}
	for _, id := range guilds {
		s.State.Guilds = append(s.State.Guilds, &discordgo.Guild{ID: id})
	}
	return &Bot{session: s}, func() []recordedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedReq(nil), reqs...)
	}
}

type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func countCmds(t *testing.T, body string) int {
	t.Helper()
	var cmds []json.RawMessage
	if err := json.Unmarshal([]byte(body), &cmds); err != nil {
		t.Fatal(err)
	}
	return len(cmds)
}

func TestRegisterSlashCommands_OneGuildClearsGlobal(t *testing.T) {
	b, got := fakeDiscord(t, []string{"g1"})
	if err := b.registerSlashCommands(); err != nil {
		t.Fatal(err)
	}
	reqs := got()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %+v", reqs)
	}
	if reqs[0].method != "PUT" || !strings.HasSuffix(reqs[0].path, "/applications/app1/guilds/g1/commands") {
		t.Fatalf("first request: %+v", reqs[0])
	}
	if n := countCmds(t, reqs[0].body); n != 11 {
		t.Fatalf("guild body has %d commands, want 11", n)
	}
	if reqs[1].method != "PUT" || !strings.HasSuffix(reqs[1].path, "/applications/app1/commands") || strings.Contains(reqs[1].path, "guilds") {
		t.Fatalf("second request: %+v", reqs[1])
	}
	if reqs[1].body != "[]" {
		t.Fatalf("global body = %q, want []", reqs[1].body)
	}
}

func TestRegisterSlashCommands_ManyGuildsKeepsGlobal(t *testing.T) {
	b, got := fakeDiscord(t, []string{"g1", "g2"})
	if err := b.registerSlashCommands(); err != nil {
		t.Fatal(err)
	}
	reqs := got()
	if len(reqs) != 1 || !strings.HasSuffix(reqs[0].path, "/applications/app1/commands") {
		t.Fatalf("want one global PUT, got %+v", reqs)
	}
	if n := countCmds(t, reqs[0].body); n != 11 {
		t.Fatalf("global body has %d commands, want 11", n)
	}
}

func TestRegisterSlashCommands_ClearFailureIsNotFatal(t *testing.T) {
	b, got := fakeDiscord(t, []string{"g1"}, "/applications/app1/commands")
	if err := b.registerSlashCommands(); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if n := len(got()); n != 2 {
		t.Fatalf("want 2 requests, got %d", n)
	}
}
